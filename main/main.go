package main

import (
	"fmt"
	"github.com/bozhen-liu/gopa/concurrency"
	"github.com/bozhen-liu/gopa/flags"
	"github.com/bozhen-liu/gopa/go/myutil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// _doMain if path != "": run for a folder
func _doMain(path string) {
	prog, mains, _ := myutil.InitialMain(path)
	if mains == nil {
		return
	}

	//officially start
	if flags.DoSeq { //AnalyzeMultiMains - default
		_, _results := myutil.DoSeq(mains)
		if flags.DoRace || flags.DoBlocking {
			concurrency.DetectBugs(prog, _results)
		}
		return
	} else {
		if flags.DoSameRoot {
			myutil.DoSameRoot(mains)
		} else { //default
			myutil.DoEach(mains)
		}
	}

	fmt.Println("\n\nBASELINE All Done  -- PTA/CG Build. \n")

	if flags.DoCompare || flags.DoDefault {
		fmt.Println("Default Algo:")
		fmt.Println("Total: ", (time.Duration(myutil.DefaultElapsed)*time.Millisecond).String()+".")
		fmt.Println("Max: ", myutil.DefaultMaxTime.String()+".")
		fmt.Println("Min: ", myutil.DefaultMinTime.String()+".")
		fmt.Println("Avg: ", float32(myutil.DefaultElapsed)/float32(len(mains))/float32(1000), "s.")
	}

	if flags.DoDefault {
		return
	}

	fmt.Println("My Algo:")
	fmt.Println("Total: ", (time.Duration(myutil.MyElapsed)*time.Millisecond).String()+".")
	fmt.Println("Max: ", myutil.MyMaxTime.String()+".")
	fmt.Println("Min: ", myutil.MyMinTime.String()+".")
	fmt.Println("Avg: ", float32(myutil.MyElapsed)/float32(len(mains))/float32(1000), "s.")
}

func visitFile(fp string, fi os.DirEntry, err error) error {
	if err != nil {
		fmt.Println(err) // can't walk here,
		return nil       // but continue walking elsewhere
	}
	if fi.IsDir() {
		return nil // no
		// t a file. ignore.
	}
	if strings.HasSuffix(fp, ".go") {
		fmt.Println("\n\nTesting: " + fp) // print file path
		_doMain(fp)
	}
	return nil
}

func main() {
	flags.ParseFlags()
	if flags.DoRace || flags.DoBlocking {
		concurrency.InitialPerformance()
	}

	if flags.DoFolder != "" {
		// command line: ./main -doSeq -doTests -doFolder=$path-to-your-folder
		// Specify the root folder you want to start from
		err := filepath.WalkDir(flags.DoFolder, visitFile)
		if err != nil {
			fmt.Printf("error walking the path: %v\n", err)
		}
	} else {
		// command: ./main -doSeq -doTests -appDir=$path-to/grpc-go
		// to run all tests and mains in grpc-go
		_doMain("")
	}
	if flags.DoRace || flags.DoBlocking {
		concurrency.Performance()
	}
}
