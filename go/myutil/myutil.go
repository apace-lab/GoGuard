package myutil

import (
	"flag"
	"fmt"
	"github.com/bozhen-liu/gopa/go/callgraph"
	"github.com/bozhen-liu/gopa/go/myutil/compare"
	"github.com/bozhen-liu/gopa/go/myutil/flags"
	"github.com/bozhen-liu/gopa/go/packages"
	"github.com/bozhen-liu/gopa/go/pointer"
	default_algo "github.com/bozhen-liu/gopa/go/pointer_default"
	"github.com/bozhen-liu/gopa/go/ssa"
	"github.com/bozhen-liu/gopa/go/ssa/ssautil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//bz: utility functions and var declared for my use
// some package names for git repos:
// "google.golang.org/grpc"
// "github.com/pingcap/tidb"
// "k8s.io/kubernetes"
// "github.com/ethereum/go-ethereum"

var excludedPkgs = []string{ //bz: excluded a lot of default constraints -> only works if a.config.Level == 1 or turn on DoCallback (check a.createForLevelX() for details)
	//"runtime",
	//"reflect", -> only consider when turn on a.config.Reflection or analyzing tests
	//"os",

	//bz: check /_founds/sum.md for the following exclusions -> create too many interface related type of pointers with pts > 100
	"fmt",
	"errors", //there are so many wrappers of errors ...
}

var scopePkgs = []string{ // considered function sig
	"context.WithCancel", // TODO:  context.cancelCtx
	"context.WithTimeout",
	"(*context.cancelCtx).cancel",
	"time.AfterFunc",
}

var MyMaxTime time.Duration
var MyMinTime time.Duration
var MyElapsed int64

var DefaultMaxTime time.Duration
var DefaultMinTime time.Duration
var DefaultElapsed int64

// InitialMain do preparation job
// if path != "": run in a folder
func InitialMain(path string) (*ssa.Program, []*ssa.Package, []*ssa.Package) {
	flags.ParseFlags()
	args := flag.Args()
	cfg := &packages.Config{
		Mode:  packages.LoadAllSyntax, // the level of information returned for each package
		Dir:   flags.AppDir,           // directory in which to run the build system's query tool
		Tests: flags.DoTests,          // setting Tests will include related test packages
	}
	if path != "" {
		args = append(args, path)
	}
	return initial(args, cfg)
}

//do preparation job: common job
func initial(args []string, cfg *packages.Config) (*ssa.Program, []*ssa.Package, []*ssa.Package) {
	fmt.Println("Loading input packages...")
	load, err := packages.Load(cfg, args...)
	if err != nil {
		panic(fmt.Sprintln(err))
	}
	if len(load) == 0 {
		fmt.Println("Package list empty")
		return nil, nil, nil
	} else if load[0] == nil {
		fmt.Println("Nil package in list")
		return nil, nil, nil
	} else if len(load) == 1 && len(load[0].Errors) > 0 {
		fmt.Println(load[0].Errors[0])
	}
	////bz: even though there are errors in initial pkgs, pointer analysis can still run on them
	//// -> tmp comment off the following code
	//else if packages.PrintErrors(initial) > 0 {
	//	errSize, errPkgs := packages.PrintErrorsAndMore(initial) //bz: errPkg will be nil in initial
	//	if errSize > 0 {
	//		fmt.Println("Excluded the following packages contain errors, due to the above errors. ")
	//		for i, errPkg := range errPkgs {
	//			fmt.Println(i, " ", errPkg.ID)
	//		}
	//		fmt.Println("Continue   -- ")
	//	}
	//	if len(initial) == 0 || initial[0] == nil {
	//		fmt.Println("All Error Pkgs, Cannot Analyze. Return. ")
	//		return nil
	//	}
	//}
	fmt.Println("Done  -- " + strconv.Itoa(len(load)) + " packages loaded")

	// Create and build SSA-form program representation.
	prog, pkgs := ssautil.AllPackages(load, 0)

	fmt.Println("Building SSA code for entire program...")
	prog.Build()
	fmt.Println("Done  -- SSA code built")

	mains, tests, err := findMainPackages(pkgs)
	if err != nil {
		fmt.Println(err)
		return nil, nil, nil
	}

	if flags.DoTests {
		fmt.Println("#TOTAL MAIN: " + strconv.Itoa(len(mains)-len(tests)) + "\n")
		fmt.Println("#TOTAL TESTS: " + strconv.Itoa(len(tests)) + "\n")
	} else {
		fmt.Println("#TOTAL MAIN: " + strconv.Itoa(len(mains)) + "\n")
	}
	if flags.Main != "" {
		fmt.Println("Capture -- ", flags.Main, "\n")
	}

	//initial set
	MyMaxTime = 0
	DefaultMaxTime = 0
	MyMinTime = 10000000000000
	DefaultMinTime = 10000000000000

	return prog, mains, tests
}

// mainPackages returns the main/test packages to analyze.
// Each resulting package is named "main" and has a main function.
func findMainPackages(pkgs []*ssa.Package) ([]*ssa.Package, []*ssa.Package, error) {
	var mains []*ssa.Package
	var tests []*ssa.Package
	for _, p := range pkgs {
		if p != nil {
			if p.Pkg.Name() == "main" && p.Func("main") != nil {
				//bz: we may see a main from out-of-scope pkg, e.g.,
				//  k8s.io/apiextensions-apiserver/test/integration/conversion.test when analyzing k8s.io/kubernetes, which is from /kubernetes/vendor/*
				// now wee need to check the pkg with scope[0], for most cases, we only have one scope
				mains = append(mains, p)
				if flags.DoTests && strings.HasSuffix(p.Pkg.String(), ".test") { //this is a test-assembled main, just identify, no other use
					tests = append(tests, p)
				}
			}
		}
	}
	if len(mains) == 0 { //bz: main as the first priority
		return nil, nil, fmt.Errorf("no main packages")
	}
	return mains, tests, nil
}

//baseline: all main together
func DoSameRoot(mains []*ssa.Package) {
	if flags.DoCompare || flags.DoDefault {
		doSameRootDefault(mains)
		fmt.Println("........................................\n........................................")
	}
	if flags.DoDefault {
		return
	}

	doSameRootMy(mains)
}

// DoSeq bz: test usesage in race checker -> this is the major usage now
func DoSeq(mains []*ssa.Package) (map[*ssa.Package]*pointer.Result, map[*ssa.Package]*pointer.ResultWCtx) {
	var logfile *os.File
	var err error
	if flags.DoLog { //create my log file
		loc := "/Users/bozhen/Documents/GoCon/gopa/_logs/my_log_hugo5379.txt"
		//loc := "/Users/bozhenliu/Documents/Go/gopa/_logs/my_log_kubernetes70277.txt"
		logfile, err = os.Create(loc)
		fmt.Println("Generate log @ " + loc)
	} else {
		logfile = nil
	}
	if err != nil {
		panic(fmt.Sprintln(err))
	}

	if flags.AppPkg != "" {
		scopePkgs = append(scopePkgs, flags.AppPkg)
	}

	ptaConfig := &pointer.Config{
		Mains:          mains,
		Reflection:     false,
		BuildCallGraph: true,
		Log:            logfile,
		//CallSiteSensitive: true, //kcfa
		Origin: true, //origin
		//shared config
		K:          1,
		LimitScope: true,         //bz: only consider app methods now -> no import will be considered
		DEBUG:      false,        //bz: rm all printed out info in console
		Scope:      scopePkgs,    // bz: consider context
		Exclusion:  excludedPkgs, //bz: copied from race_checker if any
		TrackMore:  true,         //bz: track pointers with all types
	}

	start := time.Now()                                              //performance
	results, _results, r_err := pointer.AnalyzeMultiMains(ptaConfig) // conduct pointer analysis
	t := time.Now()
	elapsed := t.Sub(start)
	if r_err != nil {
		panic(fmt.Sprintln(r_err))
	}
	fmt.Println("\nDone  -- PTA/CG Build; Using " + elapsed.String() + ".\n ")

	//check queries
	fmt.Println("#Receive Result: ", len(results))
	for main, result := range results {
		fmt.Println("Receive result (#Queries: ", len(result.Queries), ", #IndirectQueries: ", len(result.IndirectQueries),
			", #GlobalQueries: ", len(result.GlobalQueries), ") for main: ", main.String())

		////bz: debug
		//result.Statistics()
		//result.DumpCG()
		//result.CountMyReachUnreachFunctions(true)
	}

	////total statistics
	//if len(results) > 1 {
	//	pointer.TotalStatistics(results)
	//}

	//check for test
	if flags.DoTests {
		for pkg, r := range results {
			mp := r.GetTests()
			if mp == nil {
				continue
			}

			fmt.Println("\n\nTest Functions of: ", pkg)
			for fn, cgn := range mp {
				fmt.Println(fn, "\t-> ", cgn.String())
			}
			_results[pkg].TestEntries = mp
			_results[pkg].IsTest = true
		}
	}

	return results, _results
}

func doSameRootMy(mains []*ssa.Package) *pointer.Result {
	// Configure pointer analysis to build call-graph
	ptaConfig := &pointer.Config{
		Mains:          mains,
		Reflection:     false,
		BuildCallGraph: true,
		Log:            nil,
		//CallSiteSensitive: true, //kcfa
		Origin: true, //origin
		//shared config
		K:          1,
		LimitScope: true,         //bz: only consider app methods now -> no import will be considered
		DEBUG:      false,        //bz: rm all printed out info in console
		Exclusion:  excludedPkgs, //bz: copied from race_checker if any
		TrackMore:  true,         //bz: track pointers with all types
	}

	//*** compute pta here
	start := time.Now() //performance
	var result *pointer.Result
	var r_err error
	if flags.TimeLimit != 0 { //we set time limit
		c := make(chan string, 1)

		// Run the pta in it's own goroutine and pass back it's
		// response into our channel.
		go func() {
			result, r_err = pointer.Analyze(ptaConfig) // conduct pointer analysis
			c <- "done"
		}()

		// Listen on our channel AND a timeout channel - which ever happens first.
		select {
		case res := <-c:
			fmt.Println(res)
		case <-time.After(flags.TimeLimit):
			fmt.Println("\n!! Out of time (", flags.TimeLimit, "). Kill it :(")
			return nil
		}
	} else {
		result, r_err = pointer.Analyze(ptaConfig) // conduct pointer analysis
	}
	t := time.Now()
	elapsed := t.Sub(start)
	if r_err != nil {
		panic(fmt.Sprintln(r_err))
	}

	fmt.Println("\nDone  -- PTA/CG Build; Using " + elapsed.String() + ".\n ")

	if MyMaxTime < elapsed {
		MyMaxTime = elapsed
	}
	if MyMinTime > elapsed {
		MyMinTime = elapsed
	}

	return result
}

func doSameRootDefault(mains []*ssa.Package) []*default_algo.Result {
	// Configure pointer analysis to build call-graph
	ptaConfig := &default_algo.Config{
		Mains:           mains, //one main per time
		Reflection:      false,
		BuildCallGraph:  true,
		Log:             nil,
		DoPerformance:   flags.DoPerformance, //bz: I add to output performance for comparison
		DoRecordQueries: flags.DoCompare,     //bz: record all queries to compare result
	}

	//*** compute pta here
	start := time.Now() //performance
	var result *default_algo.Result
	var r_err error           //bz: result error
	if flags.TimeLimit != 0 { //we set time limit
		c := make(chan string, 1)

		// Run the pta in it's own goroutine and pass back it's
		// response into our channel.
		go func() {
			result, r_err = default_algo.Analyze(ptaConfig) // conduct pointer analysis
			c <- "done"
		}()

		// Listen on our channel AND a timeout channel - which ever happens first.
		select {
		case res := <-c:
			fmt.Println(res)
		case <-time.After(flags.TimeLimit):
			fmt.Println("\n!! Out of time (", flags.TimeLimit, "). Kill it :(")
			return nil
		}
	} else {
		result, r_err = default_algo.Analyze(ptaConfig) // conduct pointer analysis
	}
	t := time.Now()
	elapsed := t.Sub(start)
	if r_err != nil {
		panic(fmt.Sprintln(r_err))
	}

	fmt.Println("\nDone  -- PTA/CG Build; Using ", elapsed.String(), ".\n")

	if DefaultMaxTime < elapsed {
		DefaultMaxTime = elapsed
	}
	if DefaultMinTime > elapsed {
		DefaultMinTime = elapsed
	}

	if len(result.Warnings) > 0 {
		fmt.Println("Warning: ", len(result.Warnings)) //bz: just do not report not used var on result
	}

	return nil
}

//baseline: foreach from default algorithm
func DoEach(mains []*ssa.Package) {
	for i, main := range mains {
		if flags.Main != "" && flags.Main != main.Pkg.Path() { //run for IDX only
			continue
		}

		fmt.Println(i, ". ", main.String())
		var r_default *default_algo.Result
		var r_my *pointer.ResultWCtx

		start := time.Now() //performance
		if flags.DoCompare || flags.DoDefault {
			//default
			fmt.Println("Default Algo: ")
			r_default = doEachMainDefault(i, main) //default pta
			t := time.Now()
			DefaultElapsed = DefaultElapsed + t.Sub(start).Milliseconds()
			start = time.Now()
			fmt.Println("........................................\n........................................")
		}

		if flags.DoDefault {
			continue //skip running mine
		}

		//my
		fmt.Println("My Algo: ")
		r_my = DoEachMainMy(i, main) //mypta
		t := time.Now()
		MyElapsed = MyElapsed + t.Sub(start).Milliseconds()

		if flags.DoCompare {
			if r_default != nil && r_my != nil {
				start = time.Now()
				compare.Compare(r_default, r_my)
				t := time.Now()
				comp_elapsed := t.Sub(start)
				fmt.Println("Compare Total Time: ", comp_elapsed.String()+".")
			} else {
				fmt.Println("\n\n!! Cannot compare results due to OOT.")
			}
		}
		fmt.Println("=============================================================================")
	}
}

func DoEachMainMy(i int, main *ssa.Package) *pointer.ResultWCtx {
	var logfile *os.File
	var err error
	if flags.DoLog { //create my log file
		logfile, err = os.Create("/Users/bozhen/Documents/GoCon/gopa/_logs/my_log_" + strconv.Itoa(i))
	} else {
		logfile = nil
	}

	if err != nil {
		panic(fmt.Sprintln(err))
	}

	var mains []*ssa.Package
	mains = append(mains, main)
	// Configure pointer analysis to build call-graph
	ptaConfig := &pointer.Config{
		Mains:          mains, //bz: NOW assume only one main
		Reflection:     false,
		BuildCallGraph: true,
		Log:            logfile,
		//CallSiteSensitive: true, //kcfa
		Origin: true, //origin
		//shared config
		K:          1,            //bz: how many level of origins? default = 1
		LimitScope: true,         //bz: only consider app methods now -> no import will be considered
		DEBUG:      false,        //bz: rm all printed out info in console
		Exclusion:  excludedPkgs, //bz: copied from race_checker if any
		TrackMore:  true,         //bz: track pointers with types declared in Analyze Scope
	}

	//*** compute pta here
	start := time.Now() //performance
	var result *pointer.Result
	var rErr error
	if flags.TimeLimit != 0 { //we set time limit
		c := make(chan string, 1)

		// Run the pta in it's own goroutine and pass back it's
		// response into our channel.
		go func() {
			result, rErr = pointer.Analyze(ptaConfig) // conduct pointer analysis
			c <- "done"
		}()

		// Listen on our channel AND a timeout channel - which ever happens first.
		select {
		case res := <-c:
			fmt.Println(res)
		case <-time.After(flags.TimeLimit):
			fmt.Println("\n!! Out of time (", flags.TimeLimit, "). Kill it :(")
			return nil
		}
	} else {
		result, rErr = pointer.Analyze(ptaConfig) // conduct pointer analysis
	}
	t := time.Now()
	elapsed := t.Sub(start)
	if rErr != nil {
		panic(fmt.Sprintln(rErr))
	}
	defer logfile.Close()

	if flags.DoPerformance {
		_, _, _, preNodes, preFuncs := result.GetResult().CountMyReachUnreachFunctions(flags.DoDetail)
		fmt.Println("#Unreach Nodes: ", len(preNodes))
		fmt.Println("#Reach Nodes: ", len(result.GetResult().CallGraph.Nodes)-len(preNodes))
		fmt.Println("#Unreach Functions: ", len(preNodes))
		fmt.Println("#Reach Functions: ", len(result.GetResult().CallGraph.Nodes)-len(preFuncs))
		fmt.Println("\n#Unreach Nodes from Pre-Gen Nodes: ", len(preNodes))
		fmt.Println("#Unreach Functions from Pre-Gen Nodes: ", len(preFuncs))
		fmt.Println("#(Pre-Gen are created for reflections)")
	}

	fmt.Println("\nDone  -- PTA/CG Build; Using " + elapsed.String() + ".\n ")

	if MyMaxTime < elapsed {
		MyMaxTime = elapsed
	}
	if MyMinTime > elapsed {
		MyMinTime = elapsed
	}

	if ptaConfig.DEBUG {
		result.DumpAll()
	}

	if flags.DoCompare {
		_r := result.GetResult()
		_r.Queries = result.Queries
		_r.IndirectQueries = result.IndirectQueries
		return _r //bz: we only need this when comparing results/run in parallel
	}

	return nil
}

func doEachMainDefault(i int, main *ssa.Package) *default_algo.Result {
	var logfile *os.File
	var err error
	if flags.DoLog { //create my log file
		logfile, err = os.Create("/Users/bozhen/Documents/GO2/origin-go-tools/_logs/default_log_" + strconv.Itoa(i))
	} else {
		logfile = nil
	}

	if err != nil {
		panic(fmt.Sprintln(err))
	}

	var mains []*ssa.Package
	mains = append(mains, main)
	// Configure pointer analysis to build call-graph
	ptaConfig := &default_algo.Config{
		Mains:           mains, //one main per time
		Reflection:      false,
		BuildCallGraph:  true,
		Log:             logfile,
		DoPerformance:   flags.DoPerformance, //bz: I add to output performance for comparison
		DoRecordQueries: flags.DoCompare,     //bz: record all queries to compare result
	}

	//*** compute pta here
	start := time.Now() //performance
	var result *default_algo.Result
	var r_err error           //bz: result error
	if flags.TimeLimit != 0 { //we set time limit
		c := make(chan string, 1)

		// Run the pta in it's own goroutine and pass back it's
		// response into our channel.
		go func() {
			result, r_err = default_algo.Analyze(ptaConfig) // conduct pointer analysis
			c <- "done"
		}()

		// Listen on our channel AND a timeout channel - which ever happens first.
		select {
		case res := <-c:
			fmt.Println(res)
		case <-time.After(flags.TimeLimit):
			fmt.Println("\n!! Out of time (", flags.TimeLimit, "). Kill it :(")
			return nil
		}
	} else {
		result, r_err = default_algo.Analyze(ptaConfig) // conduct pointer analysis
	}
	t := time.Now()
	elapsed := t.Sub(start)
	if r_err != nil {
		panic(fmt.Sprintln(r_err))
	}
	defer logfile.Close()

	if flags.DoPerformance {
		reaches, unreaches := countReachUnreachFunctions(result)
		fmt.Println("#Unreach Nodes: ", len(unreaches))
		fmt.Println("#Reach Nodes: ", len(reaches))
		fmt.Println("#Unreach Functions: ", len(unreaches))
		fmt.Println("#Reach Functions: ", len(reaches))
	}

	fmt.Println("\nDone  -- PTA/CG Build; Using ", elapsed.String(), ".\n")

	if DefaultMaxTime < elapsed {
		DefaultMaxTime = elapsed
	}
	if DefaultMinTime > elapsed {
		DefaultMinTime = elapsed
	}

	if len(result.Warnings) > 0 {
		fmt.Println("Warning: ", len(result.Warnings)) //bz: just do not report not used var on result
	}

	return result
}

//deprecated:
//baseline: all main in parallel
//Update: fatal error: concurrent map writes!! --> change to a.track = trackall, no more panic
//go add lock @container/intsets/util.go for nodeset when doing this setting
func DoParallel(mains []*ssa.Package) map[*ssa.Package]*pointer.ResultWCtx {
	ret := make(map[*ssa.Package]*pointer.ResultWCtx) //record of result
	var _wg sync.WaitGroup
	start := time.Now()
	for i, main := range mains[0:3] {
		fmt.Println("Spawn ", i, ". ", main.String())
		_wg.Add(1)
		go func(i int, main *ssa.Package) {
			start := time.Now()
			fmt.Println("My Algo: ")
			r_my := DoEachMainMy(i, main) //mypta
			t := time.Now()
			MyElapsed = MyElapsed + t.Sub(start).Milliseconds()

			//update
			ret[main] = r_my
			_wg.Done()

			fmt.Println("Join ", i, ". ", main.String())
		}(i, main)
	}
	_wg.Wait()

	elapsed := time.Now().Sub(start)
	fmt.Println("\nDone  -- PTA/CG Build; Using " + elapsed.String() + ".\n ")

	//check
	fmt.Println("#Receive Result: ", len(ret))
	for main, result := range ret {
		fmt.Println("Receive result (#Queries: ", len(result.Queries), ", #IndirectQueries: ", len(result.IndirectQueries), ") for main: ", main.String())
	}

	return ret
}

//bz: for default algo
//    exclude root, init and main has root as caller
func countReachUnreachFunctions(result *default_algo.Result) (map[*ssa.Function]*ssa.Function, map[*ssa.Function]*ssa.Function) {
	r := result
	//fn
	reaches := make(map[*ssa.Function]*ssa.Function)
	unreaches := make(map[*ssa.Function]*ssa.Function)

	var checks []*callgraph.Edge
	//start from main and init
	root := r.CallGraph.Root
	reaches[root.Func] = root.Func
	for _, out := range root.Out {
		checks = append(checks, out)
	}

	for len(checks) > 0 {
		var tmp []*callgraph.Edge
		for _, check := range checks {
			if _, ok := reaches[check.Callee.Func]; ok {
				continue //checked already
			}

			reaches[check.Callee.Func] = check.Callee.Func
			for _, out := range check.Callee.Out {
				tmp = append(tmp, out)
			}
		}
		checks = tmp
	}

	//collect unreaches
	for _, node := range r.CallGraph.Nodes {
		if _, ok := reaches[node.Func]; !ok { //unreached
			if _, ok2 := unreaches[node.Func]; ok2 {
				continue //already stored
			}
			unreaches[node.Func] = node.Func
		}
	}

	//preGen and preFuncs are supposed to be the same with mine
	return reaches, unreaches
}
