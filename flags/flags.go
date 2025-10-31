package flags

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var Parsed = false // whether we already parsed the flags for this run

//user
var DoLog = false
var Main = ""               //bz: run for a specific main in this pkg; start from 0
var DoDefault = false       //bz: only Do default
var DoCompare = false       //bz: this has a super long time
var DoTests = false         //bz: treat a test as a main to analyze
var DoCoverage = false      //bz: compute (#analyzed fn/#total fn) in a program within the scope
var TimeLimit time.Duration //bz: time limit set by users, unit: ?h?m?s
var AppDir string           //bz: user specify application absolute path -> we run analysis here
var AppPkg string           //bz: application package

var PTSLimit int   //bz: limit the size of pts; if excess, skip its solving
var DoDiff = false //bz: compute the diff functions when turn on/off ptsLimit

var DoRace = false     // bz: do race detection
var DoBlocking = false // bz: do blocking bug detection
// bz: the range of packages to be analyzed for big projects, e.g., 1-9, include 1 and 9
var (
	RangeL = -1
	RangeR = -1
)
var Plus = false // bz: whether consider all synchronization primitives when detecting races

//my use
var PrintCGNodes = false  //bz: print #cgnodes (before solve())
var DoPerformance = false //bz: print out all statistics (time, number)
var DoDetail = false      //bz: print out all data from countReachUnreachXXX
var DoCommonPart = false  //bz: do compute common path

//different run scenario
var DoSameRoot = false //bz: do all main in a pkg together from the same root -> all mains linked by the root node
var DoSeq = true       //bz: do all mains in a pkg sequential, but input is multiple mains (test useage in race checker)
var DoFolder = ""      //bz: do all mains/tests in a folder, by walking all .go files recursively

// Function to parse the string and return two integers
func parseRange(rangeStr string) (int, int, error) {
	parts := strings.Split(rangeStr, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid format: %s", rangeStr)
	}

	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid number: %s", parts[0])
	}

	end, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid number: %s", parts[1])
	}

	return start, end, nil
}

// ParseFlags bz: analyze all flags from input
func ParseFlags() {
	if Parsed { // only parse once
		return
	}
	Parsed = true

	//user
	_main := flag.String("main", "", "Run for a specific main in this pkg.")
	_doLog := flag.Bool("doLog", false, "Do log. ")
	_doDefault := flag.Bool("doDefault", false, "Do default algo only. ")
	_doComp := flag.Bool("doCompare", false, "Do compare with default pta. ")
	_time := flag.String("timeLimit", "", "Set time limit to ?h?m?s or ?m?s or ?s, e.g. 1h15m30.918273645s. ")
	_doTests := flag.Bool("doTests", false, "Treat a test as a main to analyze. ")
	_pts := flag.Int("ptsLimit", 0, "Set a number to limit the size of pts during the solver, e.g. 999. ")
	_appDir := flag.String("appDir", "", "Specify application absolute path.")
	_appPkg := flag.String("appPkg", "", "Specify application package, e.g., google.golang.org/grpc.")

	_doRace := flag.Bool("doRace", false, "Do race detection.")
	_doBlocking := flag.Bool("doBlocking", false, "Do blocking bug detection.")
	_pkgRange := flag.String("doPkgRange", "", "Specify the range of pkgs to be analyzed, e.g., 1-9 (include 1 and 9).")
	_plus := flag.Bool("RFGPlus", false, "Use RFGPlus.")

	//my use
	_printCGNodes := flag.Bool("printCGNodes", false, "Print #cgnodes (before solve()).")
	_doSameRoot := flag.Bool("doSameRoot", false, "Do all main together from the same root in one pkg, linked by the root node.")
	_doCoverage := flag.Bool("doCoverage", false, "Compute (#analyzed fn/#total fn) in a program")

	//test useage in race checker -> main usage
	_doSeq := flag.Bool("doSeq", false, "Do all mains in a pkg sequential, but input is multiple mains.")
	_doFolder := flag.String("doFolder", "", "Do all mains in a folder sequential, but input is multiple mains.")

	flag.Parse()
	if *_main != "" {
		Main = *_main
	}
	if *_doLog {
		DoLog = true
	}
	if *_doDefault {
		DoDefault = true
	}
	if *_doComp {
		DoCompare = true
	}
	if *_time != "" {
		TimeLimit, _ = time.ParseDuration(*_time)
	}
	if *_doTests {
		DoTests = true
	}
	if *_doCoverage {
		DoCoverage = true
	}
	if *_pts != 0 {
		PTSLimit = *_pts
		//DoDiff = true
	}
	if *_appDir != "" {
		AppDir = *_appDir
	}
	if *_appPkg != "" {
		AppPkg = *_appPkg
	}

	if *_doRace {
		DoRace = true
	}
	if *_doBlocking {
		DoBlocking = true
	}
	if *_pkgRange != "" {
		var err error
		RangeL, RangeR, err = parseRange(*_pkgRange)
		if err != nil {
			panic(fmt.Sprintf("Error parsing pkg range '%s': %s\n", *_pkgRange, err))
		}
	}
	if *_plus {
		Plus = true
	}

	//my use
	if *_printCGNodes {
		PrintCGNodes = true
	}
	if *_doSameRoot {
		DoSameRoot = true
	}

	//test useage in race checker
	if *_doSeq {
		DoSeq = true
	}
	if *_doFolder != "" {
		DoFolder = *_doFolder
	}
}
