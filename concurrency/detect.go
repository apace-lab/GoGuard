package concurrency

import (
	"fmt"
	"github.com/bozhen-liu/gopa/go/myutil/flags"
	"github.com/bozhen-liu/gopa/go/pointer"
	"github.com/bozhen-liu/gopa/go/ssa"
	"go/token"
	"strings"
	"time"
)

// bz: this prototype uses gobench/gobench/goker/blocking/grpc/660/grpc660_test.go as example

var (
	build_maxTime time.Duration
	build_minTime time.Duration
	build_total   time.Duration

	detect_maxTime time.Duration
	detect_minTime time.Duration
	detect_total   time.Duration

	maxTime time.Duration
	minTime time.Duration
	total   time.Duration

	counter    int
	total_nums map[int]int // #channel 0, #lock 1, #ctx 2, #wg 3, #cond 4, #ROC 5, #select 6, #states 7, #goroutine 8

	// filtered out results
	pos2cferr map[token.Pos]*CFError // intermediate
	berrs     []*BlockError
	cferrs    []*CFError
)

func InitialPerformance() {
	build_minTime = 10000000000
	detect_minTime = 10000000000
	minTime = 10000000000
	counter = 0
	build_total = 0
	detect_total = 0
	total = 0

	total_nums = make(map[int]int)
	pos2cferr = make(map[token.Pos]*CFError)
	berrs = make([]*BlockError, 0)
	cferrs = make([]*CFError, 0)
}

func Performance() {
	fmt.Println(strings.Repeat("*", 80))
	fmt.Println(strings.Repeat("*", 80))
	if counter == 0 {
		fmt.Println("No multi-goroutine programs.")
		return
	}

	fmt.Println("Total Build: ", (build_total).String()+".")
	fmt.Println("Max Build: ", (build_maxTime).String()+".")
	fmt.Println("Min Build: ", (build_minTime).String()+".")
	fmt.Println("Avg Build: ", float32(build_total)/float32(counter), ".\n")

	fmt.Println("Total Detect: ", (detect_total).String()+".")
	fmt.Println("Max Detect: ", (detect_maxTime).String()+".")
	fmt.Println("Min Detect: ", (detect_minTime).String()+".")
	fmt.Println("Avg Detect: ", float32(detect_total)/float32(counter), ".\n")

	fmt.Println("Total: ", total.String()+".")
	fmt.Println("Max: ", (maxTime).String()+".")
	fmt.Println("Min: ", (minTime).String()+".")
	fmt.Println("Avg: ", float32(total)/float32(counter), ".")
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("Total: #states = %d #goroutine = %d #channel = %d #lock = %d #ctx = %d #wg = %d #cond = %d #ROC = %d #select = %d\n",
		total_nums[7], total_nums[8], total_nums[0], total_nums[1], total_nums[2], total_nums[3], total_nums[4], total_nums[5], total_nums[6])
	fmt.Printf("Avg  : #states = %d #goroutine = %d #channel = %d #lock = %d #ctx = %d #wg = %d #cond = %d #ROC = %d #select = %d (#entries = %d)\n",
		total_nums[7]/counter, total_nums[8]/counter, total_nums[0]/counter, total_nums[1]/counter, total_nums[2]/counter, total_nums[3]/counter,
		total_nums[4]/counter, total_nums[5]/counter, total_nums[6]/counter, counter)
	fmt.Println(strings.Repeat("*", 80))
	fmt.Println(strings.Repeat("*", 80))
}

func detectEntry(res *pointer.ResultWCtx, prog *ssa.Program, entry *pointer.Node) {
	//res.DumpAll() // debug
	rfg := InitializeRFG()
	start := time.Now()
	rfg.CreateGraph(res, prog, entry)
	if len(rfg.roots) == 1 && flags.AppPkg != "" { // skip single thread programs if not from gobench
		fmt.Println("-> Single goroutine program, return.\n")
		return
	}
	build := time.Now().Sub(start)
	start = time.Now()
	rfg.DFSDetectBlockingBugs()
	detect := time.Now().Sub(start)
	t := build + detect

	filterStates(rfg.berrs)
	filterCFErrs(rfg.cferrs)

	// statistics
	counter++
	nums := rfg.Statistics()

	build_total = build_total + build
	detect_total = detect_total + detect
	total = total + t

	if build_maxTime < build {
		build_maxTime = build
	}
	if build_minTime > build {
		build_minTime = build
	}

	if detect_maxTime < detect {
		detect_maxTime = detect
	}
	if detect_minTime > detect {
		detect_minTime = detect
	}

	if maxTime < t {
		maxTime = t
	}
	if minTime > t {
		minTime = t
	}

	total_nums[0] += nums[0]
	total_nums[1] += nums[1]
	total_nums[2] += nums[2]
	total_nums[3] += nums[3]
	total_nums[4] += nums[4]
	total_nums[5] += nums[5]
	total_nums[6] += nums[6]
	total_nums[7] += nums[7]
	total_nums[8] += nums[8]
}

func DetectBugs(prog *ssa.Program, ptResults map[*ssa.Package]*pointer.ResultWCtx) {
	for _, res := range ptResults {
		if res.IsTest {
			for _, cgnode := range res.TestEntries { // test entry
				test := res.CallGraph.Nodes[cgnode]
				detectEntry(res, prog, test)
			}
		} else { // main entry
			main := res.CallGraph.Root.Out[1].Callee
			detectEntry(res, prog, main)
		}
	}

	// print filtered results
	fmt.Println("\n\n*********************************** ")
	fmt.Println("Detection Result: ")
	for i, berr := range berrs {
		s := berr.s
		if s.noPaused() && len(s.visit_loops) == 0 { // only print out the state when blocking bugs have been detected
			continue
		}

		if s.hasMeaningfulPaused() {
			fmt.Println("=========================================")
			fmt.Printf("Detection State %d (Blocking)\n", i)
			fmt.Printf("splitting points: %v\tmatched nodes: %v\n", s.splitting_points, s.pairs)
			for j, id := range s.pause_ats {
				if id == -1 {
					// entering a loop without exit
					continue
				}

				if id != -2 && id != -3 {
					ctx := s.g.roots[j].cgnode.GetContext()
					node := s.g.id2node[id]
					loc, stmt := s.g.getLocAndStmt(node, s)
					fmt.Printf("tid = %d. Blocked@%s Thread (%s)\n-> Statement %s\n@Location %s\n\n", j, node, ctx, stmt, loc)
				}
			}
		}

		if len(s.visit_loops) > 0 {
			fmt.Println("-----------------------------------------")
			fmt.Printf("Detection State %d (No Loop Exit)\n", i)
			// TODO: it's hard to locate the loop loc
			for _, node := range s.visit_loops {
				var pos token.Pos
				var loop_name string
				switch node.typId {
				case NLoop:
					pos = node.loop_bb.Instrs[0].Pos()
					loop_name = node.loop_name
					break
				case NROC:
					pos = node.roc_bb.Instrs[0].Pos()
					loop_name = node.roc_name
					break
				}
				loc, stmt := s.g.getSourceCode(pos)
				pause_at := s.g.findWhereLoopPausedAt(node.gid, node, s) // find where loop paused at
				if pause_at == nil {
					fmt.Printf("Cannot exit the %s\n-> Statement %s\n@Location %s\n\n", loop_name, stmt, loc)
					continue
				}
				loc2, stmt2 := s.g.getLocAndStmt(pause_at, s)
				fmt.Printf("Cannot exit the %s\n-> Statement %s\n@Location %s\npaused at\n-> Statement %s\n@Location %s\n\n", loop_name, stmt, loc, stmt2, loc2)
			}
		}
	}

	i := 0
	for _, err := range pos2cferr {
		fmt.Printf("%d.\n", i)
		i++
		pos := err.inst_pos
		if err.inst_unlock_pos != token.NoPos {
			pos = err.inst_unlock_pos
		}
		loc, stmt := err.g.getSourceCode(pos)
		loc2, stmt2 := err.g.getSourceCode(err.interupt_pos)
		if stmt2 == "" {
			stmt2 = err.stmt_str
		}
		fmt.Printf("Statement %s\n@Location %s\ncannot reach the exit of function due to premature termination at:\nStatement %s\n@Location %s\n",
			stmt, loc, stmt2, loc2)
		fmt.Println("-----------------------------------------")
	}
	fmt.Println("\n\n*********************************** ")
}

// filterCFErrs remove duplicate errs
func filterCFErrs(gcferrs []*CFError) {
	for _, err := range gcferrs { // filter out redundant errs
		if _, ok := pos2cferr[err.inst_pos]; ok {
			continue
		}
		pos2cferr[err.inst_pos] = err
	}
}

// filterStates remove duplicate errs
func filterStates(gberrs []*BlockError) {
	for _, gberr := range gberrs {
		if exist(gberr) {
			continue
		}
		berrs = append(berrs, gberr)
	}
}

// only for filterStates
func exist(gberr *BlockError) bool {
	for _, berr := range berrs {
		if mapsEqual(berr.tid2loc, gberr.tid2loc) {
			return true
		}
	}
	return false
}

// mapsEqual checks if two map[int]string have the same elements.
func mapsEqual(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false // Different number of elements means they are not equal.
	}

	for key, aValue := range a {
		if bValue, ok := b[key]; !ok || aValue != bValue {
			return false // Key does not exist in b, or values do not match.
		}
	}

	return true // Maps are equal.
}
