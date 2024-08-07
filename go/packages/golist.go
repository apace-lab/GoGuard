// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package packages

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/types"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/bozhen-liu/gopa/go/internal/packagesdriver"
	"github.com/bozhen-liu/gopa/internal/gocommand"
	"golang.org/x/xerrors"
)

// debug controls verbose logging.
var debug, _ = strconv.ParseBool(os.Getenv("GOPACKAGESDEBUG"))

// A goTooOldError reports that the go command
// found by exec.LookPath is too old to use the new go list behavior.
type goTooOldError struct {
	error
}

// responseDeduper wraps a driverResponse, deduplicating its contents.
type responseDeduper struct {
	seenRoots    map[string]bool
	seenPackages map[string]*Package
	dr           *driverResponse
}

func newDeduper() *responseDeduper {
	return &responseDeduper{
		dr:           &driverResponse{},
		seenRoots:    map[string]bool{},
		seenPackages: map[string]*Package{},
	}
}

// addAll fills in r with a driverResponse.
func (r *responseDeduper) addAll(dr *driverResponse) {
	for _, pkg := range dr.Packages {
		r.addPackage(pkg)
	}
	for _, root := range dr.Roots {
		r.addRoot(root)
	}
}

func (r *responseDeduper) addPackage(p *Package) {
	if r.seenPackages[p.ID] != nil {
		return
	}
	r.seenPackages[p.ID] = p
	r.dr.Packages = append(r.dr.Packages, p)
}

func (r *responseDeduper) addRoot(id string) {
	if r.seenRoots[id] {
		return
	}
	r.seenRoots[id] = true
	r.dr.Roots = append(r.dr.Roots, id)
}

type golistState struct {
	cfg *Config
	ctx context.Context

	envOnce    sync.Once
	goEnvError error
	goEnv      map[string]string

	rootsOnce     sync.Once
	rootDirsError error
	rootDirs      map[string]string

	goVersionOnce  sync.Once
	goVersionError error
	goVersion      string // third field of 'go version'

	// vendorDirs caches the (non)existence of vendor directories.
	vendorDirs map[string]bool
}

// getEnv returns Go environment variables. Only specific variables are
// populated -- computing all of them is slow.
func (state *golistState) getEnv() (map[string]string, error) {
	state.envOnce.Do(func() {
		var b *bytes.Buffer
		b, state.goEnvError = state.invokeGo("env", "-json", "GOMOD", "GOPATH")
		if state.goEnvError != nil {
			return
		}

		state.goEnv = make(map[string]string)
		decoder := json.NewDecoder(b)
		if state.goEnvError = decoder.Decode(&state.goEnv); state.goEnvError != nil {
			return
		}
	})
	return state.goEnv, state.goEnvError
}

// mustGetEnv is a convenience function that can be used if getEnv has already succeeded.
func (state *golistState) mustGetEnv() map[string]string {
	env, err := state.getEnv()
	if err != nil {
		panic(fmt.Sprintf("mustGetEnv: %v", err))
	}
	return env
}

// goListDriver uses the go list command to interpret the patterns and produce
// the build system package structure.
// See driver for more details.
func goListDriver(cfg *Config, patterns ...string) (*driverResponse, error) {
	// Make sure that any asynchronous go commands are killed when we return.
	parentCtx := cfg.Context
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	response := newDeduper()

	// Fill in response.Sizes asynchronously if necessary.
	var sizeserr error
	var sizeswg sync.WaitGroup
	if cfg.Mode&NeedTypesSizes != 0 || cfg.Mode&NeedTypes != 0 {
		sizeswg.Add(1)
		go func() {
			var sizes types.Sizes
			sizes, sizeserr = packagesdriver.GetSizesGolist(ctx, cfg.BuildFlags, cfg.Env, cfg.gocmdRunner, cfg.Dir)
			// types.SizesFor always returns nil or a *types.StdSizes.
			response.dr.Sizes, _ = sizes.(*types.StdSizes)
			sizeswg.Done()
		}()
	}

	state := &golistState{
		cfg:        cfg,
		ctx:        ctx,
		vendorDirs: map[string]bool{},
	}

	// Determine files requested in contains patterns
	var containFiles []string
	restPatterns := make([]string, 0, len(patterns))
	// Extract file= and other [querytype]= patterns. Report an error if querytype
	// doesn't exist.
extractQueries:
	for _, pattern := range patterns {
		eqidx := strings.Index(pattern, "=")
		if eqidx < 0 {
			restPatterns = append(restPatterns, pattern)
		} else {
			query, value := pattern[:eqidx], pattern[eqidx+len("="):]
			switch query {
			case "file":
				containFiles = append(containFiles, value)
			case "pattern":
				restPatterns = append(restPatterns, value)
			case "": // not a reserved query
				restPatterns = append(restPatterns, pattern)
			default:
				for _, rune := range query {
					if rune < 'a' || rune > 'z' { // not a reserved query
						restPatterns = append(restPatterns, pattern)
						continue extractQueries
					}
				}
				// Reject all other patterns containing "="
				return nil, fmt.Errorf("invalid query type %q in query pattern %q", query, pattern)
			}
		}
	}

	// See if we have any patterns to pass through to go list. Zero initial
	// patterns also requires a go list call, since it's the equivalent of ".".
	// !! bz: when run race_checker under proj dir, it goes to here
	//        hence we are going to make this recursively traverse the subdir under the initial cfg.Dir
	if len(restPatterns) > 0 || len(patterns) == 0 {
		dr, err := state.createDriverResponse(restPatterns...) //bz: run go list cmd
		if err != nil {
			return nil, err
		}
		response.addAll(dr)

		//bz: when run under proj dir, find all mains
		handleDriverUnderDir(restPatterns, patterns, response, cfg, ctx)
	}

	if len(containFiles) != 0 { //bz: another option ？ not used now
		if err := state.runContainsQueries(response, containFiles); err != nil {
			return nil, err
		}
	}

	modifiedPkgs, needPkgs, err := state.processGolistOverlay(response)
	if err != nil {
		return nil, err
	}

	var containsCandidates []string
	if len(containFiles) > 0 {
		containsCandidates = append(containsCandidates, modifiedPkgs...)
		containsCandidates = append(containsCandidates, needPkgs...)
	}
	if err := state.addNeededOverlayPackages(response, needPkgs); err != nil {
		return nil, err
	}
	// Check candidate packages for containFiles.
	if len(containFiles) > 0 {
		for _, id := range containsCandidates {
			pkg, ok := response.seenPackages[id]
			if !ok {
				response.addPackage(&Package{
					ID: id,
					Errors: []Error{
						{
							Kind: ListError,
							Msg:  fmt.Sprintf("package %s expected but not seen", id),
						},
					},
				})
				continue
			}
			for _, f := range containFiles {
				for _, g := range pkg.GoFiles {
					if sameFile(f, g) {
						response.addRoot(id) //main
					}
				}
			}
		}
	}

	sizeswg.Wait()
	if sizeserr != nil {
		return nil, sizeserr
	}
	return response.dr, nil
}

//bz: when run under proj dir, find all mains
func handleDriverUnderDir(restPatterns []string, patterns []string, response *responseDeduper, cfg *Config, ctx context.Context) {
	skip := false
	//bz: EXCPETION: we will not traverse under proj dir if input is a specific go file
	if len(restPatterns) == 1 && len(patterns) == 1 {
		restPattern := restPatterns[0]
		pattern := patterns[0]
		if restPattern == pattern { //both point to the go file
			if strings.LastIndex(pattern, ".go") == len(pattern) - 3 {
				skip = true
			}
		}
	}

	if !skip {
		//bz: condition filter (skip) to do the list all main entry points if input is not a .go file
		//bz: list this subdir recursively until none
		var subdirs []string //bz: the subdirectories
		if runtime.GOOS == "windows" {//this does not work ... @https://helpdesk.kaseya.com/hc/en-gb/articles/229044948-Recursive-directory-listing-in-Windows
			//udpated use pathwalker
			fmt.Println("Runtime from Windows: use path walker.")
			//cmd = exec.Command("dir", "/b", "/s", "/a:-D")
			err := filepath.Walk(cfg.Dir, func(path string, info os.FileInfo, err error) error {
				if info.IsDir() {
					subdirs = append(subdirs, path)
				}
				return nil
			})
			if err != nil {
				panic(fmt.Sprintf("!!! Windows: path walker failed with err: %s\n", err))
			}

			if len(subdirs) > 0 {
				//goListDriverRecursive(subdirs, size, response, cfg, ctx, restPatterns)
				goListDriverRecursiveSeq(subdirs, response, cfg, ctx, restPatterns)
			}
		} else { //default: Unix (MacOS + Ubuntu)
			cmd := exec.Command("find", ".", "-type", "d")
			cmd.Dir = cfg.Dir   //set cmd dir
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			cmderr := cmd.Run()
			if cmderr != nil { //bz: change to panic
				panic(fmt.Sprintf("!!! cmd.Run() failed with %s\n", cmderr))
			}
			outStr, _ := string(stdout.Bytes()), string(stderr.Bytes())
			//fmt.Printf("run ls cmd. out:\n%s\nerr:\n%s\n", outStr, errStr) //bz: for me to debug
			subdirs = strings.Split(outStr, "\n") //bz: record the future dir we need to traverse

			if len(subdirs) - 2 > 0 {
				//goListDriverRecursive(subdirs, size, response, cfg, ctx, restPatterns)
				goListDriverRecursiveSeq(subdirs, response, cfg, ctx, restPatterns)
			}
		}
	}
}

//bz: let's do this sequentially ....
func goListDriverRecursiveSeq(subdirs []string, response *responseDeduper, cfg *Config,
	ctx context.Context, restPatterns []string) {
	subdirs = removeDuplicateValues(subdirs)
	for i := 1; i < len(subdirs)-1; i++ {    //bz: 1st element is ".", the last element is "", skip them
		subdir := subdirs[i]
		var realDir string //os dependent var
		if runtime.GOOS == "windows" {
			realDir = subdir //bz: windows path
		}else{
			realDir = cfg.Dir + subdir[1:] // bz: we update this. remove the "." in subdir
		}

		_cfg := &Config{
			Mode:    LoadAllSyntax,
			Context: cfg.Context,
			Logf:    cfg.Logf,
			Dir:     realDir,
			Env:     cfg.Env,
			Tests:   cfg.Tests,
		}
		_state := &golistState{
			cfg:        _cfg,
			ctx:        ctx,
			vendorDirs: map[string]bool{},
		}
		_dr, _err := _state.createDriverResponse(restPatterns...)
		if _err != nil {
			fmt.Println("ERROR from _state.createDriverResponse: %s", _err)
		}
		if _dr != nil {
			response.addAll(_dr)
		}
	}
}

//bz: do main search
func goListDriverRecursive(subdirs []string, size int, response *responseDeduper, cfg *Config,
	ctx context.Context, restPatterns []string) {
	subdirs = removeDuplicateValues(subdirs)
	var _wg sync.WaitGroup
	results := make([]*driverResponse, size) //all results
	for i := 1; i < len(subdirs)-1; i++ {    //bz: 1st element is ".", the last element is "", skip them
		subdir := subdirs[i]
		var realDir string //os dependent var
		if runtime.GOOS == "windows" {
			realDir = subdir //bz: windows path
		}else{
			realDir = cfg.Dir + subdir[1:] // bz: we update this. remove the "." in subdir
		}

		_cfg := &Config{
			Mode:    LoadAllSyntax,
			Context: cfg.Context,
			Logf:    cfg.Logf,
			Dir:     realDir,
			Env:     cfg.Env,
			Tests:   cfg.Tests,
		}
		_state := &golistState{
			cfg:        _cfg,
			ctx:        ctx,
			vendorDirs: map[string]bool{},
		}
		_wg.Add(1)
		go func(i int, _state *golistState, restPatterns []string) {
			_dr, _err := _state.createDriverResponse(restPatterns...)
			if _err != nil {
				fmt.Println("ERROR from _state.createDriverResponse: %s", _err)
				results[i-1] = nil
			} else {
				results[i-1] = _dr
			}
			_wg.Done()
		}(i, _state, restPatterns)
	}
	_wg.Wait()

	//bz: sum up
	for _, ret := range results {
		if ret != nil {
			response.addAll(ret)
		}
	}
}

//bz:
func removeDuplicateValues(subdirs []string) []string {
	keys := make(map[string]bool)
	list := []string{}

	// If the key(values of the slice) is not equal
	// to the already present value in new slice (list)
	// then we append it. else we jump on another element.
	for _, entry := range subdirs {
		if entry == "" {
			continue
		}
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

func (state *golistState) addNeededOverlayPackages(response *responseDeduper, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}
	dr, err := state.createDriverResponse(pkgs...)
	if err != nil {
		return err
	}
	for _, pkg := range dr.Packages {
		response.addPackage(pkg)
	}
	_, needPkgs, err := state.processGolistOverlay(response)
	if err != nil {
		return err
	}
	return state.addNeededOverlayPackages(response, needPkgs)
}

func (state *golistState) runContainsQueries(response *responseDeduper, queries []string) error {
	for _, query := range queries {
		// TODO(matloob): Do only one query per directory.
		fdir := filepath.Dir(query)
		// Pass absolute path of directory to go list so that it knows to treat it as a directory,
		// not a package path.
		pattern, err := filepath.Abs(fdir)
		if err != nil {
			return fmt.Errorf("could not determine absolute path of file= query path %q: %v", query, err)
		}
		dirResponse, err := state.createDriverResponse(pattern)

		// If there was an error loading the package, or the package is returned
		// with errors, try to load the file as an ad-hoc package.
		// Usually the error will appear in a returned package, but may not if we're
		// in module mode and the ad-hoc is located outside a module.
		if err != nil || len(dirResponse.Packages) == 1 && len(dirResponse.Packages[0].GoFiles) == 0 &&
			len(dirResponse.Packages[0].Errors) == 1 {
			var queryErr error
			if dirResponse, queryErr = state.adhocPackage(pattern, query); queryErr != nil {
				return err // return the original error
			}
		}
		isRoot := make(map[string]bool, len(dirResponse.Roots))
		for _, root := range dirResponse.Roots {
			isRoot[root] = true
		}
		for _, pkg := range dirResponse.Packages {
			// Add any new packages to the main set
			// We don't bother to filter packages that will be dropped by the changes of roots,
			// that will happen anyway during graph construction outside this function.
			// Over-reporting packages is not a problem.
			response.addPackage(pkg)
			// if the package was not a root one, it cannot have the file
			if !isRoot[pkg.ID] {
				continue
			}
			for _, pkgFile := range pkg.GoFiles {
				if filepath.Base(query) == filepath.Base(pkgFile) {
					response.addRoot(pkg.ID)
					break
				}
			}
		}
	}
	return nil
}

// adhocPackage attempts to load or construct an ad-hoc package for a given
// query, if the original call to the driver produced inadequate results.
func (state *golistState) adhocPackage(pattern, query string) (*driverResponse, error) {
	response, err := state.createDriverResponse(query)
	if err != nil {
		return nil, err
	}
	// If we get nothing back from `go list`,
	// try to make this file into its own ad-hoc package.
	// TODO(rstambler): Should this check against the original response?
	if len(response.Packages) == 0 {
		response.Packages = append(response.Packages, &Package{
			ID:              "command-line-arguments",
			PkgPath:         query,
			GoFiles:         []string{query},
			CompiledGoFiles: []string{query},
			Imports:         make(map[string]*Package),
		})
		response.Roots = append(response.Roots, "command-line-arguments")
	}
	// Handle special cases.
	if len(response.Packages) == 1 {
		// golang/go#33482: If this is a file= query for ad-hoc packages where
		// the file only exists on an overlay, and exists outside of a module,
		// add the file to the package and remove the errors.
		if response.Packages[0].ID == "command-line-arguments" ||
			filepath.ToSlash(response.Packages[0].PkgPath) == filepath.ToSlash(query) {
			if len(response.Packages[0].GoFiles) == 0 {
				filename := filepath.Join(pattern, filepath.Base(query)) // avoid recomputing abspath
				// TODO(matloob): check if the file is outside of a root dir?
				for path := range state.cfg.Overlay {
					if path == filename {
						response.Packages[0].Errors = nil
						response.Packages[0].GoFiles = []string{path}
						response.Packages[0].CompiledGoFiles = []string{path}
					}
				}
			}
		}
	}
	return response, nil
}

// Fields must match go list;
// see $GOROOT/src/cmd/go/internal/load/pkg.go.
type jsonPackage struct {
	ImportPath      string
	Dir             string
	Name            string
	Export          string
	GoFiles         []string
	CompiledGoFiles []string
	CFiles          []string
	CgoFiles        []string
	CXXFiles        []string
	MFiles          []string
	HFiles          []string
	FFiles          []string
	SFiles          []string
	SwigFiles       []string
	SwigCXXFiles    []string
	SysoFiles       []string
	Imports         []string
	ImportMap       map[string]string
	Deps            []string
	Module          *Module
	TestGoFiles     []string
	TestImports     []string
	XTestGoFiles    []string
	XTestImports    []string
	ForTest         string // q in a "p [q.test]" package, else ""
	DepOnly         bool

	Error *jsonPackageError
}

type jsonPackageError struct {
	ImportStack []string
	Pos         string
	Err         string
}

func otherFiles(p *jsonPackage) [][]string {
	return [][]string{p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles, p.SFiles, p.SwigFiles, p.SwigCXXFiles, p.SysoFiles}
}

// createDriverResponse uses the "go list" command to expand the pattern
// words and return a response for the specified packages.
func (state *golistState) createDriverResponse(words ...string) (*driverResponse, error) {
	// go list uses the following identifiers in ImportPath and Imports:
	//
	// 	"p"			-- importable package or main (command)
	// 	"q.test"		-- q's test executable
	// 	"p [q.test]"		-- variant of p as built for q's test executable
	// 	"q_test [q.test]"	-- q's external test package
	//
	// The packages p that are built differently for a test q.test
	// are q itself, plus any helpers used by the external test q_test,
	// typically including "golibexec_testing" and all its dependencies.

	// Run "go list" for complete
	// information on the specified packages.
	buf, err := state.invokeGo("list", golistargs(state.cfg, words)...)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]*jsonPackage)
	pkgs := make(map[string]*Package)
	additionalErrors := make(map[string][]Error)
	// Decode the JSON and convert it to Package form.
	var response driverResponse
	for dec := json.NewDecoder(buf); dec.More(); {
		p := new(jsonPackage)
		if err := dec.Decode(p); err != nil { //bz: decode
			return nil, fmt.Errorf("JSON decoding failed: %v", err)
		}

		if p.Name == "" && p.ImportPath == "." {
			continue //bz: this is the root dir when we run race_checker under this dir, ignore this.
		}

		if p.ImportPath == "" {
			// The documentation for go list says that “[e]rroneous packages will have
			// a non-empty ImportPath”. If for some reason it comes back empty, we
			// prefer to error out rather than silently discarding data or handing
			// back a package without any way to refer to it.
			if p.Error != nil {
				return nil, Error{
					Pos: p.Error.Pos,
					Msg: p.Error.Err,
				}
			}
			return nil, fmt.Errorf("package missing import path: %+v", p)
		}

		// Work around https://golang.org/issue/33157:
		// go list -e, when given an absolute path, will find the package contained at
		// that directory. But when no package exists there, it will return a fake package
		// with an error and the ImportPath set to the absolute path provided to go list.
		// Try to convert that absolute path to what its package path would be if it's
		// contained in a known module or GOPATH entry. This will allow the package to be
		// properly "reclaimed" when overlays are processed.
		if filepath.IsAbs(p.ImportPath) && p.Error != nil {
			pkgPath, ok, err := state.getPkgPath(p.ImportPath)
			if err != nil {
				return nil, err
			}
			if ok {
				p.ImportPath = pkgPath
			}
		}

		if old, found := seen[p.ImportPath]; found {
			// If one version of the package has an error, and the other doesn't, assume
			// that this is a case where go list is reporting a fake dependency variant
			// of the imported package: When a package tries to invalidly import another
			// package, go list emits a variant of the imported package (with the same
			// import path, but with an error on it, and the package will have a
			// DepError set on it). An example of when this can happen is for imports of
			// main packages: main packages can not be imported, but they may be
			// separately matched and listed by another pattern.
			// See golang.org/issue/36188 for more details.

			// The plan is that eventually, hopefully in Go 1.15, the error will be
			// reported on the importing package rather than the duplicate "fake"
			// version of the imported package. Once all supported versions of Go
			// have the new behavior this logic can be deleted.
			// TODO(matloob): delete the workaround logic once all supported versions of
			// Go return the errors on the proper package.

			// There should be exactly one version of a package that doesn't have an
			// error.
			if old.Error == nil && p.Error == nil {
				if !reflect.DeepEqual(p, old) {
					return nil, fmt.Errorf("internal error: go list gives conflicting information for package %v", p.ImportPath)
				}
				continue
			}

			// Determine if this package's error needs to be bubbled up.
			// This is a hack, and we expect for go list to eventually set the error
			// on the package.
			if old.Error != nil {
				var errkind string
				if strings.Contains(old.Error.Err, "not an importable package") {
					errkind = "not an importable package"
				} else if strings.Contains(old.Error.Err, "use of internal package") && strings.Contains(old.Error.Err, "not allowed") {
					errkind = "use of internal package not allowed"
				}
				if errkind != "" {
					if len(old.Error.ImportStack) < 1 {
						return nil, fmt.Errorf(`internal error: go list gave a %q error with empty import stack`, errkind)
					}
					importingPkg := old.Error.ImportStack[len(old.Error.ImportStack)-1]
					if importingPkg == old.ImportPath {
						// Using an older version of Go which put this package itself on top of import
						// stack, instead of the importer. Look for importer in second from top
						// position.
						if len(old.Error.ImportStack) < 2 {
							return nil, fmt.Errorf(`internal error: go list gave a %q error with an import stack without importing package`, errkind)
						}
						importingPkg = old.Error.ImportStack[len(old.Error.ImportStack)-2]
					}
					additionalErrors[importingPkg] = append(additionalErrors[importingPkg], Error{
						Pos:  old.Error.Pos,
						Msg:  old.Error.Err,
						Kind: ListError,
					})
				}
			}

			// Make sure that if there's a version of the package without an error,
			// that's the one reported to the user.
			if old.Error == nil {
				continue
			}

			// This package will replace the old one at the end of the loop.
		}
		seen[p.ImportPath] = p

		pkg := &Package{
			Name:            p.Name,
			ID:              p.ImportPath,
			GoFiles:         absJoin(p.Dir, p.GoFiles, p.CgoFiles),
			CompiledGoFiles: absJoin(p.Dir, p.CompiledGoFiles),
			OtherFiles:      absJoin(p.Dir, otherFiles(p)...),
			forTest:         p.ForTest,
			Module:          p.Module,
		}

		if (state.cfg.Mode&typecheckCgo) != 0 && len(p.CgoFiles) != 0 {
			if len(p.CompiledGoFiles) > len(p.GoFiles) {
				// We need the cgo definitions, which are in the first
				// CompiledGoFile after the non-cgo ones. This is a hack but there
				// isn't currently a better way to find it. We also need the pure
				// Go files and unprocessed cgo files, all of which are already
				// in pkg.GoFiles.
				cgoTypes := p.CompiledGoFiles[len(p.GoFiles)]
				pkg.CompiledGoFiles = append([]string{cgoTypes}, pkg.GoFiles...)
			} else {
				// golang/go#38990: go list silently fails to do cgo processing
				pkg.CompiledGoFiles = nil
				pkg.Errors = append(pkg.Errors, Error{
					Msg:  "go list failed to return CompiledGoFiles; https://golang.org/issue/38990?",
					Kind: ListError,
				})
			}
		}

		// Work around https://golang.org/issue/28749:
		// cmd/go puts assembly, C, and C++ files in CompiledGoFiles.
		// Filter out any elements of CompiledGoFiles that are also in OtherFiles.
		// We have to keep this workaround in place until go1.12 is a distant memory.
		if len(pkg.OtherFiles) > 0 {
			other := make(map[string]bool, len(pkg.OtherFiles))
			for _, f := range pkg.OtherFiles {
				other[f] = true
			}

			out := pkg.CompiledGoFiles[:0]
			for _, f := range pkg.CompiledGoFiles {
				if other[f] {
					continue
				}
				out = append(out, f)
			}
			pkg.CompiledGoFiles = out
		}

		// Extract the PkgPath from the package's ID.
		if i := strings.IndexByte(pkg.ID, ' '); i >= 0 {
			pkg.PkgPath = pkg.ID[:i]
		} else {
			pkg.PkgPath = pkg.ID
		}

		if pkg.PkgPath == "unsafe" {
			pkg.GoFiles = nil // ignore fake unsafe.go file
		}

		// Assume go list emits only absolute paths for Dir.
		if p.Dir != "" && !filepath.IsAbs(p.Dir) {
			log.Fatalf("internal error: go list returned non-absolute Package.Dir: %s", p.Dir)
		}

		if p.Export != "" && !filepath.IsAbs(p.Export) {
			pkg.ExportFile = filepath.Join(p.Dir, p.Export)
		} else {
			pkg.ExportFile = p.Export
		}

		// imports
		//
		// Imports contains the IDs of all imported packages.
		// ImportsMap records (path, ID) only where they differ.
		ids := make(map[string]bool)
		for _, id := range p.Imports {
			ids[id] = true
		}
		pkg.Imports = make(map[string]*Package)
		for path, id := range p.ImportMap {
			pkg.Imports[path] = &Package{ID: id} // non-identity import
			delete(ids, id)
		}
		for id := range ids {
			if id == "C" {
				continue
			}

			pkg.Imports[id] = &Package{ID: id} // identity import
		}
		if !p.DepOnly {
			response.Roots = append(response.Roots, pkg.ID)
		}

		// Work around for pre-go.1.11 versions of go list.
		// TODO(matloob): they should be handled by the fallback.
		// Can we delete this?
		if len(pkg.CompiledGoFiles) == 0 {
			pkg.CompiledGoFiles = pkg.GoFiles
		}

		// Temporary work-around for golang/go#39986. Parse filenames out of
		// error messages. This happens if there are unrecoverable syntax
		// errors in the source, so we can't match on a specific error message.
		if err := p.Error; err != nil && state.shouldAddFilenameFromError(p) {
			addFilenameFromPos := func(pos string) bool {
				split := strings.Split(pos, ":")
				if len(split) < 1 {
					return false
				}
				filename := strings.TrimSpace(split[0])
				if filename == "" {
					return false
				}
				if !filepath.IsAbs(filename) {
					filename = filepath.Join(state.cfg.Dir, filename)
				}
				info, _ := os.Stat(filename)
				if info == nil {
					return false
				}
				pkg.CompiledGoFiles = append(pkg.CompiledGoFiles, filename)
				pkg.GoFiles = append(pkg.GoFiles, filename)
				return true
			}
			found := addFilenameFromPos(err.Pos)
			// In some cases, go list only reports the error position in the
			// error text, not the error position. One such case is when the
			// file's package name is a keyword (see golang.org/issue/39763).
			if !found {
				addFilenameFromPos(err.Err)
			}
		}

		if p.Error != nil {
			msg := strings.TrimSpace(p.Error.Err) // Trim to work around golang.org/issue/32363.
			// Address golang.org/issue/35964 by appending import stack to error message.
			if msg == "import cycle not allowed" && len(p.Error.ImportStack) != 0 {
				msg += fmt.Sprintf(": import stack: %v", p.Error.ImportStack)
			}
			pkg.Errors = append(pkg.Errors, Error{
				Pos:  p.Error.Pos,
				Msg:  msg,
				Kind: ListError,
			})
		}

		pkgs[pkg.ID] = pkg
	}

	for id, errs := range additionalErrors {
		if p, ok := pkgs[id]; ok {
			p.Errors = append(p.Errors, errs...)
		}
	}
	for _, pkg := range pkgs {
		response.Packages = append(response.Packages, pkg)
	}
	sort.Slice(response.Packages, func(i, j int) bool { return response.Packages[i].ID < response.Packages[j].ID })

	return &response, nil
}

func (state *golistState) shouldAddFilenameFromError(p *jsonPackage) bool {
	if len(p.GoFiles) > 0 || len(p.CompiledGoFiles) > 0 {
		return false
	}

	goV, err := state.getGoVersion()
	if err != nil {
		return false
	}

	// On Go 1.14 and earlier, only add filenames from errors if the import stack is empty.
	// The import stack behaves differently for these versions than newer Go versions.
	if strings.HasPrefix(goV, "go1.13") || strings.HasPrefix(goV, "go1.14") {
		return len(p.Error.ImportStack) == 0
	}

	// On Go 1.15 and later, only parse filenames out of error if there's no import stack,
	// or the current package is at the top of the import stack. This is not guaranteed
	// to work perfectly, but should avoid some cases where files in errors don't belong to this
	// package.
	return len(p.Error.ImportStack) == 0 || p.Error.ImportStack[len(p.Error.ImportStack)-1] == p.ImportPath
}

func (state *golistState) getGoVersion() (string, error) {
	state.goVersionOnce.Do(func() {
		var b *bytes.Buffer
		// Invoke go version. Don't use invokeGo because it will supply build flags, and
		// go version doesn't expect build flags.
		inv := gocommand.Invocation{
			Verb: "version",
			Env:  state.cfg.Env,
			Logf: state.cfg.Logf,
		}
		gocmdRunner := state.cfg.gocmdRunner
		if gocmdRunner == nil {
			gocmdRunner = &gocommand.Runner{}
		}
		b, _, _, state.goVersionError = gocmdRunner.RunRaw(state.cfg.Context, inv)
		if state.goVersionError != nil {
			return
		}

		sp := strings.Split(b.String(), " ")
		if len(sp) < 3 {
			state.goVersionError = fmt.Errorf("go version output: expected 'go version <version>', got '%s'", b.String())
			return
		}
		state.goVersion = sp[2]
	})
	return state.goVersion, state.goVersionError
}

// getPkgPath finds the package path of a directory if it's relative to a root directory.
func (state *golistState) getPkgPath(dir string) (string, bool, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	roots, err := state.determineRootDirs()
	if err != nil {
		return "", false, err
	}

	for rdir, rpath := range roots {
		// Make sure that the directory is in the module,
		// to avoid creating a path relative to another module.
		if !strings.HasPrefix(absDir, rdir) {
			continue
		}
		// TODO(matloob): This doesn't properly handle symlinks.
		r, err := filepath.Rel(rdir, dir)
		if err != nil {
			continue
		}
		if rpath != "" {
			// We choose only one root even though the directory even it can belong in multiple modules
			// or GOPATH entries. This is okay because we only need to work with absolute dirs when a
			// file is missing from disk, for instance when gopls calls go/packages in an overlay.
			// Once the file is saved, gopls, or the next invocation of the tool will get the correct
			// result straight from golist.
			// TODO(matloob): Implement module tiebreaking?
			return path.Join(rpath, filepath.ToSlash(r)), true, nil
		}
		return filepath.ToSlash(r), true, nil
	}
	return "", false, nil
}

// absJoin absolutizes and flattens the lists of files.
func absJoin(dir string, fileses ...[]string) (res []string) {
	for _, files := range fileses {
		for _, file := range files {
			if !filepath.IsAbs(file) {
				file = filepath.Join(dir, file)
			}
			res = append(res, file)
		}
	}
	return res
}

func golistargs(cfg *Config, words []string) []string {
	const findFlags = NeedImports | NeedTypes | NeedSyntax | NeedTypesInfo
	fullargs := []string{
		"-e", "-json",
		fmt.Sprintf("-compiled=%t", cfg.Mode&(NeedCompiledGoFiles|NeedSyntax|NeedTypes|NeedTypesInfo|NeedTypesSizes) != 0),
		fmt.Sprintf("-test=%t", cfg.Tests),
		fmt.Sprintf("-export=%t", usesExportData(cfg)),
		fmt.Sprintf("-deps=%t", cfg.Mode&NeedImports != 0),
		// go list doesn't let you pass -test and -find together,
		// probably because you'd just get the TestMain.
		fmt.Sprintf("-find=%t", !cfg.Tests && cfg.Mode&findFlags == 0),
	}
	fullargs = append(fullargs, cfg.BuildFlags...)
	fullargs = append(fullargs, "--")
	fullargs = append(fullargs, words...)
	return fullargs
}

// invokeGo returns the stdout of a go command invocation.
func (state *golistState) invokeGo(verb string, args ...string) (*bytes.Buffer, error) {
	cfg := state.cfg

	inv := gocommand.Invocation{
		Verb:       verb,
		Args:       args,
		BuildFlags: cfg.BuildFlags,
		Env:        cfg.Env,
		Logf:       cfg.Logf,
		WorkingDir: cfg.Dir, //bz: this is the key part
	}
	gocmdRunner := cfg.gocmdRunner
	if gocmdRunner == nil {
		gocmdRunner = &gocommand.Runner{}
	}
	stdout, stderr, _, err := gocmdRunner.RunRaw(cfg.Context, inv)
	if err != nil {
		// Check for 'go' executable not being found.
		if ee, ok := err.(*exec.Error); ok && ee.Err == exec.ErrNotFound {
			return nil, fmt.Errorf("'go list' driver requires 'go', but %s", exec.ErrNotFound)
		}

		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			// Catastrophic error:
			// - context cancellation
			return nil, xerrors.Errorf("couldn't run 'go': %w", err)
		}

		// Old go version?
		if strings.Contains(stderr.String(), "flag provided but not defined") {
			return nil, goTooOldError{fmt.Errorf("unsupported version of go: %s: %s", exitErr, stderr)}
		}

		// Related to #24854
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "unexpected directory layout") {
			return nil, fmt.Errorf("%s", stderr.String())
		}

		// Is there an error running the C compiler in cgo? This will be reported in the "Error" field
		// and should be suppressed by go list -e.
		//
		// This condition is not perfect yet because the error message can include other error messages than runtime/cgo.
		isPkgPathRune := func(r rune) bool {
			// From https://golang.org/ref/spec#Import_declarations:
			//    Implementation restriction: A compiler may restrict ImportPaths to non-empty strings
			//    using only characters belonging to Unicode's L, M, N, P, and S general categories
			//    (the Graphic characters without spaces) and may also exclude the
			//    characters !"#$%&'()*,:;<=>?[\]^`{|} and the Unicode replacement character U+FFFD.
			return unicode.IsOneOf([]*unicode.RangeTable{unicode.L, unicode.M, unicode.N, unicode.P, unicode.S}, r) &&
				!strings.ContainsRune("!\"#$%&'()*,:;<=>?[\\]^`{|}\uFFFD", r)
		}
		if len(stderr.String()) > 0 && strings.HasPrefix(stderr.String(), "# ") {
			msg := stderr.String()[len("# "):]
			if strings.HasPrefix(strings.TrimLeftFunc(msg, isPkgPathRune), "\n") {
				return stdout, nil
			}
			// Treat pkg-config errors as a special case (golang.org/issue/36770).
			if strings.HasPrefix(msg, "pkg-config") {
				return stdout, nil
			}
		}

		// This error only appears in stderr. See golang.org/cl/166398 for a fix in go list to show
		// the error in the Err section of stdout in case -e option is provided.
		// This fix is provided for backwards compatibility.
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "named files must be .go files") {
			output := fmt.Sprintf(`{"ImportPath": "command-line-arguments","Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Similar to the previous error, but currently lacks a fix in Go.
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "named files must all be in one directory") {
			output := fmt.Sprintf(`{"ImportPath": "command-line-arguments","Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Backwards compatibility for Go 1.11 because 1.12 and 1.13 put the directory in the ImportPath.
		// If the package doesn't exist, put the absolute path of the directory into the error message,
		// as Go 1.13 list does.
		const noSuchDirectory = "no such directory"
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), noSuchDirectory) {
			errstr := stderr.String()
			abspath := strings.TrimSpace(errstr[strings.Index(errstr, noSuchDirectory)+len(noSuchDirectory):])
			output := fmt.Sprintf(`{"ImportPath": %q,"Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				abspath, strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Workaround for #29280: go list -e has incorrect behavior when an ad-hoc package doesn't exist.
		// Note that the error message we look for in this case is different that the one looked for above.
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "no such file or directory") {
			output := fmt.Sprintf(`{"ImportPath": "command-line-arguments","Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Workaround for #34273. go list -e with GO111MODULE=on has incorrect behavior when listing a
		// directory outside any module.
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "outside available modules") {
			output := fmt.Sprintf(`{"ImportPath": %q,"Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				// TODO(matloob): command-line-arguments isn't correct here.
				"command-line-arguments", strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Another variation of the previous error
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "outside module root") {
			output := fmt.Sprintf(`{"ImportPath": %q,"Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				// TODO(matloob): command-line-arguments isn't correct here.
				"command-line-arguments", strings.Trim(stderr.String(), "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Workaround for an instance of golang.org/issue/26755: go list -e  will return a non-zero exit
		// status if there's a dependency on a package that doesn't exist. But it should return
		// a zero exit status and set an error on that package.
		if len(stderr.String()) > 0 && strings.Contains(stderr.String(), "no Go files in") {
			// Don't clobber stdout if `go list` actually returned something.
			if len(stdout.String()) > 0 {
				return stdout, nil
			}
			// try to extract package name from string
			stderrStr := stderr.String()
			var importPath string
			colon := strings.Index(stderrStr, ":")
			if colon > 0 && strings.HasPrefix(stderrStr, "go build ") {
				importPath = stderrStr[len("go build "):colon]
			}
			output := fmt.Sprintf(`{"ImportPath": %q,"Incomplete": true,"Error": {"Pos": "","Err": %q}}`,
				importPath, strings.Trim(stderrStr, "\n"))
			return bytes.NewBufferString(output), nil
		}

		// Export mode entails a build.
		// If that build fails, errors appear on stderr
		// (despite the -e flag) and the Export field is blank.
		// Do not fail in that case.
		// The same is true if an ad-hoc package given to go list doesn't exist.
		// TODO(matloob): Remove these once we can depend on go list to exit with a zero status with -e even when
		// packages don't exist or a build fails.
		if !usesExportData(cfg) && !containsGoFile(args) {
			return nil, fmt.Errorf("go %v: %s: %s", args, exitErr, stderr)
		}
	}
	return stdout, nil
}

func containsGoFile(s []string) bool {
	for _, f := range s {
		if strings.HasSuffix(f, ".go") {
			return true
		}
	}
	return false
}

func cmdDebugStr(cmd *exec.Cmd, args ...string) string {
	env := make(map[string]string)
	for _, kv := range cmd.Env {
		split := strings.Split(kv, "=")
		k, v := split[0], split[1]
		env[k] = v
	}
	var quotedArgs []string
	for _, arg := range args {
		quotedArgs = append(quotedArgs, strconv.Quote(arg))
	}

	return fmt.Sprintf("GOROOT=%v GOPATH=%v GO111MODULE=%v PWD=%v go %s", env["GOROOT"], env["GOPATH"], env["GO111MODULE"], env["PWD"], strings.Join(quotedArgs, " "))
}
