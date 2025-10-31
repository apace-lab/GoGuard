package concurrency

import (
	"fmt"
	"github.com/bozhen-liu/gopa/flags"
	"github.com/bozhen-liu/gopa/go/ssa"
	"go/token"
	"strings"
)

// TODO:
//  1. go parameter is pass-by-value (pointer is copied and then used)
//   -> for racy pairs originated from parameter, double check whether they are pointers/references (map and slice) or not
//   -> careful about go function without parameters: has race

var (
	UNIQUE_RACE_LOC = true // true when we use inst.Pos() as key for distinguishing different races;
	// false when we use rwnode.id to distinguish, which will have many same race locations with duplicate interleavings

	USE_PTS_COMBINE = false // use pts's obj ids in ascending order as the key of sharing memory access
	// TODO: this may introduce true negatives

	MORE_WRITE = false // true when change possible reads to writes that will be identified later

	summary = make(map[string]*DataRace) // collect all races across different entries
)

// SummarizeRaces collect all races across different entries
func SummarizeRaces(entry string, races map[string]*DataRace) {
	for key, race := range races {
		if r, ok := summary[key]; ok {
			r.entries[entry] = entry
		} else {
			summary[key] = race
			race.entries = make(map[string]string)
			race.entries[entry] = entry
		}
	}
}

func PrintSummary() {
	fmt.Println("\n\n\n\n*********************************** ")
	fmt.Printf("Detection Result (Summarized Data races): # = %d\n", len(summary))
	i := 0
	for _, race := range summary {
		i++
		fmt.Println("=========================================")
		fmt.Printf("Data Race %d:\n", i)
		attr_a := ""
		if race.a.typID == NWrite {
			attr_a = "Write"
		} else {
			attr_a = "Read"
		}
		attr_b := ""
		if race.a.typID == NWrite {
			attr_b = "Write"
		} else {
			attr_b = "Read"
		}

		fmt.Printf("Statement (tid = %d, Node%d, %s%d)\n\t%s\n@Location\n\t%s\nvs.\nStatement (tid = %d, Node%d, %s%d)\n\t%s\n@Location\n\t%s\n",
			race.a.n.gid, race.a.n.id, attr_a, race.a.id, race.stmt_a, race.loc_a, race.b.n.gid, race.b.n.id, attr_b, race.b.id, race.stmt_b, race.loc_b)

		fmt.Println("entries: ")
		j := 0
		for _, e := range race.entries {
			j++
			fmt.Printf("\t%d. %s\n", j, e)
		}
	}
	fmt.Println("\n\n*********************************** ")
}

type TypeOfRWNode uint

const (
	// for detecting data race
	NRead TypeOfRWNode = iota
	NWrite
)

// RWNode a read/write node
type RWNode struct {
	n       *Node
	id      int
	typID   TypeOfRWNode
	inst    ssa.Instruction
	lockset []int // a set of lock objs TODO: distinguish rlock and lock?

	pts   []int
	field int // field index if accessing an object field
	// TODO: do the same for keys in a map or array indexes?

	//mdarr_objs []int // corresponding multi-d array pts's objs
}

func (n *RWNode) String() string {
	s := ""
	switch n.typID {
	case NRead:
		s += fmt.Sprintf("Read%d@%s (tid = %d) %s; lockset = [%d]", n.id, n.inst.String(), n.n.gid, n.n.cgnode.GetFunc().String(), n.lockset)
	case NWrite:
		s += fmt.Sprintf("Write%d@%s (tid = %d) %s; lockset = [%d]", n.id, n.inst.String(), n.n.gid, n.n.cgnode.GetFunc().String(), n.lockset)
	default:
	}
	//if len(n.mdarr_objs) > 0 {
	//	s += fmt.Sprintf(" MD Array Objs: [%d]", n.mdarr_objs)
	//}
	return s
}

type DataRace struct {
	a              *RWNode
	b              *RWNode
	pos_a          token.Pos
	pos_b          token.Pos
	stmt_b, stmt_a string
	loc_a, loc_b   token.Position

	entries map[string]string // detected this race from which entries, i.e., main or test
}

type RFGPlusGraph struct {
	g *RFGraph

	// pts's obj ids in ascending order -> < rwnode id -> the rwnode accessing this obj >
	pts2WriteNode map[string]map[int]*RWNode
	pts2ReadNode  map[string]map[int]*RWNode

	// obj -> < rwnode id -> the rwnode accessing this obj >
	obj2WriteNode map[int]map[int]*RWNode
	obj2ReadNode  map[int]map[int]*RWNode

	checked map[string]string // checked node id pairs

	size_edge int // size of rfgplus edges
	size_node int // size of rfgplus nodes

	races map[string]*DataRace // key: <small number + big number> -> value: race => check doc for UNIQUE_RACE_LOC
}

func InitializeRFGPlus() *RFGPlusGraph {
	return &RFGPlusGraph{
		pts2WriteNode: make(map[string]map[int]*RWNode),
		pts2ReadNode:  make(map[string]map[int]*RWNode),
		obj2WriteNode: make(map[int]map[int]*RWNode),
		obj2ReadNode:  make(map[int]map[int]*RWNode),
		checked:       make(map[string]string),
		races:         make(map[string]*DataRace),
	}
}

func (g *RFGPlusGraph) UpdateRecords(pts []int, node *RWNode) {
	if len(pts) == 0 {
		return
	}

	//if USE_PTS_COMBINE {
	key := fmt.Sprintf("%v", pts)
	switch node.typID {
	case NWrite:
		if _, ok := g.pts2WriteNode[key]; !ok {
			g.pts2WriteNode[key] = make(map[int]*RWNode)
		}
		g.pts2WriteNode[key][node.id] = node
	case NRead:
		if _, ok := g.pts2ReadNode[key]; !ok {
			g.pts2ReadNode[key] = make(map[int]*RWNode)
		}
		g.pts2ReadNode[key][node.id] = node
	}
	//} else {
	for _, obj := range pts { // obj actually is objID
		switch node.typID {
		case NWrite:
			if _, ok := g.obj2WriteNode[obj]; !ok {
				g.obj2WriteNode[obj] = make(map[int]*RWNode)
			}
			g.obj2WriteNode[obj][node.id] = node
		case NRead:
			if _, ok := g.obj2ReadNode[obj]; !ok {
				g.obj2ReadNode[obj] = make(map[int]*RWNode)
			}
			g.obj2ReadNode[obj][node.id] = node
		}
	}
	//}
}

// ChangeRead2Write node previously was NRead, now it's NWrite
func (g *RFGPlusGraph) ChangeRead2Write(node *RWNode) {
	if MORE_WRITE {
		pts := node.pts
		//if USE_PTS_COMBINE {
		key := fmt.Sprintf("%v", pts)
		// delete from read map
		delete(g.pts2ReadNode[key], node.id)
		// add to write map
		if _, ok := g.pts2WriteNode[key]; !ok {
			g.pts2WriteNode[key] = make(map[int]*RWNode)
		}
		g.pts2WriteNode[key][node.id] = node
		//} else {
		for _, obj := range pts {
			// delete from read map
			delete(g.obj2ReadNode[obj], node.id)
			// add to write map
			if _, ok := g.obj2WriteNode[obj]; !ok {
				g.obj2WriteNode[obj] = make(map[int]*RWNode)
			}
			g.obj2WriteNode[obj][node.id] = node
		}
		//}
	}
}

func (g *RFGPlusGraph) PrintRecords() {
	fmt.Println("\nPrinting Records: ")
	for obj, writes := range g.obj2WriteNode {
		fmt.Printf("obj = %v:\n\twrites: (# = %d)\n", obj, len(writes))
		for i, w := range writes {
			fmt.Printf("\t%d %s;\n", i, w)
		}
		if reads, ok := g.obj2ReadNode[obj]; ok {
			fmt.Printf("\n\treads: (# = %d)\n", len(reads))
			j := 0
			for _, r := range reads {
				j++
				fmt.Printf("\t%d %s;\n", j, r)
			}
		}
		fmt.Println()
	}

	fmt.Println("\n - Reads without Writes: ")
	for obj, reads := range g.obj2ReadNode {
		if _, ok := g.obj2WriteNode[obj]; !ok {
			fmt.Printf("obj = %d:\n\treads: (# = %d)\n", obj, len(reads))
			j := 0
			for _, r := range reads {
				j++
				fmt.Printf("\t%d %s;\n", j, r)
			}
		}
		fmt.Println()
	}
}

func (g *RFGPlusGraph) CreateGraph(rfg *RFGraph) {
	g.g = rfg

	fmt.Println("\n\n*********************************** ")
	fmt.Println("Creating Happens-Before Edges ... ")

	if flags.Plus {
		g.g.AddHappenBeforeEdges()
	}
}

func (g *RFGPlusGraph) Checked(a *RWNode, b *RWNode) bool {
	nkey := ""
	if a.n.id > b.n.id {
		nkey = fmt.Sprintf("%d-%d", b.n.id, a.n.id)
	} else {
		nkey = fmt.Sprintf("%d-%d", a.n.id, b.n.id)
	}
	if _, ok := g.checked[nkey]; ok {
		return true
	}
	return false
}

func (g *RFGPlusGraph) AddRace(a, b *RWNode) {
	// start
	pos_a := a.inst.Pos()
	pos_b := b.inst.Pos()

	if pos_a == token.NoPos || pos_b == token.NoPos {
		// TODO: for now, we ignore any races without a valid source code location
		return
	}

	key := ""
	if UNIQUE_RACE_LOC {
		pos_a_int := int(pos_a)
		pos_b_int := int(pos_b)
		if pos_a_int > pos_b_int {
			key = fmt.Sprintf("%d-%d", pos_b_int, pos_a_int)
		} else {
			key = fmt.Sprintf("%d-%d", pos_a_int, pos_b_int)
		}
	} else {
		if a.id > b.id {
			key = fmt.Sprintf("%d-%d", b.id, a.id)
		} else {
			key = fmt.Sprintf("%d-%d", a.id, b.id)
		}
	}

	if _, ok := g.races[key]; ok {
		return
	}

	loc_a, stmt_a := g.g.getSourceCode(pos_a)
	loc_b, stmt_b := g.g.getSourceCode(pos_b)

	// heuristic to remove FPs
	if strings.Contains(stmt_a, "err") || strings.Contains(stmt_b, "err") ||
		strings.Contains(stmt_a, "Err") || strings.Contains(stmt_b, "Err") ||
		strings.Contains(stmt_a, "error") || strings.Contains(stmt_b, "error") ||
		strings.Contains(stmt_a, "Error") || strings.Contains(stmt_b, "Error") ||
		strings.Contains(stmt_a, "logger") || strings.Contains(stmt_b, "logger") || // grpc-go
		strings.Contains(stmt_a, "plog.") || strings.Contains(stmt_b, "plog.") ||
		strings.Contains(stmt_a, "Info") || strings.Contains(stmt_b, "Info") ||
		strings.Contains(stmt_a, "Warningf") || strings.Contains(stmt_b, "Warningf") ||
		strings.Contains(stmt_a, "Panicf") || strings.Contains(stmt_b, "Panicf") { // etcd
		return // ignore these type of races, too much FPs
	}
	if strings.EqualFold(stmt_a, stmt_b) && strings.Contains(stmt_a, ":=") {
		// new definition, e.g.,
		//msg := fmt.Sprintf("trace[%d] %s", traceNum, t.operation) -> location 1
		//msg := fmt.Sprintf("trace[%d] %s", traceNum, t.operation) -> location 2
		return
	}

	// add a new race
	g.races[key] = &DataRace{
		a:      a,
		b:      b,
		pos_a:  pos_a,
		pos_b:  pos_b,
		loc_a:  loc_a,
		loc_b:  loc_b,
		stmt_a: stmt_a,
		stmt_b: stmt_b,
	}
}

func (g *RFGPlusGraph) DetectRace() {
	fmt.Println("\n\n*********************************** ")
	fmt.Println("Detecting Races")

	if USE_PTS_COMBINE {
		for obj, writes := range g.pts2WriteNode {
			wnodes := make([]*RWNode, 0, len(writes))
			for _, wnode := range writes {
				wnodes = append(wnodes, wnode)
			}

			for i := 0; i < len(wnodes); i++ {
				write := wnodes[i]
				if reads, ok := g.pts2ReadNode[obj]; ok {
					for _, read := range reads {
						if write.n.gid != read.n.gid && !g.Checked(write, read) {
							if DEBUG {
								fmt.Printf("checking race: %s vs %s ...\n", write.String(), read.String())
							}

							// lockset to see if they have common lock
							if HasCommonIntArray(write.lockset, read.lockset) {
								continue
							}
							// DFS on RFGraph to find happens-before relation
							if g.g.DFSCanReach(write, read) {
								continue
							}

							// is a race
							g.AddRace(write, read)
						}
					}
				}

				for j := i + 1; j < len(wnodes); j++ {
					awrite := wnodes[j]
					if write.n.gid != awrite.n.gid && !g.Checked(write, awrite) {
						if DEBUG {
							fmt.Printf("checking race: %s vs %s ...\n", write.String(), awrite.String())
						}
						// same as above todos
						if HasCommonIntArray(write.lockset, awrite.lockset) {
							continue
						}
						if g.g.DFSCanReach(write, awrite) {
							continue
						}

						// is a race
						g.AddRace(write, awrite)
					}
				}
			}
		}
	} else {
		for obj, writes := range g.obj2WriteNode {
			//if obj == 65099 {
			//	fmt.Println() // debug
			//}

			wnodes := make([]*RWNode, 0, len(writes))
			for _, wnode := range writes {
				wnodes = append(wnodes, wnode)
			}

			for i := 0; i < len(wnodes); i++ {
				write := wnodes[i]
				if reads, ok := g.obj2ReadNode[obj]; ok {
					for _, read := range reads {
						if write.n.gid != read.n.gid && !g.Checked(write, read) {
							if DEBUG {
								fmt.Printf("checking race: %s vs %s ...\n", write.String(), read.String())
							}

							// lockset to see if they have common lock
							if HasCommonIntArray(write.lockset, read.lockset) {
								continue
							}
							// DFS on RFGraph to find happens-before relation
							if g.g.DFSCanReach(write, read) {
								continue
							}

							// is a race
							g.AddRace(write, read)
						}
					}
				}

				for j := i + 1; j < len(wnodes); j++ {
					awrite := wnodes[j]
					if write.n.gid != awrite.n.gid && !g.Checked(write, awrite) {
						if DEBUG {
							fmt.Printf("checking race: %s vs %s ...\n", write.String(), awrite.String())
						}
						// same as above todos
						if HasCommonIntArray(write.lockset, awrite.lockset) {
							continue
						}
						if g.g.DFSCanReach(write, awrite) {
							continue
						}

						// is a race
						g.AddRace(write, awrite)
					}
				}
			}
		}
	}

	fmt.Println("\n\n*********************************** ")
	fmt.Printf("Detection Result (Data races): # = %d\n", len(g.races))
	i := 0
	for _, race := range g.races {
		i++
		fmt.Println("=========================================")
		fmt.Printf("Data Race %d:\n", i)
		attr_a := ""
		if race.a.typID == NWrite {
			attr_a = "Write"
		} else {
			attr_a = "Read"
		}
		attr_b := ""
		if race.a.typID == NWrite {
			attr_b = "Write"
		} else {
			attr_b = "Read"
		}

		fmt.Printf("Statement (tid = %d, Node%d, %s%d)\n\t%s\n@Location\n\t%s\nvs.\nStatement (tid = %d, Node%d, %s%d)\n\t%s\n@Location\n\t%s\n",
			race.a.n.gid, race.a.n.id, attr_a, race.a.id, race.stmt_a, race.loc_a, race.b.n.gid, race.b.n.id, attr_b, race.b.id, race.stmt_b, race.loc_b)
	}
	fmt.Println("\n\n*********************************** ")
}
