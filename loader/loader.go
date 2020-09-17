package loader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/tinygo-org/tinygo/cgo"
	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/goenv"
)

// Program holds all packages and some metadata about the program as a whole.
type Program struct {
	config       *compileopts.Config
	clangHeaders string
	typeChecker  types.Config
	goroot       string // synthetic GOROOT
	workingDir   string

	Packages map[string]*Package
	sorted   []*Package
	fset     *token.FileSet

	// Information obtained during parsing.
	LDFlags []string
}

// PackageJSON is a subset of the JSON struct returned from `go list`.
type PackageJSON struct {
	Dir        string
	ImportPath string
	ForTest    string

	// Source files
	GoFiles  []string
	CgoFiles []string
	CFiles   []string

	// Dependency information
	Imports   []string
	ImportMap map[string]string

	// Error information
	Error *struct {
		ImportStack []string
		Pos         string
		Err         string
	}
}

// Package holds a loaded package, its imports, and its parsed files.
type Package struct {
	PackageJSON

	program *Program
	Files   []*ast.File
	Pkg     *types.Package
	info    types.Info
}

// Load loads the given package with all dependencies (including the runtime
// package). Call .Parse() afterwards to parse all Go files (including CGo
// processing, if necessary).
func Load(config *compileopts.Config, inputPkgs []string, clangHeaders string, typeChecker types.Config) (*Program, error) {
	goroot, err := GetCachedGoroot(config)
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	p := &Program{
		config:       config,
		clangHeaders: clangHeaders,
		typeChecker:  typeChecker,
		goroot:       goroot,
		workingDir:   wd,
		Packages:     make(map[string]*Package),
		fset:         token.NewFileSet(),
	}

	// List the dependencies of this package, in raw JSON format.
	extraArgs := []string{"-json", "-deps"}
	if config.TestConfig.CompileTestBinary {
		extraArgs = append(extraArgs, "-test")
	}
	cmd, err := List(config, extraArgs, inputPkgs)
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(status.ExitStatus())
			}
			os.Exit(1)
		}
		return nil, fmt.Errorf("failed to run `go list`: %s", err)
	}

	// Parse the returned json from `go list`.
	decoder := json.NewDecoder(buf)
	var testImportPath string
	for {
		pkg := &Package{
			program: p,
			info: types.Info{
				Types:      make(map[ast.Expr]types.TypeAndValue),
				Defs:       make(map[*ast.Ident]types.Object),
				Uses:       make(map[*ast.Ident]types.Object),
				Implicits:  make(map[ast.Node]types.Object),
				Scopes:     make(map[ast.Node]*types.Scope),
				Selections: make(map[*ast.SelectorExpr]*types.Selection),
			},
		}
		err := decoder.Decode(&pkg.PackageJSON)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if pkg.Error != nil {
			// There was an error while importing (for example, a circular
			// dependency).
			pos := token.Position{}
			fields := strings.Split(pkg.Error.Pos, ":")
			if len(fields) >= 2 {
				// There is some file/line/column information.
				if n, err := strconv.Atoi(fields[len(fields)-2]); err == nil {
					// Format: filename.go:line:colum
					pos.Filename = strings.Join(fields[:len(fields)-2], ":")
					pos.Line = n
					pos.Column, _ = strconv.Atoi(fields[len(fields)-1])
				} else {
					// Format: filename.go:line
					pos.Filename = strings.Join(fields[:len(fields)-1], ":")
					pos.Line, _ = strconv.Atoi(fields[len(fields)-1])
				}
				pos.Filename = p.getOriginalPath(pos.Filename)
			}
			err := scanner.Error{
				Pos: pos,
				Msg: pkg.Error.Err,
			}
			if len(pkg.Error.ImportStack) != 0 {
				return nil, Error{
					ImportStack: pkg.Error.ImportStack,
					Err:         err,
				}
			}
			return nil, err
		}
		if config.TestConfig.CompileTestBinary {
			// When creating a test binary, `go list` will list up to 4 packages
			// used for testing:
			// * The original package, unmodified (no *_test.go files).
			// * The to be tested package, with *_test.go files included. This
			//   package has a different import path (such as "math [math.test]").
			// * The _test packages (such as math_test) that can only access the
			//   external API. It is given an import path such as
			//   "math_test [math.test]".
			// * The main package that actually calls all the test functions,
			//   which is generated on demand.
			// The second package replaces the first when building a test
			// package. Unfortunately, //go:linkname pragmas aren't adjusted for
			// the new import path (and thus link name). Additionally, the first
			// package is useless in the build and only makes the build slower.
			// Therefore, if we detect the second package remove the first and
			// adjust the import path as if it is the first.
			if pkg.ForTest != "" && pkg.ImportPath == pkg.ForTest+" ["+pkg.ForTest+".test]" {
				// There can only be one package import path that is being
				// tested.
				if testImportPath != "" {
					return nil, fmt.Errorf("found two test import paths: %#v and %#v", testImportPath, pkg.ForTest)
				}
				testImportPath = pkg.ForTest
				// Delete the previous package (that this package overrides).
				delete(p.Packages, testImportPath)
				for i, pkg := range p.sorted {
					if pkg.ImportPath == testImportPath {
						p.sorted = append(p.sorted[:i], p.sorted[i+1:]...)
						break
					}
				}
				pkg.ImportPath = pkg.ForTest
			}
			if testImportPath != "" {
				// Do not replace the import path, the new one has been replaced
				// with the old one (see above).
				if _, ok := pkg.ImportMap[testImportPath]; ok {
					delete(pkg.ImportMap, testImportPath)
				}
			}
		}
		p.sorted = append(p.sorted, pkg)
		p.Packages[pkg.ImportPath] = pkg
	}

	return p, nil
}

// getOriginalPath looks whether this path is in the generated GOROOT and if so,
// replaces the path with the original path (in GOROOT or TINYGOROOT). Otherwise
// the input path is returned.
func (p *Program) getOriginalPath(path string) string {
	originalPath := path
	if strings.HasPrefix(path, p.goroot+string(filepath.Separator)) {
		// If this file is part of the synthetic GOROOT, try to infer the
		// original path.
		relpath := path[len(filepath.Join(p.goroot, "src"))+1:]
		realgorootPath := filepath.Join(goenv.Get("GOROOT"), "src", relpath)
		if _, err := os.Stat(realgorootPath); err == nil {
			originalPath = realgorootPath
		}
		maybeInTinyGoRoot := false
		for prefix := range pathsToOverride(needsSyscallPackage(p.config.BuildTags())) {
			if !strings.HasPrefix(relpath, prefix) {
				continue
			}
			maybeInTinyGoRoot = true
		}
		if maybeInTinyGoRoot {
			tinygoPath := filepath.Join(goenv.Get("TINYGOROOT"), "src", relpath)
			if _, err := os.Stat(tinygoPath); err == nil {
				originalPath = tinygoPath
			}
		}
	}
	return originalPath
}

// Sorted returns a list of all packages, sorted in a way that no packages come
// before the packages they depend upon.
func (p *Program) Sorted() []*Package {
	return p.sorted
}

// MainPkg returns the last package in the Sorted() slice. This is the main
// package of the program.
func (p *Program) MainPkg() *Package {
	return p.sorted[len(p.sorted)-1]
}

// Parse parses all packages and typechecks them.
//
// The returned error may be an Errors error, which contains a list of errors.
//
// Idempotent.
func (p *Program) Parse() error {
	// Parse all packages.
	// TODO: do this in parallel.
	for _, pkg := range p.sorted {
		err := pkg.Parse()
		if err != nil {
			return err
		}
	}

	// Typecheck all packages.
	for _, pkg := range p.sorted {
		err := pkg.Check()
		if err != nil {
			return err
		}
	}

	return nil
}

// parseFile is a wrapper around parser.ParseFile.
func (p *Program) parseFile(path string, mode parser.Mode) (*ast.File, error) {
	if p.fset == nil {
		p.fset = token.NewFileSet()
	}

	rd, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return parser.ParseFile(p.fset, p.getOriginalPath(path), rd, mode)
}

// Parse parses and typechecks this package.
//
// Idempotent.
func (p *Package) Parse() error {
	if len(p.Files) != 0 {
		return nil // nothing to do (?)
	}

	// Load the AST.
	if p.ImportPath == "unsafe" {
		// Special case for the unsafe package, which is defined internally by
		// the types package.
		p.Pkg = types.Unsafe
		return nil
	}

	files, err := p.parseFiles()
	if err != nil {
		return err
	}
	p.Files = files

	return nil
}

// Check runs the package through the typechecker. The package must already be
// loaded and all dependencies must have been checked already.
//
// Idempotent.
func (p *Package) Check() error {
	if p.Pkg != nil {
		return nil // already typechecked
	}

	var typeErrors []error
	checker := p.program.typeChecker // make a copy, because it will be modified
	checker.Error = func(err error) {
		typeErrors = append(typeErrors, err)
	}

	// Do typechecking of the package.
	checker.Importer = p

	typesPkg, err := checker.Check(p.ImportPath, p.program.fset, p.Files, &p.info)
	if err != nil {
		if err, ok := err.(Errors); ok {
			return err
		}
		return Errors{p, typeErrors}
	}
	p.Pkg = typesPkg
	return nil
}

// parseFiles parses the loaded list of files and returns this list.
func (p *Package) parseFiles() ([]*ast.File, error) {
	var files []*ast.File
	var fileErrs []error

	// Parse all files (incuding CgoFiles).
	parseFile := func(file string) {
		if !filepath.IsAbs(file) {
			file = filepath.Join(p.Dir, file)
		}
		f, err := p.program.parseFile(file, parser.ParseComments)
		if err != nil {
			fileErrs = append(fileErrs, err)
			return
		}
		files = append(files, f)
	}
	for _, file := range p.GoFiles {
		parseFile(file)
	}
	for _, file := range p.CgoFiles {
		parseFile(file)
	}

	// Do CGo processing.
	if len(p.CgoFiles) != 0 {
		var cflags []string
		cflags = append(cflags, p.program.config.CFlags()...)
		cflags = append(cflags, "-I"+p.Dir)
		if p.program.clangHeaders != "" {
			cflags = append(cflags, "-Xclang", "-internal-isystem", "-Xclang", p.program.clangHeaders)
		}
		generated, ldflags, errs := cgo.Process(files, p.program.workingDir, p.program.fset, cflags)
		if errs != nil {
			fileErrs = append(fileErrs, errs...)
		}
		files = append(files, generated)
		p.program.LDFlags = append(p.program.LDFlags, ldflags...)
	}

	// Only return an error after CGo processing, so that errors in parsing and
	// CGo can be reported together.
	if len(fileErrs) != 0 {
		return nil, Errors{p, fileErrs}
	}

	return files, nil
}

// Import implements types.Importer. It loads and parses packages it encounters
// along the way, if needed.
func (p *Package) Import(to string) (*types.Package, error) {
	if to == "unsafe" {
		return types.Unsafe, nil
	}
	if replace, ok := p.ImportMap[to]; ok {
		// This import path should be replaced by another import path, according
		// to `go list`.
		to = replace
	}
	if imported, ok := p.program.Packages[to]; ok {
		return imported.Pkg, nil
	} else {
		return nil, errors.New("package not imported: " + to)
	}
}
