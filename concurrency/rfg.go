package concurrency

import (
	"bufio"
	"fmt"
	"github.com/bozhen-liu/gopa/go/myutil/flags"
	"github.com/bozhen-liu/gopa/go/pointer"
	"github.com/bozhen-liu/gopa/go/ssa"
	"go/token"
	"go/types"
	"math/rand"
	"os"
	"strings"
	"time"
)

// bz: Resource Flow Graph Implementation

var (
	DEBUG             = false // print out a large amount of info
	DEBUG_EACH_RESULT = false // print out result for each detection right after its detection

	ID                 = 0                        // increment for id of the node in rfg
	EXPLORE_ALL_STATES = true                     // split and explore all states
	limit              = 3                        // see g.visited
	RANDOM_STATE       = true                     // use
	scopePkg           = "command-line-arguments" // default value
)

// considerFns go programs always use these functions to set timeout or inject user-defined functions with channels
var considerFns = []string{
	"io.ReadFull",
	"io.ReadAtLeast",
	"context.WithCancel$1",
}

// inScopePkg return true if pkg starts with scopePkg
// TODO: adjust inScopePkg all the time
func inScopePkg(fn *ssa.Function) bool {
	for _, f := range considerFns {
		if fn.String() == f {
			return true
		}
	}

	var pkg string
	if _, ok := fn.Object().(*types.Func); ok {
		if fn.Object().Pkg() == nil {
			return false
		}
		pkg = fn.Object().Pkg().String()
	} else if fn.Pkg != nil {
		pkg = fn.Pkg.String()
	} else {
		return false
	}
	return strings.Contains(pkg, scopePkg)
}

// TypeOfNode Define a custom type for Node
type TypeOfNode uint

// Define constants of the custom type
const (
	NGoroutine TypeOfNode = iota
	NChannel
	NSelect
	NCase
	NCaseDone // for pretty printing-out info, which has ctx.Done() in case
	NReturn
	NLoop
	NROC
	NClose

	NLock
	NUnlock
	NRLock
	NRUnlock

	NFunc
	NExit

	// for context
	NCancel
	NCtxDone

	// for waitgroup
	NWGWait
	NWGDone

	// for cond
	NCONDWait
	NSignal
	NBroadcast
)

// Node goroutine node
type Node struct {
	// for all types of nodes
	id        int           // id of this node; if it is exit loop/select/ROC, match with its enter loop/select/ROC id
	typId     TypeOfNode    // type id of TypeOfNode
	cgnode    *pointer.Node // the cgnode where inst belongs to
	parent_id int           // parent node's id
	gid       int           // tid of this node

	// for goroutine node: reuse gid
	g_name string  // thread name
	g_inst *ssa.Go // instruction that creates a goroutine

	// for channel
	ch_name      string    // channel name if exists or ssa.Value?
	ch_send_inst *ssa.Send // sender inst
	ch_rec_inst  *ssa.UnOp // receiver inst
	ch_ptr       ssa.Value // the channel
	isSender     bool      // this is a sending channel op

	// for channel close, reuse ch_name and ch_ptr from above
	ch_close_inst ssa.Instruction // can be ssa.Defer or ssa.Call

	// for loop
	loop_name string          // name of this loop = "loc@bb idx" (use the bb idx with for.body) TODO: use its loc, e.g., pos@cgnode?
	loop_bb   *ssa.BasicBlock // the bb entering a loop
	enterLoop bool            // true if this is entering this loop

	// for roc
	roc_name string
	roc_bb   *ssa.BasicBlock
	enterROC bool
	roc_ch   *ssa.Value // the receiving channel

	// for select
	sel_name   string      // name of the select and its cases or ssa.Value?
	sel_inst   *ssa.Select // instruction of this select
	cases      []*Node     // all cases of this select
	is_in_loop bool        // whether this select is in a loop

	// for select case: reuse isSender and ch_ptr
	case_ch_name string           // name of the case
	case_sel     *Node            // the parent select Node of this "select.body"
	case_to_end  bool             // whether this case leads to exit of loop only when it's in a loop
	case_state   *ssa.SelectState // state from select, will be nil if default
	default_inst ssa.Instruction  // the 1st instruction of default block,

	// for function call
	fn_name string // callee name

	// for lock/rlock
	lock_name string
	lock_inst ssa.Instruction
	lock_ptr  ssa.Value // the lock

	// for unlock/runlock
	unlock_name string
	unlock_inst ssa.Instruction

	// for context.WithCancel (if in case, reuse case_state)
	ctx_name    string           // name format: loc of the call to context.WithCancel = bb idx @ fn name
	cancel_inst *ssa.Instruction // can be *ssa.Call or *ssa.Go
	done_inst   *ssa.Call        // Done()
	p           *Pair
	is_cancel   bool // true if is cancel()

	// for waitgroup
	wg_ptr       ssa.Value       // the ptr of waitgroup; for Wait()
	wg_pts       []int           // the objs of waitgroup; for exit, reuse this ptr
	wait_inst    *ssa.Call       // Wait()
	wg_done_inst ssa.Instruction // Done(): call or defer
	seen_done    bool            // NOTE: used in detection, mark true when seen this wg.Done()

	// for cond: reuse wait_inst
	cond_ptr       ssa.Value
	signal_inst    *ssa.Call
	broadcast_inst *ssa.Call

	// for defer
	from_defer bool

	// for return
	ret_inst *ssa.Return

	// for copy
	is_copy bool

	// others
	select_node_id int // the select_node_id if this node is from a select case, default = 0
}

func copyDeferNodes(defer_nodes []*Node, g *RFGraph) []*Node {
	ret := make([]*Node, len(defer_nodes))
	for i, d := range defer_nodes {
		ret[i] = d.Copy(g)
	}
	return ret
}

// Copy copy a node for defer
func (n *Node) Copy(g *RFGraph) *Node {
	cop := &Node{
		id:         ID,
		typId:      n.typId,
		cgnode:     n.cgnode,
		parent_id:  n.parent_id,
		gid:        n.gid,
		from_defer: n.from_defer,
		is_copy:    true,
	}

	switch n.typId {
	case NUnlock, NRUnlock:
		cop.unlock_inst = n.unlock_inst
		cop.lock_ptr = n.lock_ptr
		g.updateNodes(nil, cop, false)
		g.UpdateResources(cop.lock_ptr, cop.cgnode, cop)
		break

	case NClose:
		cop.ch_name = n.ch_name
		cop.ch_close_inst = n.ch_close_inst
		cop.ch_ptr = n.ch_ptr
		g.updateNodes(nil, cop, true)
		g.UpdateResources(cop.ch_ptr, cop.cgnode, cop)
		break

	default:
		fmt.Println("TODO: unimplement copy type.")
	}

	return cop
}

// String format = Node + id + (type of Node): name
func (n *Node) String() string {
	s := fmt.Sprintf("Node%d ", n.id)
	switch n.typId {
	case NLock:
		s += "(Lock): @" + n.lock_ptr.String()
	case NUnlock:
		s += "(Unlock): @" + n.lock_ptr.String()
	case NRLock:
		s += "(RLock): @" + n.lock_ptr.String()
	case NRUnlock:
		s += "(RUnlock): @" + n.lock_ptr.String()
	case NGoroutine:
		s += "(thread): " + fmt.Sprintf(" (tid = %d) ", n.gid) + n.g_name
	case NChannel:
		var prop string
		if n.isSender {
			prop = "isSender"
		} else {
			prop = "isReceiver"
		}
		s += "(channel): " + n.ch_name + " " + prop
	case NClose:
		s += "(close): " + n.ch_name
	case NSelect:
		//s += fmt.Sprintf("(select): %s Cases (# = %d): [", n.sel_name, len(n.cases))
		//for i, c := range n.cases {
		//	if c == nil {
		//		s += fmt.Sprintf("%d empty body;", i)
		//		continue
		//	}
		//	s += fmt.Sprintf("%d %s; ", i, c.String())
		//}
		//s += "]"
		s += fmt.Sprintf("(select): %s Cases (# = %d)", n.sel_name, len(n.cases))
	case NCase:
		if n.case_state == nil {
			s += "(case): " + n.case_ch_name
			break
		}
		var prop string
		if n.isSender {
			prop = "isSender"
		} else {
			prop = "isReceiver"
		}
		s += "(case): " + n.case_ch_name + " " + prop
	case NFunc:
		s += "(func): " + n.fn_name
	case NReturn:
		s += "(return)"
	case NLoop:
		var prop string
		if n.enterLoop {
			prop = "isEnter"
		} else {
			prop = "isExit"
		}
		s += "(loop): " + n.loop_name + " " + prop
	case NROC:
		var prop string
		if n.enterROC {
			prop = "isEnter"
		} else {
			prop = "isExit"
		}
		s += "(roc): " + n.roc_name + " " + prop
	case NCancel:
		s += "(ctx.cancel): @obj" + fmt.Sprintf("%d", n.p.base_obj)
	case NCtxDone:
		s += "(ctx.done): @obj" + fmt.Sprintf("%d", n.p.base_obj)
	case NCaseDone:
		var prop string
		if n.isSender {
			prop = "isSender"
		} else {
			prop = "isReceiver"
		}
		s += "(case)+(ctx.done): @obj" + fmt.Sprintf("%d", n.p.base_obj) + n.case_ch_name + " " + prop
	case NWGWait:
		s += "(wg.wait): @" + n.wait_inst.String()
	case NWGDone:
		s += "(wg.done): @" + n.wg_done_inst.String()
	case NExit:
		s += "(exit)"
	case NCONDWait:
		s += "(cond.wait): @" + n.cond_ptr.String()
	case NSignal:
		s += "(signal): @" + n.cond_ptr.String()
	case NBroadcast:
		s += "(broadcast): @" + n.cond_ptr.String()
	default:
	}
	if n.from_defer {
		s += " (defer)"
	}
	if n.is_copy {
		s += " (copy)"
	}
	return s
}

// Pair for context
type Pair struct {
	loc      string // loc of the call to context.WithCancel = bb idx @ fn name
	ctx_inst *ssa.Call
	base_obj int // this is ctx obj invoking a Done() functions
}

// ProducerConsumer obviously
type ProducerConsumer struct {
	producers []*Node
	consumers []*Node
}

type BlockError struct {
	tid2loc map[int]string // tid to program loc, to check redundancy
	s       *detectionState
}

type RFGraph struct {
	pa   *pointer.ResultWCtx
	cg   *pointer.GraphWCtx
	prog *ssa.Program

	roots   []*Node           // thread start node, including main
	adjList map[*Node][]*Node // parent -> kids
	id2node map[int]*Node

	// for graph creation
	visited map[string]int // visit 2 times of the same function for each thread from the same call site
	// string is signature = call.String()@fn.String()@gid
	contexts map[int]*Pair // the return value map by +3 for context.WithCancel calls: ctx obj as key
	alias    map[int]int   // manually alias:
	// (1) due to t5 = (*sync.RWMutex).RLocker(t4),
	// which cast an instance of sync.RWMutex (t4) to sync.Locker (t5), however,
	// t4.RLock() == t5.Lock() and pta cannot create the alias,
	// a map = <t5, t4>
	// (2) due to a make channel has different pts when used in make closure and local
	// t1 = new chan struct{} (done)     *chan struct{} -> used in closure
	// t2 = make chan struct{} 0:int     chan struct{}  -> used locally
	// *t1 = t2
	// a map = <t1, t2>
	tid2end map[int]*Node // for wg.Wait(): which thread match NExit or NWGDone

	// for detection
	resources  map[int]*ProducerConsumer // resources to its producers and consumers
	states     []*detectionState         // summary of all explored states
	tid2wg_pts map[int][]int             // this goroutine calls wg.Done() and need to update at the exit
	cferrs     []*CFError                // for store control flow error result
	berrs      []*BlockError             // for store blocking error result
}

// Statistics reports #states #goroutine #channel #lock #ctx #wg #cond #ROC #select
func (g *RFGraph) Statistics() map[int]int {
	nums := make(map[int]int) // #channel 0, #lock 1, #ctx 2, #wg 3, #cond 4, #ROC 5, #select 6
	for _, n := range g.id2node {
		switch n.typId {
		case NChannel, NCase, NCaseDone:
			nums[0]++
		case NLock, NRLock:
			nums[1]++
		case NCancel:
			nums[2]++
		case NWGWait:
			nums[3]++
		case NSignal, NBroadcast:
			nums[4]++
		case NROC:
			nums[5]++
		case NSelect:
			nums[6]++
		}
	}

	fmt.Println("\n\n***********************************\nRFG Statistics")
	fmt.Printf("#states = %d #goroutine = %d #channel = %d #lock = %d #ctx = %d #wg = %d #cond = %d #ROC = %d #select = %d\n",
		len(g.states), len(g.roots), nums[0], nums[1], nums[2], nums[3], nums[4], nums[5], nums[6])
	fmt.Println("***********************************")
	nums[7] = len(g.states)
	nums[8] = len(g.roots)

	return nums
}

func InitializeRFG() *RFGraph {
	ID = 0 // reset
	if flags.AppPkg != "" {
		scopePkg = flags.AppPkg
	}

	return &RFGraph{
		adjList:    make(map[*Node][]*Node),
		id2node:    make(map[int]*Node),
		visited:    make(map[string]int),
		contexts:   make(map[int]*Pair),
		alias:      make(map[int]int),
		tid2end:    make(map[int]*Node),
		resources:  make(map[int]*ProducerConsumer),
		tid2wg_pts: make(map[int][]int),
		cferrs:     make([]*CFError, 0),
		berrs:      make([]*BlockError, 0),
	}
}

// updateNodes update id2node, append to adjList then increment ID
func (g *RFGraph) updateNodes(prev_node, cur_node *Node, skip_append bool) {
	if DEBUG {
		if prev_node == nil {
			fmt.Printf("-> created="+cur_node.String()+"\tis_skip=%v\n", skip_append)
		} else {
			fmt.Printf("-> created="+cur_node.String()+"\tprev="+prev_node.String()+"\tis_skip=%v\n", skip_append)
		}
	}

	g.id2node[ID] = cur_node
	ID++
	if skip_append {
		return
	}
	g.adjList[prev_node] = append(g.adjList[prev_node], cur_node)
}

// getPTS get pts from pts result with ctx
func (g *RFGraph) getPTS(key ssa.Value, cgnode *pointer.Node) []int {
	ptr := g.pa.PointsToByContext(key, cgnode.GetContext())
	pts := ptr.PointsTo().GetPTS() // pts = {obj, ... }
	// replace with alias
	for i, obj := range pts {
		if base, ok := g.alias[obj]; ok {
			pts[i] = base // see alias
		}
	}
	return pts
}

// UpdateResources during rfg creation
func (g *RFGraph) UpdateResources(key ssa.Value, cgnode *pointer.Node, node *Node) {
	// find pts
	pts := g.getPTS(key, cgnode)
	// updateNodes
	for _, obj := range pts { // obj actually is objID
		if base, ok := g.alias[obj]; ok { // due to locker
			g.UpdateResourcesInner(base, node)
			continue
		}
		g.UpdateResourcesInner(obj, node)
	}
}

func (g *RFGraph) UpdateResourcesInner(obj int, node *Node) {
	var pcs *ProducerConsumer
	var ok bool
	if pcs, ok = g.resources[obj]; !ok { // no such entry for obj
		pcs = &ProducerConsumer{
			producers: make([]*Node, 0),
			consumers: make([]*Node, 0),
		}
	}
	if node.isSender || node.is_cancel ||
		node.typId == NUnlock || node.typId == NRUnlock || node.typId == NClose ||
		node.typId == NExit || node.typId == NWGDone ||
		node.typId == NSignal || node.typId == NBroadcast {
		pcs.producers = append(pcs.producers, node)
	} else {
		pcs.consumers = append(pcs.consumers, node)
	}
	g.resources[obj] = pcs
}

// updateChannelPTS due to pta, we have to handle some alias here:
// Note the pts of a make channel will be different from its pointer type of var used in a closure and local,
// e.g., moby30408
//
//	 t1 = new chan struct{} (done)     *chan struct{} -> used in closure
//		t2 = make chan struct{} 0:int     chan struct{}  -> used locally
//	 *t1 = t2
//
// pta cannot capture this ... have to capture here:
func (g *RFGraph) updateChannelPTS(ch ssa.Value, cur_cgnode *pointer.Node) {
	pts := g.getPTS(ch, cur_cgnode)
	for _, obj := range pts {
		base_obj := g.pa.GetPTSByObjID(obj)
		if len(base_obj) == 0 {
			continue // obj is not a pointer type
		}
		g.alias[obj] = base_obj[0] // TODO: assume 1 obj in pts
	}
}

// getChannelPtrForValue(AndUpdateChannelPTS) due to different channel types in *ssa.XXX
func (g *RFGraph) getChannelPtrForValue(val ssa.Value) ssa.Value {
	var ch ssa.Value
	switch ext := val.(type) {
	case *ssa.UnOp:
		ch = ext
	case *ssa.Call, *ssa.MakeChan:
		ch = ext
	case *ssa.Parameter:
		ch = ext
	default:
		// TODO: not sure about the correctness of ptr for the following inst
		if DEBUG {
			fmt.Println("TODO: implement non other channel case: " + ext.String())
		}
		ch = ext
	}
	return ch
}

// getChannelPtrForInst(AndUpdateChannelPTS) due to different channel types in *ssa.XXX
func (g *RFGraph) getChannelPtrForInst(inst *ssa.Send) ssa.Value {
	var ch ssa.Value
	if _, ok := inst.Chan.(*ssa.Extract); ok {
		ch = inst.X
	} else {
		ch = g.getChannelPtrForValue(inst.Chan)
	}
	return ch
}

// DFSBB creating graph for a bb
func (g *RFGraph) DFSBB(bb *ssa.BasicBlock, parent_node, prev_node *Node, h *creatorHelper) *Node {
	if h.skips[bb] || bb.Comment == "recover" || onlyDeferReturn(bb) { // handled
		return prev_node
	}
	cur_node := prev_node
	if bb.Comment == "for.body" || bb.Comment == "rangeindex.loop" {
		cur_node = &Node{
			id:        ID,
			typId:     NLoop,
			cgnode:    h.cur_cgnode,
			parent_id: parent_node.id,
			gid:       h.cur_tid,
			loop_name: "loop@" + bb.String() + h.cur_cgnode.GetFunc().String(),
			loop_bb:   bb,
			enterLoop: true,
		}

		// updateNodes
		g.updateNodes(prev_node, cur_node, false)
		h.is_in_loop = true
		h.loop_name = cur_node.loop_name
		prev_node = cur_node

	} else if bb.Comment == "rangechan.loop" {
		cur_node = &Node{
			id:        ID,
			typId:     NROC,
			cgnode:    h.cur_cgnode,
			parent_id: parent_node.id,
			gid:       h.cur_tid,
			roc_name:  "roc@" + bb.String() + h.cur_cgnode.GetFunc().String(),
			roc_bb:    bb,
			enterROC:  true,
		}

		// updateNodes
		g.updateNodes(prev_node, cur_node, false)
		h.is_in_roc = true
		h.roc_name = cur_node.roc_name
		prev_node = cur_node

	}

	// when returning a receive-only channel
	returnChannel := func(inst *ssa.UnOp, j int) bool {
		val, ok := bb.Instrs[j+1].(*ssa.Return)
		if ok && len(val.Results) > 0 && val.Results[0].Name() == inst.Name() {
			// e.g., etcd6857
			// 	t3 = <-t0     Status
			//	return t3
			return true
		}
		return false
	}

	// checkCtxCancel handle if this inst is a cancel function from context
	checkCtxCancel := func(inst ssa.Instruction, call ssa.CallCommon, prev_node *Node) bool {
		if call.Value.Type().String() == "context.CancelFunc" {
			// this is cancel() function from context
			cancel_pts := g.getPTS(call.Value, h.cur_cgnode)
			if len(cancel_pts) == 0 {
				panic("Empty pts for context.cancel(). Check.")
			}
			// TODO: we assume each set of context calls only produce one obj (and its +3)
			//      cancel_pts here may include many objs which are from the parent contexts, e.g., grpc862
			//      heuristic: take the largest number one, which should be from the smallest kid cancel, aka, this cancel
			//        obj := cancel_pts[len(cancel_pts)-1]
			for _, obj := range cancel_pts {
				if p, ok := g.contexts[obj-3]; ok {
					// this is the matched context
					cur_node = &Node{
						id:          ID,
						typId:       NCancel,
						cgnode:      h.cur_cgnode,
						parent_id:   parent_node.id,
						gid:         h.cur_tid,
						ctx_name:    p.loc,
						cancel_inst: &inst,
						p:           p,
						is_cancel:   true, // sender
					}
					g.updateNodes(prev_node, cur_node, false)
					g.UpdateResourcesInner(obj-3, cur_node)
					prev_node = cur_node
				}
			}
			return true
		}
		return false
	}

	// checkCtxDone handle if this inst is a Done function from context
	checkCtxDone := func(j int) bool {
		if j < 1 {
			return false
		}
		if call, ok := isCtxDone(bb.Instrs[j-1]); ok {
			// This receiver is how context.Done() is called here
			//  t0 = invoke ctx.Done()            <-chan struct{}
			//  t1 = <-t0
			done_pts := g.getPTS(call.Call.Value, h.cur_cgnode)
			for _, obj := range done_pts {
				if p, ok2 := g.contexts[obj]; ok2 {
					// this is the matched context
					d := &Node{
						id:        ID,
						typId:     NCtxDone,
						cgnode:    h.cur_cgnode,
						parent_id: parent_node.id,
						gid:       h.cur_tid,
						ctx_name:  p.loc,
						done_inst: call,
						p:         p,
						is_cancel: false, // sender
					}
					g.updateNodes(prev_node, d, false)
					g.UpdateResourcesInner(obj, d)
				}
			}
			return true
		}
		return false
	}

	// for normal calls
	// inst == nil when called by defer
	traverseCallee := func(inst *ssa.Call, site *ssa.CallCommon) {
		callees := h.cur_cgnode.GetCalleeByCommon(site)
		if DEBUG && len(callees) > 1 {
			fmt.Printf("!!!! %s has multiple callees %s\n", h.cur_cgnode, callees)
		}
		if len(callees) == 0 {
			return
		}
		for _, callee := range callees {
			if g.doVisit(inst, callee, h) {
				if inst != nil && callee.GetFunc().String() == "context.WithCancel$1" { // e.g., kubernetes25331
					checkCtxCancel(inst, inst.Call, prev_node)
					continue
				}
				_, cur_node = g.DFS(callee, prev_node, inst, false)
				prev_node = cur_node
			}
		}
	}

	createChannelClose := func(site *ssa.CallCommon, inst ssa.Instruction) *Node {
		ch := g.getChannelPtrForValue(site.Args[0])
		c := &Node{
			id:            ID,
			typId:         NClose,
			cgnode:        h.cur_cgnode,
			parent_id:     parent_node.id,
			gid:           h.cur_tid,
			ch_name:       ch.String(),
			ch_close_inst: inst,
			ch_ptr:        ch,
		}
		g.UpdateResources(ch, h.cur_cgnode, c)
		return c
	}

	createLockNode := func(site *ssa.CallCommon, inst ssa.Instruction, typ TypeOfNode, skip_append bool) *Node {
		h.has_lock = true
		var ptr ssa.Value
		if len(site.Args) == 1 { // is of form (*sync.RWMutex).Lock(t2)
			ptr = site.Args[0]
		} else { // is abstract invoke and should create a NRLock
			ptr = site.Value
			typ = NRLock
		}
		lock := &Node{
			id:        ID,
			typId:     typ,
			cgnode:    h.cur_cgnode,
			parent_id: parent_node.id,
			gid:       h.cur_tid,
			lock_inst: inst,
			lock_ptr:  ptr,
		}
		g.updateNodes(prev_node, lock, skip_append)
		g.UpdateResources(ptr, h.cur_cgnode, lock)
		return lock
	}

	createUnlockNode := func(site *ssa.CallCommon, inst ssa.Instruction, typ TypeOfNode, skip_append bool) *Node {
		var ptr ssa.Value
		if len(site.Args) == 1 { // is of form (*sync.RWMutex).Lock(t2)
			ptr = site.Args[0]
		} else { // is abstract invoke and should create a NRLock
			ptr = site.Value
			typ = NRUnlock
		}
		unlock := &Node{
			id:          ID,
			typId:       typ,
			cgnode:      h.cur_cgnode,
			parent_id:   parent_node.id,
			gid:         h.cur_tid,
			unlock_inst: inst,
			lock_ptr:    ptr,
		}
		g.updateNodes(prev_node, unlock, skip_append)
		g.UpdateResources(ptr, h.cur_cgnode, unlock)
		return unlock
	}

	// when there is no defer
	updateWGDone := func(site *ssa.CallCommon, inst ssa.Instruction) *Node {
		ptr := site.Args[0]
		pts := g.getPTS(ptr, h.cur_cgnode) // get pts here, otherwise wrong pointer
		g.tid2wg_pts[h.cur_tid] = pts

		done_node := &Node{
			id:           ID,
			typId:        NWGDone,
			cgnode:       h.cur_cgnode,
			parent_id:    parent_node.id,
			gid:          h.cur_tid,
			wg_done_inst: inst,
			wg_pts:       pts,
		}
		g.updateNodes(prev_node, done_node, false)
		g.tid2end[h.cur_tid] = done_node

		// update resource
		for _, obj := range pts {
			g.UpdateResourcesInner(obj, done_node)
		}
		return done_node
	}

	// when defer wg.Done()
	updateWGExit := func(tid int, exit *Node) {
		if g.tid2end[tid] != nil {
			return // already handled by updateWGDone
		}

		if wg_pts, ok := g.tid2wg_pts[tid]; ok {
			// update resource if this goroutine is returned with a call to wg.Done()
			for _, obj := range wg_pts {
				g.UpdateResourcesInner(obj, exit)
			}
			exit.wg_pts = wg_pts
			exit.gid = tid
		}
	}

	for j, instr := range bb.Instrs {
		if DEBUG {
			fmt.Println("inst = " + instr.String())
		}

		switch inst := instr.(type) {
		case *ssa.Go: // TODO: recursive creation for the same go site, currently we don't consider such cases
			// we dont check scope here
			site := inst.Common()
			callees := h.cur_cgnode.GetCalleeByCommon(site)

			if h.is_in_loop { // has 2 callees when in loop
				for _, callee := range callees {
					if !g.doVisit(inst, callee, h) {
						break
					}
					g_parent_node, last_node := g.DFS(callee, prev_node, inst, true)
					checkCtxCancel(inst, inst.Call, last_node) // this might be context.cancel()
					exit := &Node{
						id:        ID,
						typId:     NExit,
						cgnode:    h.cur_cgnode,
						parent_id: parent_node.id,
						gid:       last_node.gid,
					}
					g.updateNodes(last_node, exit, false)
					updateWGExit(g_parent_node.gid, exit)
				}
				break
			}

			// go is not in a loop
			if len(callees) == 0 {
				break
			} else if DEBUG && len(callees) > 1 {
				fmt.Printf("More than 1 callees: # = %d\n", len(callees))
			}
			for _, callee := range callees {
				if !g.doVisit(inst, callee, h) {
					break
				}

				g_parent_node, last_node := g.DFS(callee, prev_node, inst, true)
				checkCtxCancel(inst, inst.Call, last_node) // this might be context.cancel()
				if last_node.is_in_loop {
					// TODO: link the exit to the loop exit node e.g., cockroach2448
				}
				exit := &Node{
					id:        ID,
					typId:     NExit,
					cgnode:    h.cur_cgnode,
					parent_id: parent_node.id,
					gid:       last_node.gid,
				}
				g.updateNodes(last_node, exit, false)
				updateWGExit(g_parent_node.gid, exit)
			}
			break

		case *ssa.RunDefers: // indicates the location to run defer, but has no instruction included
			// In Go, the defer statements are executed in a last-in, first-out (LIFO) order.
			// Note: we may have multiple rundefer in one function, and then the deferred nodes make a loop
			// e.g., cockroach7504, (cockroach10214)
			// we copy the defer node each time we see rundefer to avoid circular dependency
			copies := copyDeferNodes(h.defer_nodes, g)
			if len(copies) > 0 {
				for i := len(copies) - 1; i >= 0; i-- {
					defer_node := copies[i]
					if prev_node == defer_node { // recursion, e.g., grpc3017
						continue
					}
					g.adjList[prev_node] = append(g.adjList[prev_node], defer_node)
					prev_node = defer_node
				}
			}
			if len(h.defer_calls) > 0 {
				// visit the fn here
				for i := len(h.defer_calls) - 1; i >= 0; i-- {
					defer_call := h.defer_calls[i]
					traverseCallee(nil, defer_call)
				}
			}
			break

		case *ssa.Defer:
			// make a node, append at the end
			name := inst.Call.Value.Name()
			if inst.Call.Method != nil { // is abstract invoke
				name = inst.Call.Method.Name()
			}
			switch name {
			case "Unlock":
				unlock := createUnlockNode(&inst.Call, inst, NUnlock, true)
				unlock.from_defer = true
				h.defer_nodes = append(h.defer_nodes, unlock)
				break

			case "RUnlock":
				unlock := createUnlockNode(&inst.Call, inst, NRUnlock, true)
				unlock.from_defer = true
				h.defer_nodes = append(h.defer_nodes, unlock)
				break

			case "close":
				if inst.Call.Value.String() == "builtin close" {
					c := createChannelClose(&inst.Call, inst)
					g.updateNodes(prev_node, c, true)
					h.defer_nodes = append(h.defer_nodes, c)
				} else {
					h.defer_calls = append(h.defer_calls, &inst.Call)
				}
				break

			case "Done":
				if strings.HasPrefix(inst.Call.String(), "(*sync.WaitGroup).Done") { // from waitgroup.Done()
					// handled by updateWGExit
				} else {
					h.defer_calls = append(h.defer_calls, &inst.Call)
				}
				break

			default:
				h.defer_calls = append(h.defer_calls, &inst.Call)
			}
			break

		case *ssa.Call:
			// check if it is a lock/unlock/close
			// for lock/unlock: the call might be:
			// (1) t2 = (*sync.RWMutex).Lock(t1)  or (2) t4 = invoke t3.Lock()
			//     use inst.Call.Value.Name()             use inst.Call.Method.Name()
			name := inst.Call.Value.Name()
			if inst.Call.Method != nil { // is abstract invoke
				name = inst.Call.Method.Name()
			}
			switch name { // we dont check wg.Done() here
			case "Lock":
				prev_node = createLockNode(&inst.Call, inst, NLock, false)
				break

			case "RLock":
				prev_node = createLockNode(&inst.Call, inst, NRLock, false)
				break

			case "Unlock":
				prev_node = createUnlockNode(&inst.Call, inst, NUnlock, false)
				break

			case "RUnlock":
				prev_node = createUnlockNode(&inst.Call, inst, NRUnlock, false)
				break

			case "RLocker": // see g.alias, only here for RLocker()
				base_ptr := inst.Call.Args[0]
				base_pts := g.getPTS(base_ptr, h.cur_cgnode)
				locker_pts := g.getPTS(inst, h.cur_cgnode)
				for _, base := range base_pts {
					for _, locker := range locker_pts {
						g.alias[locker] = base
					}
				}
				break

			case "close":
				if inst.Call.Value.String() == "builtin close" {
					cur_node = createChannelClose(&inst.Call, inst)
					g.updateNodes(prev_node, cur_node, false)
					prev_node = cur_node
				} else {
					traverseCallee(inst, inst.Common())
				}
				break

			case "WithCancel", "WithTimeout":
				// how to match the resource of context.WithCancel(), the IR looks like:
				//  t1 = context.WithCancel(ctx) (ctx context.Context, cancel context.CancelFunc)
				//  t2 = extract t1 #0                                      context.Context
				//  ...
				//  t3 = extract t1 #1
				//  ...
				// where t2 is the context, t3 is the cancel function -> mark them
				// heuristics: as long as we dont change go lib, ir does not change,
				//   the return values t2 + 3 = t3, e.g., in cockroach13755: they are n64142 and n64145
				// TODO: pta distinguish calls for context.WithCancel
				base_pts := g.getPTS(inst, h.cur_cgnode)
				for _, obj := range base_pts {
					g.contexts[obj] = &Pair{
						loc:      fmt.Sprintf("bb%d@%s", bb.Index, h.cur_cgnode.GetFunc().String()),
						ctx_inst: inst,
						base_obj: obj,
					}
				}
				break

			case "Wait":
				if strings.HasPrefix(inst.Call.String(), "(*sync.WaitGroup).Wait") { // from waitgroup.Wait()
					ptr := inst.Call.Args[0]
					cur_node = &Node{
						id:        ID,
						typId:     NWGWait,
						cgnode:    h.cur_cgnode,
						parent_id: parent_node.id,
						gid:       h.cur_tid,
						wg_ptr:    ptr, // the free var
						wait_inst: inst,
					}
					g.updateNodes(prev_node, cur_node, false)
					g.UpdateResources(ptr, h.cur_cgnode, cur_node)
					prev_node = cur_node
					break
				} else if strings.HasPrefix(inst.Call.String(), "(*sync.Cond).Wait") { // from cond.Wait()
					ptr := inst.Call.Args[0]
					cur_node = &Node{
						id:        ID,
						typId:     NCONDWait,
						cgnode:    h.cur_cgnode,
						parent_id: parent_node.id,
						gid:       h.cur_tid,
						cond_ptr:  ptr, // the free var
						wait_inst: inst,
					}
					g.updateNodes(prev_node, cur_node, false)
					g.UpdateResources(ptr, h.cur_cgnode, cur_node)
					prev_node = cur_node
					break
				}
				// normal calls
				traverseCallee(inst, inst.Common())
				break

			case "Signal": // for (*sync.Cond).Signal
				ptr := inst.Call.Args[0]
				cur_node = &Node{
					id:          ID,
					typId:       NSignal,
					cgnode:      h.cur_cgnode,
					parent_id:   parent_node.id,
					gid:         h.cur_tid,
					cond_ptr:    ptr, // the free var
					signal_inst: inst,
				}
				g.updateNodes(prev_node, cur_node, false)
				g.UpdateResources(ptr, h.cur_cgnode, cur_node)
				prev_node = cur_node
				break

			case "Broadcast": // (*sync.Cond).Broadcast
				ptr := inst.Call.Args[0]
				cur_node = &Node{
					id:             ID,
					typId:          NBroadcast,
					cgnode:         h.cur_cgnode,
					parent_id:      parent_node.id,
					gid:            h.cur_tid,
					cond_ptr:       ptr, // the free var
					broadcast_inst: inst,
				}
				g.updateNodes(prev_node, cur_node, false)
				g.UpdateResources(ptr, h.cur_cgnode, cur_node)
				prev_node = cur_node
				break

			case "Done":
				// context.Done() does not need to be visited, since it follows a channel receive op, e.g.,
				//  t0 = invoke ctx.Done()
				//	t1 = <-t0   => as long as we collect this
				if strings.HasPrefix(inst.Call.String(), "(*sync.WaitGroup).Done") { // from waitgroup.Done()
					cur_node = updateWGDone(&inst.Call, inst)
					prev_node = cur_node
					break
				}
				// normal calls
				traverseCallee(inst, inst.Common())
				break

			case "Do":
				if strings.HasPrefix(inst.Call.String(), "(*sync.Once).Do") {
					ptr := inst.Call.Args[1]
					callee_pts := g.getPTS(ptr, h.cur_cgnode)
					if DEBUG && len(callee_pts) > 1 {
						fmt.Printf("!!! pts of %s has %d objs in its pts\n", ptr, len(callee_pts))
					}
					for _, obj := range callee_pts {
						callee := g.pa.GetCalleeByID(obj)
						if g.doVisit(inst, callee, h) {
							_, cur_node = g.DFS(callee, cur_node, inst, false)
						}
					}
					prev_node = cur_node
					break
				}
				// normal calls
				traverseCallee(inst, inst.Common())
				break

			case "AfterFunc": // its makeclosure runs in a new goroutine, e.g., grpc3017
				// TODO: give the makeclosure a new context
				mc_inst := inst.Call.Args[1]
				pts := g.getPTS(mc_inst, h.cur_cgnode)
				for _, obj := range pts {
					mc_fn := g.pa.GetCalleeByID(obj)
					if g.doVisit(inst, mc_fn, h) {
						_, cur_node = g.DFS(mc_fn, cur_node, inst, true)
					}
				}
				break

			default: // workflow of normal function calls
				traverseCallee(inst, inst.Common())
				break
			}
			break

		case *ssa.Return:
			if h.is_in_loop { // for now, the return is only important when it's in a loop
				// we avoid the naming pattern, but use r and e
				// ofter called by select cases
				r := &Node{ // return
					id:        ID,
					typId:     NReturn,
					cgnode:    h.cur_cgnode,
					parent_id: parent_node.id,
					gid:       h.cur_tid,
					ret_inst:  inst,
				}
				g.updateNodes(prev_node, r, false)

				// link to a loop exit node
				cur_node = &Node{ // exit
					id:        ID,
					typId:     NLoop,
					cgnode:    h.cur_cgnode,
					parent_id: parent_node.id,
					gid:       h.cur_tid,
					loop_name: h.loop_name,
					enterLoop: false,
				}
				g.updateNodes(r, cur_node, false)
				prev_node = cur_node
				break
			}

			if bb.Comment == "rangechan.done" { // the return is also important when it's in a roc
				r := &Node{ // return
					id:        ID,
					typId:     NReturn,
					cgnode:    h.cur_cgnode,
					parent_id: parent_node.id,
					gid:       h.cur_tid,
					ret_inst:  inst,
				}
				g.updateNodes(prev_node, r, false)

				prev_node = r
			}
			break

		case *ssa.Send:
			createSendNode := func(channel ssa.Value) {
				cur_node = &Node{
					id:           ID,
					typId:        NChannel,
					cgnode:       h.cur_cgnode,
					parent_id:    parent_node.id,
					gid:          h.cur_tid,
					ch_name:      "send@channel " + channel.String(),
					ch_send_inst: inst,
					ch_ptr:       channel,
					isSender:     true,
				}
				g.updateNodes(prev_node, cur_node, false)
				g.UpdateResources(channel, h.cur_cgnode, cur_node)
			}

			ch := g.getChannelPtrForInst(inst)
			g.updateChannelPTS(ch, h.cur_cgnode)
			createSendNode(ch)
			prev_node = cur_node
			break

		case *ssa.UnOp:
			if inst.Op == token.ARROW {
				if checkCtxDone(j) {
					break
				}
				if returnChannel(inst, j) {
					break
				}

				// normal channel receive
				channel := inst.X
				g.updateChannelPTS(channel, h.cur_cgnode)
				cur_node = &Node{
					id:          ID,
					typId:       NChannel,
					cgnode:      h.cur_cgnode,
					parent_id:   parent_node.id,
					gid:         h.cur_tid,
					ch_name:     "receive@channel " + inst.X.String(),
					ch_rec_inst: inst,
					ch_ptr:      channel,
					isSender:    false,
				}
				g.updateNodes(prev_node, cur_node, false)
				g.UpdateResources(channel, h.cur_cgnode, cur_node)
				prev_node = cur_node
			} // else are not important
			break

		case *ssa.Select:
			cur_node = &Node{
				id:         ID,
				typId:      NSelect,
				cgnode:     h.cur_cgnode,
				parent_id:  parent_node.id,
				gid:        h.cur_tid,
				sel_name:   "select@" + inst.String(),
				sel_inst:   inst,
				is_in_loop: h.is_in_loop,
			}
			g.updateNodes(prev_node, cur_node, false)
			h.select_node_id = cur_node.id // for later marks

			// collect all cases, including the empty cases in examples
			case_bodies := getCaseBBs(bb, h, inst.Blocking) // find case body
			size := len(case_bodies)
			if size == 0 { // when all are empty bodies
				if len(inst.States) != 0 {
					size += len(inst.States)
				}
				if !inst.Blocking {
					size++ // for empty default block
				}
			} else if size == len(inst.States) && !inst.Blocking {
				size++ // for empty default block
			}
			cur_node.cases = make([]*Node, size)

			// completeCaseNode for a select case
			completeCaseNode := func(c *Node, ith_state int) {
				// fill in the case body if exists
				var _cur_node *Node
				if len(case_bodies) > ith_state {
					bodies := case_bodies[ith_state]
					if bodies == nil {
						return
					}
					body := bodies[0]
					_cur_node = g.DFSBB(body, parent_node, c, h)
					h.skips[body] = true
					if len(bodies) > 1 {
						for k := 1; k < len(bodies); k++ {
							body = bodies[k]
							if h.skips[body] {
								continue
							}
							h.skips[body] = true
							_node := g.DFSBB(body, c, _cur_node, h)
							if _node != nil {
								_cur_node = _node
							}
						}
					}

					// see if existing a bb that contains a return to stop the loop
					// or other interesting statements
					if h.is_in_loop && _cur_node.typId == NLoop && _cur_node.enterLoop == false {
						// this node lead to end of path
						// TODO: any other possibilities?
						c.case_to_end = true
					}
				}
			}

			// match body with its cases
			for i, state := range inst.States {
				is_sender := state.Dir == types.SendOnly

				switch val := state.Chan.(type) {
				case *ssa.UnOp:
					//channel := val.X
					channel := val
					g.updateChannelPTS(channel, h.cur_cgnode)
					// normal cases or when the above pts is empty
					c := &Node{
						id:           ID,
						typId:        NCase,
						cgnode:       h.cur_cgnode,
						parent_id:    parent_node.id,
						gid:          h.cur_tid,
						case_ch_name: "case@" + channel.String(),
						case_sel:     cur_node,
						isSender:     is_sender,
						case_state:   state,
						ch_ptr:       channel,
					}
					cur_node.cases[i] = c

					g.updateNodes(cur_node, c, true)
					g.UpdateResources(channel, h.cur_cgnode, c) // pts of the return value of the call
					completeCaseNode(c, i)
					break

				case *ssa.Call, *ssa.Parameter, *ssa.Phi:
					// e.g., moby17176 is call, moby33781 is parameter, kubernetes70277 is phi
					var c *Node
					if call, ok := val.(*ssa.Call); ok {
						// NOTE this might be a context.Done()
						if call.Call.Method != nil &&
							call.Call.Method.String() == "func (context.Context).Done() <-chan struct{}" {
							done_pts := g.getPTS(call.Call.Value, h.cur_cgnode)
							// TODO: what if multiple NCaseDone node?
							for _, obj := range done_pts {
								createCaseDone := func(p *Pair) {
									c = &Node{
										id:         ID,
										typId:      NCaseDone,
										cgnode:     h.cur_cgnode,
										parent_id:  parent_node.id,
										gid:        h.cur_tid,
										ctx_name:   "case@done@" + p.loc + " " + val.String(),
										case_sel:   cur_node,
										isSender:   is_sender,
										case_state: state,
										done_inst:  call,
										p:          p,
										is_cancel:  false, // sender
									}
									cur_node.cases[i] = c

									g.updateNodes(cur_node, c, true)
									g.UpdateResourcesInner(p.base_obj, c)
									completeCaseNode(c, i)
								}

								if p, ok2 := g.contexts[obj]; ok2 {
									// this is the matched context
									createCaseDone(p)
								} else {
									done_pts2 := g.pa.GetPTSByPtrID(obj)
									if len(done_pts2) == 0 {
										continue // obj is not a pointer type
									}
									for _, obj2 := range done_pts2 {
										if p2, ok3 := g.contexts[obj2]; ok3 {
											createCaseDone(p2)
										}
									}
								}
							}
							if c != nil {
								break
							}
						}
					}

					// normal cases or when the above pts is empty
					g.updateChannelPTS(val, h.cur_cgnode)
					c = &Node{
						id:           ID,
						typId:        NCase,
						cgnode:       h.cur_cgnode,
						parent_id:    parent_node.id,
						gid:          h.cur_tid,
						case_ch_name: "case@" + val.String(),
						case_sel:     cur_node,
						isSender:     is_sender,
						case_state:   state,
						ch_ptr:       val,
					}
					cur_node.cases[i] = c

					g.updateNodes(cur_node, c, true)
					g.UpdateResources(val, h.cur_cgnode, c) // pts of the return value of the call
					completeCaseNode(c, i)

				default:
					// TODO: Handle other non-UnOp cases here
					fmt.Printf("TODO: Handle other non-UnOp cases here: %s \n", val.Type())
				}
			}

			if !inst.Blocking { // we have default case body
				c := &Node{
					id:           ID,
					typId:        NCase,
					cgnode:       h.cur_cgnode,
					parent_id:    parent_node.id,
					gid:          h.cur_tid,
					case_ch_name: "case@default",
					case_sel:     cur_node,
					isSender:     false,
					case_state:   nil,
					ch_ptr:       nil,
				}
				if case_bodies[size-1] == nil {
					// empty default case
				} else {
					c.default_inst = case_bodies[size-1][0].Instrs[0]
				}
				cur_node.cases[size-1] = c

				// updateNodes
				g.updateNodes(cur_node, c, true)
				completeCaseNode(c, size-1)
			}

			prev_node = cur_node
			h.select_node_id = -1
			break

		default:
			if DEBUG {
				fmt.Printf("%s %T\n", inst, inst)
			}
		}

		if h.select_node_id > 0 {
			prev_node.select_node_id = h.select_node_id
		}
	}

	return prev_node
}

// isCtxDone return true if this inst is a Done function from context
func isCtxDone(inst ssa.Instruction) (*ssa.Call, bool) {
	call, ok := inst.(*ssa.Call)
	return call, ok && call.Call.Method != nil &&
		call.Call.Method.String() == "func (context.Context).Done() <-chan struct{}"
}

// creatorHelper to help the traversal of DFSBB
type creatorHelper struct {
	cur_cgnode     *pointer.Node
	skips          map[*ssa.BasicBlock]bool
	is_in_loop     bool
	loop_name      string // define when seeing "for.body"
	is_in_roc      bool
	roc_name       string // define when seeing "rangechan.loop"
	cur_tid        int    // current tid
	select_node_id int    // the select_node_id if this is from a select case, default = -1; only in the belonging function, do not inherent

	// for unexpected exit of control flow
	has_lock    bool
	has_wg      bool
	defer_nodes []*Node           // stack of defer insts
	defer_calls []*ssa.CallCommon // stack of defer fn calls
}

// isWrappedInFn is makeclosure/function call wrapped in a function parameter which is called later
// we may have the following scenario, e.g., Kubernetes11298 (below)
//
//	t10 = make closure Notify$1 [t0, t1]                             func()
//	t11 = After(t10)                                                 Signal
//	*t9 = t11
//	t12 = make closure Notify$2 [t1, t9]                             func()
//	t13 = *t0                                               <-chan struct{}
//	t14 = Until(t12, 0:time.Duration, t13)                               ()
//
// where t11 will be used in future function call as parameter
// we need to identify then and skip their visits by dataflow analysis ... TODO: avoid visit the same inst twice
func (h *creatorHelper) isWrappedInFn(inst *ssa.Call) bool {
	visited := make(map[ssa.Instruction]ssa.Instruction)
	refs := *inst.Referrers()
	var nexts []ssa.Instruction
	for len(refs) > 0 {
		for _, ref := range refs {
			visited[ref] = ref
			if _, ok := ref.(*ssa.MakeClosure); ok {
				return true
			}
			switch next := ref.(type) {
			case *ssa.Call:
				for _, n := range *next.Referrers() {
					if _, ok := visited[n]; !ok {
						nexts = append(nexts, n)
					}
				}
			case *ssa.Alloc:
				for _, n := range *next.Referrers() {
					if _, ok := visited[n]; !ok {
						nexts = append(nexts, n)
					}
				}
			case *ssa.Store:
				if alloc, ok := next.Addr.(*ssa.Alloc); ok {
					for _, n := range *alloc.Referrers() {
						if _, ok2 := visited[n]; !ok2 {
							nexts = append(nexts, n)
						}
					}
				}
			}
		}
		if len(nexts) == 0 {
			break
		}
		refs = nexts
		nexts = make([]ssa.Instruction, 0)
	}
	return false
}

// DFS for creating graph: only called by the start of main or goroutines or function entries
// return parent_node and prev_node
func (g *RFGraph) DFS(cur_cgnode *pointer.Node, parent *Node, creation_site ssa.CallInstruction, isThread bool) (*Node, *Node) {
	var parent_node *Node
	if isThread {
		var name string
		if parent == nil { // main entry
			name = "main@" + cur_cgnode.String()
		} else {
			name = "go@[" + creation_site.String() + "] " + cur_cgnode.String()
		}
		go_creation_site, _ := creation_site.(*ssa.Go)
		parent_node = &Node{
			id:        ID,
			typId:     NGoroutine,
			cgnode:    cur_cgnode,
			parent_id: -1,
			g_name:    name,
			g_inst:    go_creation_site,
			gid:       len(g.roots),
		}
		g.roots = append(g.roots, parent_node)
	} else { // normal function/method node -> bridge nodes among functions
		parent_node = &Node{
			id:        ID,
			typId:     NFunc,
			cgnode:    cur_cgnode,
			parent_id: parent.id,
			gid:       parent.gid,
			fn_name:   "fn@" + cur_cgnode.String(),
		}
	}

	cur_node := parent_node
	sig := getSig(creation_site, cur_cgnode, parent_node.gid)
	g.visited[sig]++

	// updateNodes manually
	g.id2node[ID] = cur_node
	if parent != nil {
		g.adjList[parent] = append(g.adjList[parent], cur_node)
	}
	ID++

	if DEBUG {
		fmt.Println("\n" + strings.Repeat("=", 40))
		fmt.Printf("Entering CGNode: %+v\n", cur_cgnode.String())
	}

	// rough filter: check if any interesting instruction in bbs
	bbs := cur_cgnode.GetCGNode().GetFunc().Blocks
	if !g.hasInterestingInsts(bbs) {
		// only handle function calls
		for _, edge := range cur_cgnode.Out {
			callee := edge.Callee
			site, _ := edge.Site.(*ssa.Call)
			if g.doVisit(site, callee, nil) {
				g.DFS(edge.Callee, cur_node, edge.Site, false)
			}
		}

		if DEBUG {
			fmt.Printf("Exiting CGNode: %+v\n", cur_cgnode.String())
			fmt.Println(strings.Repeat("=", 40) + "\n")
		}
		return parent_node, cur_node
	}

	// skip if this fn is context.cancel()
	if cur_cgnode.GetFunc().String() == "context.WithCancel$1" {
		if DEBUG {
			fmt.Printf("Exiting CGNode: %+v\n", cur_cgnode.String())
			fmt.Println(strings.Repeat("=", 40) + "\n")
		}
		return parent_node, cur_node
	}

	h := &creatorHelper{
		cur_cgnode: cur_cgnode,
		skips:      make(map[*ssa.BasicBlock]bool), // skip the traversal for consumed "select.body"
		is_in_loop: false,                          // TODO: inherent loop?
		cur_tid:    parent_node.gid,
	}

	// traverse the instructions in cur_cgnode with different scenarios:
	// 1. select -> case1, case2, ...
	// whenever a bb has select, its successors contains bb with Comment "select.body",
	// which are first bbs of select cases
	// for bb with Comment "select.next", it leads to panic, ignore them
	// when select is not in a loop, there is a bb with Comment "select.done" to end the control flow of all "select.body"s
	// 2. range over channel (ROC) and loop with select inside
	// the bb of ROC starts with "rangechan.loop" and may have "rangechan.done" ending its loop
	// for bb of normal loop without select, it starts with "for.body", then index change and check with "for.loop", and end with
	// "for.done".
	// for bb of a loop with select, it only starts with "for.body", there is no other blocks, the only way to exit the loop is
	// receiving a msg and return/break
	prev_node := cur_node
	for _, bb := range bbs {
		if DEBUG {
			fmt.Println("=> bb: " + bb.String() + " #" + bb.Comment)
		}
		cur_node = g.DFSBB(bb, prev_node, prev_node, h) // traverse each basic block (BB)

		if h.is_in_roc {
			// visit the roc bbs
			prev_node = cur_node
			bodies := getROCBBs(bb, h)
			for _, body := range bodies {
				cur_node = g.DFSBB(body, prev_node, prev_node, h) // TODO: which parent_node can look better?
				if cur_node != nil {
					prev_node = cur_node
				}
			}

			// link to a roc exit node
			cur_node = &Node{ // exit
				id:        ID,
				typId:     NROC,
				cgnode:    h.cur_cgnode,
				parent_id: parent_node.id,
				gid:       h.cur_tid,
				roc_name:  h.roc_name,
				enterROC:  false,
			}
			g.updateNodes(prev_node, cur_node, false)
			prev_node = cur_node

			// finish the process of roc
			h.is_in_roc = false
			continue
		}

		if cur_node != nil {
			prev_node = cur_node
		} else if DEBUG {
			fmt.Println("?? nil cur_node")
		}
	}

	if DEBUG {
		fmt.Printf("Exiting CGNode: %+v\n", cur_cgnode.String())
		fmt.Println(strings.Repeat("=", 40) + "\n")
	}

	// check unexpected exit of control flow
	cfchecker := InitializeCFChecker(cur_cgnode, g)
	es := cfchecker.CheckCompleteFlow()
	if len(es) > 0 {
		g.cferrs = append(g.cferrs, es...)
	}

	//return parent_node
	return parent_node, prev_node
}

// onlyDeferReturn the basic block only have the following inst:
//
//	rundefers
//	return
func onlyDeferReturn(bb *ssa.BasicBlock) bool {
	if len(bb.Instrs) != 2 {
		return false
	}
	_, ok1 := bb.Instrs[0].(*ssa.RunDefers)
	_, ok2 := bb.Instrs[1].(*ssa.Return)
	return ok1 && ok2
}

// getSig create a string sig for checking g.visited
// main does not have creation_site
func getSig(site ssa.CallInstruction, callee *pointer.Node, tid int) string {
	sig := callee.GetFunc().String()
	if site == nil {
		return sig
	}
	switch call_site := site.(type) {
	case *ssa.Call:
		if call_site == nil {
			return sig
		}
		sig = call_site.String() + "@" + sig + "@" + fmt.Sprintf("%v", callee.GetContext())
	case *ssa.Go:
		sig = call_site.String() + "@" + sig + "@" + fmt.Sprintf("%v", callee.GetContext())
	default:
		panic("should not handle other site type: " + site.String())
	}
	return sig
}

// doVisit return true if we need to visit this callee
// TODO: we only consider the app functions for now
func (g *RFGraph) doVisit(site ssa.CallInstruction, callee *pointer.Node, h *creatorHelper) bool {
	if callee != nil && callee.GetFunc() != nil && inScopePkg(callee.GetFunc()) {
		sig := getSig(site, callee, h.cur_tid)
		visited_num := g.visited[sig]
		if visited_num < limit {
			return true
		}
		return false
	}
	return false
}

// hasInterestingInsts
func (g *RFGraph) hasInterestingInsts(bbs []*ssa.BasicBlock) bool {
	for _, bb := range bbs {
		for _, instr := range bb.Instrs {
			switch instr.(type) {
			// TODO: adjust interesting insts all the time
			case *ssa.Go, *ssa.Select, *ssa.Send, *ssa.Defer, *ssa.Call, *ssa.MakeClosure, *ssa.MakeChan, *ssa.UnOp:
				return true
			}
		}
	}
	return false
}

// CreateGraph create RFG ƒor one main
func (g *RFGraph) CreateGraph(ptResult *pointer.ResultWCtx, prog *ssa.Program, entry *pointer.Node) {
	g.pa = ptResult
	g.cg = ptResult.CallGraph
	g.prog = prog

	fmt.Println("\n\n*********************************** ")
	if ptResult.IsTest {
		fmt.Println("Creating RFG for test entry: " + entry.String())
		_, last_node := g.DFS(entry, nil, nil, true)
		exit := &Node{
			id:        ID,
			typId:     NExit,
			cgnode:    entry,
			parent_id: -1,
			gid:       0,
		}
		g.updateNodes(last_node, exit, false)
		fmt.Println("Finish building RFG for test entry: " + entry.String())
	} else { // main entry
		fmt.Println("Creating RFG for main entry: " + entry.String())
		_, last_node := g.DFS(entry, nil, nil, true)
		exit := &Node{
			id:        ID,
			typId:     NExit,
			cgnode:    entry,
			parent_id: -1,
			gid:       0,
		}
		g.updateNodes(last_node, exit, false)
		fmt.Println("Finish building RFG for main entry: " + entry.String())
	}

	if DEBUG {
		g.PrintGraph() // debug
	}
}

func (g *RFGraph) prettyPrintTree(node *Node, indent string) {
	if node == nil {
		return
	}

	fmt.Println(indent+"->", node.String())
	indent += ""
	if node.typId == NSelect {
		// print its cases as kid
		for _, c := range node.cases {
			if c == nil {
				fmt.Println(indent + "-> Node (case): empty")
				continue
			}
			g.prettyPrintTree(c, indent)
		}
	}

	for _, kid := range g.adjList[node] {
		switch kid.typId {
		//case NFunc, NLock, NUnlock:
		//	if DEBUG {
		//		g.prettyPrintTree(kid, indent)
		//	}
		//	continue
		default:
			g.prettyPrintTree(kid, indent)
		}
	}
}

func (g *RFGraph) PrintGraph() {
	fmt.Println("\n\n*********************************** ")
	fmt.Println("Printing RFG: ")
	//for _, root := range g.roots {
	//	g.prettyPrintTree(root, "")
	//}
	g.prettyPrintTree(g.roots[0], "")

	fmt.Println("\nPrinting Producer&Consumer: ")
	for id, pcs := range g.resources {
		fmt.Printf("\n%d: %s ->\nproducers:\n", id, g.pa.GetLabelFor(id))
		for _, p := range pcs.producers {
			fmt.Printf("\t%s\n", p.String())
		}
		fmt.Println("consumers:")
		for _, c := range pcs.consumers {
			fmt.Printf("\t%s\n", c.String())
		}
	}
}

// detectionState to maintain each possible state during the traversal by DFSBB
// this is also the result of a state exploration
// int -> node.id
type detectionState struct {
	g *RFGraph // this graph

	splitting_points []int // at which node (id) we copy the prev state and start a new explore

	pause_ats []int // for each goroutine (including main, indicating by index), which node did our traversal paused at (last traversed)
	// initialize: -1 means not start yet or reset by starting the visit of a paused node;
	// -2 means reaching the exit node already;
	// -3 means has unclosed loop
	can_walk    map[int]bool     // the consumers waiting to be consumed in next iteration
	pairs       map[int]int      // a map of a paired consumer -> producer  Note: any producer and consumer can only pair ONCE unless from the pair-all producers
	hip         []int            // tid that currently can run in parallel TODO: change to map -> faster
	visit_last  []int            // delay the handling of some producers, e.g., signal, broadcast, TODO: others?
	visit_loops map[string]*Node // all the loop nodes we visited: when add to the map when seeing enter, and remove when seeing its exit
	// loop name -> 1st statement in the loop

	// for deadlocks
	lockset     map[int]*Node   // a map of locked object and its holding lock (with thread id)
	rlockset    map[int]*Node   // a map of rlocked object and its holding lock (with thread id)
	doublelock  map[int][]*Node // a map of double locks: tid -> the locks (rw or ww, which have the same tid)
	twolock     map[int][]*Node // w -> w or r -> w or w -> r from different threads (tid uses the 1st lock)
	triplelocks map[int][]*Node // a map of rwr tripple locks: tid -> rwrlock node (tid from 1st and 3rd rlock)

	visited map[int]int // visited nodes in this state TODO: time limit for better precision?
	//default value = 0: unvisited, 1: visited, 2: wlock waiting for a rlock

	// where to start the traversal
	start_node_id int
	start_tid     int
}

func (h *detectionState) isInHIP(tid int) bool {
	for _, t := range h.hip {
		if t == tid {
			return true
		}
	}
	return false
}

// isInLocks whether node_id is in h.doublelock or h.triplelocks
func (h *detectionState) isInLocks(node_id int) bool {
	for _, l := range h.doublelock {
		for _, n := range l {
			if n.id == node_id {
				return true
			}
		}
	}
	for _, l := range h.twolock {
		for _, n := range l {
			if n.id == node_id {
				return true
			}
		}
	}
	for _, l := range h.triplelocks {
		for _, n := range l {
			if n.id == node_id {
				return true
			}
		}
	}
	return false
}

// holdingLock we pause at a lock, and at some point, there is no more goroutines can walk but this lock
// return node_id and n_tid
func (h *detectionState) holdingLock(id2node map[int]*Node) (int, int) {
	for _, lock := range h.lockset { // a goroutine pauses at a lock
		tid := lock.gid
		node_id := h.pause_ats[tid]
		if node_id < 0 || !h.isInHIP(tid) {
			continue
		}
		node := id2node[node_id]
		if node.typId == NLock && !h.isInLocks(node.id) {
			return node_id, node.gid
		}
	}
	return -1, -1
}

// holdingRLock same as holdingLock
func (h *detectionState) holdingRLock(id2node map[int]*Node) (int, int) {
	for _, lock := range h.rlockset { // a goroutine pauses at a rlock
		tid := lock.gid
		node_id := h.pause_ats[tid]
		if node_id < 0 || !h.isInHIP(tid) {
			continue
		}
		node := id2node[node_id]
		if node.typId == NRLock && !h.isInLocks(node.id) {
			return node_id, node.gid
		}
	}
	return -1, -1
}

// getRandomTID that is not cur_tid
func (h *detectionState) getRandomTID(cur_tid int, prev_tid int) int {
	if len(h.hip) == 0 {
		return -1
	}

	rand.Seed(time.Now().UnixNano())
	size := len(h.hip)
	for {
		random_idx := rand.Intn(size)
		random_tid := h.hip[random_idx]

		// If there are only two elements in hip and one is the same as the given element,
		// ensure that the first call reaching this point returns a different element,
		// and the second call here returns the other element that is different from prev_tid and random_tid.
		if size == 2 && random_tid == cur_tid {
			if prev_tid == random_tid {
				return h.hip[1-random_idx]
			}
			continue
		}

		// If the retrieved element is different from the given element, return the index
		if random_tid != cur_tid {
			return random_tid
		} else if size == 1 && random_tid == prev_tid { // no other options
			return random_tid
		}
	}
}

// canHIP = check_tid can happen in parallel with h.hip, but not in the cur_tid
func (h *detectionState) canHIP(check_tid, cur_tid int) bool {
	if check_tid == cur_tid {
		return false
	}
	for _, id := range h.hip {
		if id == check_tid {
			return true
		}
	}
	return false
}

func (h *detectionState) hasVisitLast() bool {
	for _, node_id := range h.visit_last {
		if node_id > 0 {
			return true
		}
	}
	return false
}

// hasManyPausedWaits return true if some cond.Wait() are paused due to missing producer
func (h *detectionState) hasManyPausedWaits(id2node map[int]*Node) bool {
	count := 0
	for _, pause_id := range h.pause_ats {
		if n, ok := id2node[pause_id]; ok && n.typId == NCONDWait {
			count++
		}
	}
	return count >= 1
}

// noPausedIsWalkable none of the paused node is walkable
// no progress after iterating all goroutines again -> quit
func (h *detectionState) noPausedIsWalkable() bool {
	for _, tid := range h.hip {
		pause_id := h.pause_ats[tid]
		if can, ok := h.can_walk[pause_id]; ok && can {
			return false
		}
	}
	return true
}

// noWalk return true if no goroutine has node can walk
func (h *detectionState) noWalk() bool {
	for _, can := range h.can_walk {
		if can {
			return false
		}
	}
	return true
}

// hasMeaningfulPaused pause with node id >= 0
func (h *detectionState) hasMeaningfulPaused() bool {
	for _, pause := range h.pause_ats {
		if pause >= 0 {
			return true
		}
	}
	return false
}

// noPaused return true if no goroutine has paused node and all reached to the end
func (h *detectionState) noPaused() bool {
	for _, pause := range h.pause_ats {
		if pause != -2 {
			return false
		}
	}
	return true
}

// noPausedWaits no paused different types of waits, e.g., unlocks, wgwaits
// TODO: condwaits?
func (h *detectionState) noPausedWaits(id2node map[int]*Node) (int, int) {
	for _, pause := range h.pause_ats {
		if pause < 0 {
			continue
		}
		node := id2node[pause]
		if node.typId == NUnlock || node.typId == NRUnlock ||
			node.typId == NWGWait {
			return node.id, node.gid
		}
	}
	return -1, -1
}

// copy the state and return a new instance with its pointer
// id -> splitting node id
func (h *detectionState) copy(id int) *detectionState {
	c := &detectionState{
		splitting_points: make([]int, len(h.splitting_points)),
		pause_ats:        make([]int, len(h.pause_ats)),
		can_walk:         make(map[int]bool),
		pairs:            make(map[int]int),
		lockset:          make(map[int]*Node),
		rlockset:         make(map[int]*Node),
		doublelock:       make(map[int][]*Node),
		twolock:          make(map[int][]*Node),
		triplelocks:      make(map[int][]*Node),
		hip:              make([]int, len(h.hip)),
		visited:          make(map[int]int),
		visit_loops:      make(map[string]*Node),
		visit_last:       make([]int, len(h.visit_last)),
	}
	c.g = h.g
	copy(c.splitting_points, h.splitting_points)
	c.splitting_points = append(c.splitting_points, id)
	copy(c.pause_ats, h.pause_ats)
	copy(c.hip, h.hip)
	for k, v := range h.can_walk {
		c.can_walk[k] = v
	}
	for k, v := range h.pairs {
		c.pairs[k] = v
	}
	for k, v := range h.lockset {
		c.lockset[k] = v
	}
	for k, v := range h.rlockset {
		c.rlockset[k] = v
	}
	for k, v := range h.doublelock {
		c.doublelock[k] = v
	}
	for k, v := range h.twolock {
		c.twolock[k] = v
	}
	for k, v := range h.triplelocks {
		c.triplelocks[k] = v
	}
	for k, v := range h.visited {
		c.visited[k] = v
	}
	for k, v := range h.visit_loops {
		c.visit_loops[k] = v
	}
	copy(c.visit_last, h.visit_last)
	return c
}

// noOtherLockOnResource for the locked obj, return true if there is only one lock in g.resources
func (g *RFGraph) noOtherLockOnResource(pts []int, cur_node *Node) bool {
	for _, obj := range pts { // obj actually is objID
		if pcs, exist := g.resources[obj]; exist {
			if len(pcs.consumers) > 1 {
				return false
			}
		}
	}
	return true
}

// pickConsumers filter the consumers that has the smallest node id in its belonging thread
// and can happen in parallel with cur_tid and not paired
// since this is the possible match
func (g *RFGraph) pickConsumers(consumers []*Node, cur_tid int, ds *detectionState) []*Node {
	ret := make([]*Node, 0)
	tid2nodeid := make(map[int]int) // tid -> smallest node id
	select_node_id := -1            // check if multiple cases from the same select can match
	for _, n := range consumers {
		n_tid := n.gid
		nid, ok := tid2nodeid[n_tid]
		if ok {
			min_id := nid
			if min_id > n.id {
				min_id = n.id
			}
			tid2nodeid[n_tid] = min_id
		} else {
			tid2nodeid[n_tid] = n.id
		}

		if select_node_id == -1 {
			select_node_id = n.select_node_id
		} else if n.select_node_id == select_node_id {
			// if n.select_node_id > select_node_id, happens after this select, no need to consider
			// if n.select_node_id < select_node_id, should not happen, since consumers are ascending id order
			ret = append(ret, n)
		}
	}
	for tid, n_id := range tid2nodeid {
		if _, paired := ds.pairs[n_id]; !paired && ds.canHIP(tid, cur_tid) {
			ret = append(ret, g.id2node[n_id])
		}
	}
	return ret
}

// onlyICanWalk after a lock in node.gid, other goroutines are either
// (1) exit, (2) not started and haven't visited its go node, (3) not paused at a lock or a go node
func (g *RFGraph) onlyICanWalk(cur_tid int, ds *detectionState) bool {
	for tid, _ := range g.roots {
		paused_id := ds.pause_ats[tid]
		if cur_tid == tid || paused_id == -2 || paused_id == -1 { // -1 means no-exit loop or the current visiting goroutine
			continue
		}
		return false
	}

	fmt.Printf(" - only i can walk: tid = %d\n", cur_tid)
	return true
}

// isDuplicateState return true if we have this state already
func (g *RFGraph) isDuplicateState(state *detectionState) bool {
	// if the last two nodes in a state repeat, it's dead ... don't explore again
	// e.g., moby4951
	if hasRepeatState(state.splitting_points) {
		return true
	}

	for _, s := range g.states {
		if IntArrayContains(s.splitting_points, state.splitting_points) ||
			IntMapContains(s.pairs, state.pairs) {
			return true
		}
	}
	return false
}

// visitNode return whether we continue the traversal on this thread
func (g *RFGraph) visitNode(cur_node *Node, ds *detectionState) bool {
	if DEBUG {
		fmt.Printf("    cur_node: %s\n", cur_node)
	}
	cur_tid := cur_node.gid
	if cur_node == nil { // reach the end of this thread
		if DEBUG {
			fmt.Println("*=> End of path")
		}
		ds.pause_ats[cur_tid] = -2
		return false
	}
	traverse := true

	// pause at cur_node
	pauseHere := func(paused_node_id int) {
		ds.pause_ats[cur_tid] = paused_node_id
		traverse = false
		if DEBUG {
			paused_node := g.id2node[paused_node_id]
			fmt.Println("=== pause here@" + paused_node.String())
		}
	}

	// create a new state: we copy a new ds and explore from there
	createNewState := func(consumer *Node, matched_producer_id int) {
		_, paired := ds.pairs[consumer.id]
		v := ds.visited[consumer.id]
		if !paired && v == 0 && ds.canHIP(consumer.gid, cur_tid) {
			_ds := ds.copy(cur_node.id)
			if g.isDuplicateState(_ds) {
				return
			}
			g.states = append(g.states, _ds)
			if DEBUG {
				fmt.Printf("\t- pairing p=%d with c=%d\n", matched_producer_id, consumer.id)
			}
			_ds.pairs[consumer.id] = matched_producer_id
			_ds.can_walk[consumer.id] = true
			_ds.start_node_id = matched_producer_id
			_ds.start_tid = cur_tid
			// new state expr starting from the next node of cur_node, since cur_node already visited here
		}
	}

	// create a new state for lock that already taken by other threads, e.g., etcd6873
	createNewLockState := func(lock *Node, pts []int, held *Node) {
		_ds := ds.copy(cur_node.id)
		if g.isDuplicateState(_ds) {
			return
		}
		g.states = append(g.states, _ds)
		// update lockset
		held_tid := held.gid
		delete(_ds.lockset, held_tid)
		_ds.pause_ats[held_tid] = held.id // pause at this lock
		for _, obj := range pts {         // obj actually is objID
			_ds.lockset[obj] = lock
		}
		_ds.visited[held.id] = 0
		_ds.can_walk[lock.id] = true
		if DEBUG {
			fmt.Printf("\t- try to lock = %v in a new state\n", lock.String())
		}
		_ds.start_node_id = lock.id
		_ds.start_tid = cur_tid
	}

	markConsumerAfterMatching := func(consumer *Node, matched_producer_id int) {
		// TODO: check if other cases of its select already matched/paired when select is not in a loop
		// match in current state
		ds.pairs[consumer.id] = matched_producer_id
		if DEBUG {
			fmt.Printf("\t- pairing p=%d with c=%d\n", matched_producer_id, consumer.id)
		}
		cur_pause_id := ds.pause_ats[consumer.gid]
		if cur_pause_id >= 0 && cur_pause_id < consumer.id {
			return // it will walk to this consumer later, e.g., grpc862
		}
		ds.can_walk[consumer.id] = true
		ds.pause_ats[consumer.gid] = consumer.id
		// mark it for consumer
		if (consumer.typId == NCase || consumer.typId == NCaseDone) && ds.visited[consumer.case_sel.id] == 0 {
			// we did not find match when last time visits the select
			ds.visited[consumer.case_sel.id] = 1 // mark it visited and matched
		}
	}

	// match consumer with a specific producer
	matchChannel := func(pts []int, need_new_state bool, matched_producer_id int) {
		if DEBUG && len(pts) > 1 {
			fmt.Printf("!!! pts of %s has %d objs in its pts\n", cur_node.ch_ptr, len(pts))
		}

		var ds_consumer *Node            // we save the matched pc in current state at the end
		found_consumer := need_new_state // we may need new state at first

		for _, obj := range pts { // obj actually is objID
			if pcs, exist := g.resources[obj]; exist {
				possibles := g.pickConsumers(pcs.consumers, cur_tid, ds)
				for _, consumer := range possibles {
					if (consumer.typId == NCase || consumer.typId == NCaseDone) &&
						ds.visited[consumer.case_sel.id] == 1 {
						// we visited the case and already made the match
						continue
					}
					if found_consumer && EXPLORE_ALL_STATES { // when > 1 consumers can be paired with this producer
						createNewState(consumer, matched_producer_id)
						continue
					}
					if _, paired := ds.pairs[consumer.id]; !paired && ds.canHIP(consumer.gid, cur_tid) {
						// from different threads
						found_consumer = true
						ds_consumer = consumer
						if DEBUG {
							fmt.Printf("\t- pairing p=%d with c=%d (current state)\n", cur_node.id, consumer.id)
						}
					}
				}
			}
		}

		if found_consumer { // update here so this pair/match stays in current ds, not pollute other _ds created for new states
			markConsumerAfterMatching(ds_consumer, matched_producer_id)
		}
		// pause here
		// whether found_consumer or not, wait until consumer consumes it to trigger this event
		pauseHere(matched_producer_id)
	}

	// match as a consumer by finding a matched pair with a *non-paused* producer
	// producer_id != -1: we inform the producer to continue because some producers
	// do not pause the execution and no need to inform
	matchConsumer := func(inform bool) {
		if producer_id, paired := ds.pairs[cur_node.id]; paired && producer_id >= 0 { // from its producer
			if inform { // inform the producer to continue
				ds.can_walk[producer_id] = true
			}
		} else { // pause here
			pauseHere(cur_node.id)
		}
	}

	matchAllConsumer := func(obj int) { // obj actually is objID of resource
		if pcs, exist := g.resources[obj]; exist {
			possibles := g.pickConsumers(pcs.consumers, cur_tid, ds)
			for _, consumer := range possibles {
				if (consumer.typId == NCase || consumer.typId == NCaseDone) && ds.visited[consumer.case_sel.id] == 1 {
					continue // skip already matched cases
				}
				// match with all unmatched consumers
				if _, paired := ds.pairs[consumer.id]; !paired && ds.canHIP(consumer.gid, cur_tid) {
					// from different threads
					markConsumerAfterMatching(consumer, cur_node.id)
				}
			}
		}
	}

	// check if a case is <- timeout.C
	isTimer := func(c *Node) bool {
		if c.case_state == nil {
			return false
		}
		if c.case_state.Chan.Type().String() == "<-chan time.Time" {
			// this is timeout channel -> select this and continue the run, e.g., moby33781, time.After()
			return true
		}
		return false
	}

	// add to map when entering a loop, and delete from map when exiting
	updateLoops := func(name string, is_enter bool, node *Node) {
		if is_enter {
			ds.visit_loops[name] = node
		} else {
			delete(ds.visit_loops, name)
		}
	}

	// check if exists any loops unclosed in current function
	hasUnclosedLoop := func() bool {
		if len(ds.visit_loops) > 0 {
			for _, node := range ds.visit_loops {
				if node.gid == cur_tid && node.cgnode.GetFunc() == cur_node.cgnode.GetFunc() {
					return true
				}
			}
		}
		return false
	}

	// check if we cannot lock here,
	// return held_tid if we have an obj in pts that cannot be locked right now
	cannotLock := func(pts []int) *Node {
		if DEBUG && len(pts) > 1 {
			fmt.Println("!!! warn: > 1 obj get locked ")
		}
		for _, obj := range pts { // obj actually is objID
			if held, ok := ds.lockset[obj]; ok {
				return held
			}
		}
		return nil
	}

	cannotRLock := func(pts []int) *Node {
		if DEBUG && len(pts) > 1 {
			fmt.Println("!!! warn: > 1 obj get rlocked ")
		}
		for _, obj := range pts { // obj actually is objID
			if held, ok := ds.rlockset[obj]; ok {
				return held
			}
		}
		return nil
	}

	// if calls wg.Done() and all spawned goroutine completes, inform wg.Wait()
	// return true when matched
	matchWGDone := func() bool {
		for _, obj := range cur_node.wg_pts {
			if pcs, exist := g.resources[obj]; exist {
				// NOTE: all producers (NEXITs) have to complete, check first
				for _, producer := range pcs.producers {
					p_tid := producer.gid
					done_node, ok := g.tid2end[p_tid]
					if ds.pause_ats[p_tid] == -2 || (ok && ds.visited[done_node.id] == 1) {
						// we seen the exit or the done node
					} else if DEBUG {
						fmt.Printf("groutine tid = %d did not complete, wg wait here ... ", cur_tid)
						return false
					}
				}
				// inform Wait() with normal workflow TODO: we should only have one Wait()
				var ds_consumer *Node // we save the matched pc in current state at the end
				found_consumer := false
				possibles := g.pickConsumers(pcs.consumers, cur_tid, ds)
				for _, consumer := range possibles {
					if found_consumer && EXPLORE_ALL_STATES {
						createNewState(consumer, cur_node.id)
						continue
					}
					if _, paired := ds.pairs[consumer.id]; !paired && ds.canHIP(consumer.gid, cur_tid) {
						// from different threads
						found_consumer = true
						ds_consumer = consumer
						if DEBUG {
							fmt.Printf("\t- pairing p=%d with c=%d (current state)\n", cur_node.id, consumer.id)
						}
					}
				}

				if found_consumer { // update here so this pair/match stays in current ds, not pollute other _ds created for new states
					markConsumerAfterMatching(ds_consumer, cur_node.id)
				}
			}
		}
		return true
	}

	// TODO: refactor producer/consumer handling, they are all the same code for different cases
	switch cur_node.typId {
	case NChannel:
		if cur_node.isSender { // find its consumer
			pts := g.getPTS(cur_node.ch_ptr, cur_node.cgnode)
			matchChannel(pts, false, cur_node.id)
		} else { // consumer
			matchConsumer(true)
		}
		break

	case NCaseDone, NCase:
		// when select is visited already,
		// in order to mark the next node of its select can walk, e.g., etcd6873
		select_node := cur_node.case_sel
		select_id := select_node.id
		ds.pause_ats[select_node.gid] = select_id
		ds.can_walk[select_id] = true // traverse later

		// e.g., kubernetes25331
		if !cur_node.isSender {
			producer_id, ok := ds.pairs[cur_node.id]
			if ok && producer_id >= 0 {
				ds.can_walk[producer_id] = true // inform the producer to go
			}
		}
		break

	case NSelect:
		// Note: only one select case can pair per time to simulate the real execution
		// (1) when select is in a loop, we only need to pick the one with exit/return, since picking it will block other case channels -> skip explore the states for other cases
		// (2) if select is not in a loop, we need to try all cases for different explores
		// (3) if a case is waiting for timeout from timeout.C channel, select this first to block other cases
		// (4) if a case is a sender, we will handle it when no receiver case is paired (no matter when the select is in a loop or not)
		//     if multiple senders and is possible, pick the sender without a consumer; otherwise, create a new state for each
		// TODO: what if multiple cases leading to return/exit of loop?
		//       we need to explore all of them, see which other case channels will be blocked
		//   if cur_node.is_in_loop {   && c.case_to_end { // marked by one of its producer
		// normal cases
		found_producer := false // this is a consumer op, and we already have its matched producer in ds.pairs
		found_consumer := false // vice versa
		found_default := false

		for _, c := range cur_node.cases {
			if c == nil {
				// empty cse
				continue
			}
			if c.case_state == nil { // default block: always the last case
				if (found_producer || found_consumer) && EXPLORE_ALL_STATES {
					// we copy a new ds and explore from there
					_ds := ds.copy(c.id)
					if g.isDuplicateState(_ds) {
						continue
					}
					g.states = append(g.states, _ds)
					_ds.can_walk[c.id] = true // inform the producer in a new state
					_ds.start_node_id = c.id
					_ds.start_tid = cur_tid
					// new state expr we already visit this case, starting from its kid
					continue
				}
				// take default
				ds.pause_ats[c.gid] = c.id
				ds.can_walk[c.id] = true // continue traversal with this default block
				// visit the case branch since it is not a kid of select
				traverse = g.visit(c, true, ds) // e.g., false -> true: kubernetes25331
				found_default = true
				continue
			}
			if c.isSender { // is producer
				pts := g.getPTS(c.ch_ptr, cur_node.cgnode)
				matchChannel(pts, found_consumer || found_producer, c.id)
				found_producer = true
				continue
			}
			// is consumer
			if (found_producer || found_consumer) && EXPLORE_ALL_STATES { // when > 1 cases in a select have paired producers
				if producer_id, paired := ds.pairs[c.id]; paired && producer_id >= 0 { // marked by one of its producer, already checked whether c has been visited or not
					// we copy a new ds and explore from there
					_ds := ds.copy(cur_node.id)
					if g.isDuplicateState(_ds) {
						continue
					}
					g.states = append(g.states, _ds)
					_ds.can_walk[producer_id] = true // inform the producer in a new state
					_ds.start_node_id = c.id
					_ds.start_tid = cur_tid
					//  new state expr: we already visit this case, starting from its kid
				}
				continue
			}
			// for 1st paired case
			if producer_id, paired := ds.pairs[c.id]; paired && producer_id >= 0 { // marked by one of its producer
				found_consumer = true
				ds.can_walk[producer_id] = true // inform the producer to continue
				// visit the case branch since it is not a kid of select
				traverse = g.visit(c, false, ds)
			}
			if isTimer(c) {
				// this is timeout channel -> it has no blocking
				if (found_producer || found_consumer) && EXPLORE_ALL_STATES {
					// we copy a new ds and explore from there
					_ds := ds.copy(cur_node.id)
					if g.isDuplicateState(_ds) {
						continue
					}
					g.states = append(g.states, _ds)
					_ds.start_node_id = c.id
					_ds.start_tid = cur_tid
					// new state expr: we already visit this case, starting from its kid
					continue
				}
				found_consumer = true
				// when this is the first match found in this select
				traverse = g.visit(c, false, ds)
				continue
			}
		}

		if !found_consumer && !found_producer && !found_default { // pause here
			if DEBUG {
				fmt.Println("?? Why no consumer or producer or default in select: " + cur_node.String())
			}
			// unvisit this select
			ds.visited[cur_node.id] = 0
			pauseHere(cur_node.id)
		}
		break

		// Note: NLock cannot lock after NRLock is acquired, NRLock cannot lock after NLock is acquired,
		//       but multiple goroutines can acquire NRLocks concurrently, but is not courage
		// TODO: for NLock and NRLock, count this as a successful lock only if holding-all-obj in pts,
		//   otherwise we revert the locks
	case NLock: // consumer
		pts := g.getPTS(cur_node.lock_ptr, cur_node.cgnode)
		held := cannotLock(pts) // check both rlock and wlock
		rheld := cannotRLock(pts)
		if held != nil {
			// pause here, cannot lock
			held_tid := held.gid
			if held_tid == cur_tid {
				// double lock: wlock before wlock, may block here
				tmp := make([]*Node, 2)
				tmp[0] = held
				tmp[1] = cur_node
				ds.doublelock[held_tid] = tmp
				pauseHere(cur_node.id)
				return traverse
			}

			two := ds.twolock[held_tid]
			if two != nil && two[1].id == cur_node.id { // it's already involved in a block
				if DEBUG {
					fmt.Printf("\t- cannot lock: w -> w (already locked)\n\t%v\n", two)
				}
				pauseHere(cur_node.id)
				break
			}

			// create a new state to make this lock happen e.g., etcd6873
			if EXPLORE_ALL_STATES && held.id >= 0 {
				createNewLockState(cur_node, pts, held)
			}

			// update the two locks
			two = make([]*Node, 2)
			two[0] = held
			two[1] = cur_node
			ds.twolock[held_tid] = two
			if DEBUG {
				fmt.Printf("\t- cannot lock: w -> w\n\t%v\n", two)
			}

			// inform the lockset who has the current lock to go
			paused_id := ds.pause_ats[held_tid]
			if paused_id >= 0 {
				ds.can_walk[paused_id] = true
				ds.visited[cur_node.id] = 0 // mark as unvisited
				pauseHere(cur_node.id)
			}
			break
		} else if rheld != nil {
			// pause here, cannot lock
			rheld_tid := rheld.gid
			if rheld_tid == cur_tid {
				// double lock: rlock before wlock, block here
				tmp := make([]*Node, 2)
				tmp[0] = rheld
				tmp[1] = cur_node
				ds.doublelock[rheld_tid] = tmp
				pauseHere(cur_node.id)
				return traverse
			}

			two := ds.twolock[rheld_tid]
			if two != nil && two[1].id == cur_node.id { // it's already involved in a block
				if DEBUG {
					fmt.Printf("\t- cannot lock: r -> w (already locked)\n\t%v\n", two)
				}
				pauseHere(cur_node.id)
				break
			}

			// create a new state to make this lock happen e.g., etcd6873
			if EXPLORE_ALL_STATES && rheld.id >= 0 {
				createNewLockState(cur_node, pts, rheld)
			}

			// update the two locks
			two = make([]*Node, 2)
			two[0] = rheld
			two[1] = cur_node
			ds.twolock[rheld_tid] = two
			if DEBUG {
				fmt.Printf("\t- cannot lock: r -> w\n\t%v\n", two)
			}

			// inform the lockset who has the current lock to go
			paused_id := ds.pause_ats[rheld_tid]
			if paused_id >= 0 {
				ds.can_walk[paused_id] = true
				ds.visited[cur_node.id] = 0 // mark as waiting
				pauseHere(cur_node.id)
			}
			break
		} else if _, paired := ds.pairs[cur_node.id]; paired {
			// matched. we can lock. lock them!
			for _, obj := range pts { // obj actually is objID
				ds.lockset[obj] = cur_node
			}
		} else { // no one is holding this lock, we can lock or others can lock too
			for _, obj := range pts { // obj actually is objID
				ds.lockset[obj] = cur_node
			}
		}
		if DEBUG {
			fmt.Println("\t- get lock")
		}
		delete(ds.doublelock, cur_tid)

		if g.noOtherLockOnResource(pts, cur_node) {
			break // no need to break, will not cause trouble
		}

		// pause at each lock when multiple threads have locks, try to trigger a state with deadlock
		// will come back after another thread obtains another lock.
		pauseHere(cur_node.id)
		break

	case NRLock: // consumer
		pts := g.getPTS(cur_node.lock_ptr, cur_node.cgnode)
		held := cannotLock(pts)
		rheld := cannotRLock(pts)
		if held != nil { // pause here, cannot lock
			held_tid := held.gid
			if held_tid == cur_tid {
				// wlock before rlock: double lock: block here
				tmp := make([]*Node, 2)
				tmp[0] = held
				tmp[1] = cur_node
				ds.doublelock[held_tid] = tmp
				pauseHere(cur_node.id)
				return traverse
			}

			two := ds.twolock[held_tid]
			if two != nil && two[1].id == cur_node.id { // it's already involved in a block
				if DEBUG {
					fmt.Printf("\t- cannot lock: w -> r (already locked)\n\t%v\n", two)
				}
				pauseHere(cur_node.id)
				break
			}

			// create a new state to make this lock happen
			if EXPLORE_ALL_STATES && held.id >= 0 {
				createNewLockState(cur_node, pts, held)
			}

			// update the two locks
			two = make([]*Node, 2)
			two[0] = held
			two[1] = cur_node
			ds.twolock[held_tid] = two
			if DEBUG {
				fmt.Printf("\t- cannot lock: w -> r\n\t%v\n", two)
			}

			// inform the lockset who has the current lock to go
			paused_id := ds.pause_ats[held_tid]
			if paused_id >= 0 {
				ds.can_walk[paused_id] = true
				ds.visited[cur_node.id] = 0 // mark as unvisited
				pauseHere(cur_node.id)
			}
			break
		} else if rheld != nil && rheld.gid == cur_tid {
			// current goroutine is holding the rlock, maybe a revisit of this rlock or held by other rlock
			// this is not encouraged
			doubles := ds.doublelock[cur_tid]
			if doubles != nil {
				// if another goroutine is waiting to hold a wlock, block here
				// update
				doubles = append(doubles, cur_node)
				ds.triplelocks[cur_tid] = doubles // triple

				pauseHere(cur_node.id)
				if DEBUG {
					fmt.Printf("\t- cannot lock: r -> w -> r\n\t%v\n", doubles)
				}
				break
			}

			if DEBUG {
				fmt.Println("\t- already lock (two rlocks, bad practice)")
			}
			break
		} else { // no one is holding this lock, we can lock or other can lock too
			for _, obj := range pts { // obj actually is objID
				ds.rlockset[obj] = cur_node
			}
		}
		if DEBUG {
			fmt.Println("\t- get lock")
		}
		delete(ds.doublelock, cur_tid)

		if g.noOtherLockOnResource(pts, cur_node) {
			break // no need to break, will not cause trouble
		}

		// pause at each lock when multiple threads have locks, try to trigger a state with deadlock
		// will come back after another thread obtains another lock.
		pauseHere(cur_node.id)
		break

	case NUnlock, NRUnlock: // producer
		pts := g.getPTS(cur_node.lock_ptr, cur_node.cgnode)
		// first check if we can unlock here
		unlocked_objs := 0
		for _, obj := range pts { // obj actually is objID
			if held, ok := ds.lockset[obj]; ok && cur_tid == held.gid {
				// if current thread is holding the lock obj
			} else {
				//panic("should not happen: cannot unlock missing n" + fmt.Sprintf("%d", obj))
				unlocked_objs++
			}
		}
		if unlocked_objs == len(pts) && cur_node.from_defer {
			// TODO: we may see many return with defer of unlock,
			//   we can skip them if we have non-defer unlock right after or we havent unlock yet
			break // skip for now
		}

		found_consumer := false
		for _, obj := range pts { // obj actually is objID
			// we are no longer holding this lock, delete it from the current lockset
			if held, ok := ds.lockset[obj]; ok && cur_tid == held.gid {
				delete(ds.lockset, obj)
			}
			delete(ds.twolock, cur_tid)

			// find the next lock
			if pcs, exist := g.resources[obj]; exist && cur_node.typId == NUnlock {
				// Iterate over potential producers (locks) waiting for this unlock
				possibles := g.pickConsumers(pcs.consumers, cur_tid, ds)
				for _, consumer := range possibles { // Assuming g.resources is a map that stores potential consumers for each unlock
					if found_consumer && EXPLORE_ALL_STATES {
						createNewState(consumer, cur_node.id)
						continue
					}
					// the 1st producer we see that is not the current lock/unlock pair
					// since a pair of lock/unlock are always in one function, aka, in one parent node, we check the parent node id
					if _, paired := ds.pairs[consumer.id]; !paired && ds.canHIP(consumer.gid, cur_tid) {
						found_consumer = true
						ds.can_walk[consumer.id] = true
						if DEBUG {
							fmt.Printf("\t- pairing p=%d with c=%d (current state)\n", cur_node.id, consumer.id)
						}
					}
				}
			}
		}
		if found_consumer { // pause here
			pauseHere(cur_node.id)
		}
		break

	case NCtxDone:
		matchConsumer(false)
		break

	case NCONDWait:
		matchConsumer(false)
		break

	case NSignal:
		if !ds.hasManyPausedWaits(g.id2node) {
			// delay the visit of signal and broadcast
			if DEBUG {
				fmt.Printf(" -- delay the visit of signal: %s\n", cur_node.String())
			}
			ds.visit_last[cur_tid] = cur_node.id
			pauseHere(cur_node.id)
			break
		}

		if DEBUG {
			fmt.Printf(" -- visit the delayed signal: %s\n", cur_node.String())
		}
		var ds_consumer *Node // we save the matched pc in current state at the end
		found_consumer := false

		pts := g.getPTS(cur_node.cond_ptr, cur_node.cgnode)
		for _, obj := range pts {
			if pcs, exist := g.resources[obj]; exist {
				possibles := g.pickConsumers(pcs.consumers, cur_tid, ds)
				for _, consumer := range possibles {
					if found_consumer && EXPLORE_ALL_STATES {
						createNewState(consumer, cur_node.id)
						continue
					}
					if _, paired := ds.pairs[consumer.id]; !paired && ds.canHIP(consumer.gid, cur_tid) {
						// from different threads
						found_consumer = true
						ds_consumer = consumer
						if DEBUG {
							fmt.Printf("\t- pairing p=%d with c=%d (current state)\n", cur_node.id, consumer.id)
						}
					}
				}
			}
		}

		if found_consumer { // update here so this pair/match stays in current ds, not pollute other _ds created for new states
			markConsumerAfterMatching(ds_consumer, cur_node.id)
		}
		// no pause here
		break

	// the following are a producer matching all consumers
	case NClose:
		pts := g.getPTS(cur_node.ch_ptr, cur_node.cgnode)
		for _, obj := range pts {
			matchAllConsumer(obj)
		}
		break // no blocking/waiting here

	case NBroadcast:
		pts := g.getPTS(cur_node.cond_ptr, cur_node.cgnode)
		for _, obj := range pts {
			matchAllConsumer(obj)
		}
		break // no blocking/waiting here

	case NCancel:
		// when you call the cancel function on a context created with context.WithCancel,
		// it not only cancels the current context but also cancels all the child contexts derived from it.
		// The cancellation signal is propagated to the child contexts and any goroutines associated with them,
		// but the parent context can continue executing.
		// moreover, it matches all consumers
		obj := cur_node.p.base_obj
		matchAllConsumer(obj)
		break // no blocking/waiting here

	// the following are for happens-before: SHIPS
	case NGoroutine:
		ds.hip = append(ds.hip, cur_node.gid) // gid == index of this goroutine in g.roots
		break

	case NWGWait:
		// if wait is in a loop, wait will be blocked forever
		if hasUnclosedLoop() {
			// we cannot terminate here, skip
			ds.pause_ats[cur_tid] = cur_node.id
			traverse = false
			break
		}

		matchConsumer(true)
		break

	case NExit:
		// check if exists any loops unclosed
		if hasUnclosedLoop() {
			// we cannot terminate here
			ds.pause_ats[cur_tid] = -3
			traverse = false
			break
		}

		// reach the end of this thread
		ds.pause_ats[cur_tid] = -2
		traverse = false
		if DEBUG {
			fmt.Println("*=> End of path")
		}

		// delete cur_tid from hip
		index := indexOf(ds.hip, cur_tid)
		if index != -1 {
			// Delete the element using slicing
			ds.hip = append(ds.hip[:index], ds.hip[index+1:]...)
			if DEBUG {
				fmt.Printf("groutine with tid = %d deleted. updated slice = %v\n", cur_tid, ds.hip)
			}
		} else {
			if DEBUG {
				fmt.Printf("groutine with tid = %d not found in the slice\n", cur_tid)
			}
			panic("X")
		}

		// if calls defer wg.Done() and all spawned goroutine completes, inform wg.Wait()
		if len(cur_node.wg_pts) > 0 {
			matchWGDone()
		}
		break

	case NWGDone: // if calls wg.Done() and all spawned goroutine completes, inform wg.Wait()
		cur_node.seen_done = true
		if matchWGDone() {
			// pause here to force possible blocking
			pauseHere(cur_node.id)
			break
		}

		// the following are for loops
	case NLoop:
		updateLoops(cur_node.loop_name, cur_node.enterLoop, cur_node)
		break

	case NROC:
		updateLoops(cur_node.roc_name, cur_node.enterROC, cur_node)
		break

	default: // move to next node
	}

	return traverse
}

// visit DFS return whether we continue the traversal on this thread
func (g *RFGraph) visit(cur_node *Node, skip bool, ds *detectionState) bool {
	ds.visited[cur_node.id] = 1
	if !skip && !g.visitNode(cur_node, ds) {
		return false
	}

	// top-down
	for _, v := range g.adjList[cur_node] {
		if _, ok := ds.visited[v.id]; !ok {
			// TODO: all siblings of this goroutine node can happen before of after this goroutine
			if v.typId == NGoroutine {
				g.updateHIP(v, ds) // gid == index of this goroutine in g.roots
				continue           // go to next node
			}
			if !g.visit(v, false, ds) {
				return false
			}
		}
	}

	return true
}

func (g *RFGraph) exploreRandomState(ds *detectionState, start_node_id int, cur_tid int) {
	if start_node_id != -1 {
		// continue from a walkable node
		// here we save the id of cur_node (already visited), to skip revisit, inform function visit()
		start_node := g.id2node[start_node_id]
		if DEBUG {
			fmt.Printf("\n*=> cur_node: %s (tid = %d)\n", start_node, cur_tid)
		}
		g.visit(start_node, true, ds)
	}

	//  update this order of thread visit to make it random except for the 1st round of visit
	is_visit_last := false
	prev_tid := cur_tid // initialize
	tid := cur_tid
	for {
		tid = ds.getRandomTID(tid, prev_tid) // update
		if tid == -1 {
			break
		}
		prev_tid = tid

		prepareLastVisit := func(node_id, n_tid int) {
			if node_id < 0 {
				return
			}
			ds.can_walk[node_id] = true
			ds.pause_ats[n_tid] = node_id
			is_visit_last = true
		}

		if ds.noPaused() || ds.noWalk() || ds.noPausedIsWalkable() {
			if ds.hasVisitLast() { // still can go
				if DEBUG {
					fmt.Printf(" -- going to visit ds.visit_last = %v\n", ds.visit_last)
				}
				for n_tid, node_id := range ds.visit_last {
					if node_id > 0 {
						prepareLastVisit(node_id, n_tid)
					}
				}
			} else if rnode_id, rn_tid := ds.holdingRLock(g.id2node); rn_tid != -1 && rnode_id != -1 {
				// check rlock first to see if there is RWR deadlock
				if DEBUG {
					node := g.id2node[rnode_id]
					fmt.Printf(" -- going to continue on rlock = %s\n", node.String())
				}
				prepareLastVisit(rnode_id, rn_tid)
			} else if node_id, n_tid := ds.holdingLock(g.id2node); n_tid != -1 && node_id != -1 {
				if DEBUG {
					node := g.id2node[node_id]
					fmt.Printf(" -- going to continue on lock = %s\n", node.String())
				}
				prepareLastVisit(node_id, n_tid)
			} else if unode_id, un_tid := ds.noPausedWaits(g.id2node); un_tid != -1 && unode_id != -1 {
				if DEBUG {
					node := g.id2node[unode_id]
					fmt.Printf(" -- going to continue on lock = %s\n", node.String())
				}
				prepareLastVisit(unode_id, un_tid)
			} else {
				break // no more walkable Nodes in any threads
			}
			if is_visit_last {
				ds.visit_last = make([]int, len(g.roots)) // reset
			}
		} else {
			if is_visit_last {
				is_visit_last = false
			}
		}

		cur_node_id := ds.pause_ats[tid]
		cur_node := g.id2node[cur_node_id]
		if cur_node == nil {
			if DEBUG {
				fmt.Printf("\n!=> cur_node: %s (tid = %d) cannot walk, skip\n", cur_node, tid)
			}
			continue
		}

		can, ok := ds.can_walk[cur_node_id]
		if ok && can {
			if is_visit_last && (ds.pause_ats[tid] == -2 || ds.pause_ats[tid] == -3) {
				continue // already done with tid
			}
			ds.can_walk[cur_node_id] = false
			ds.pause_ats[tid] = -1
		} else if (cur_node.typId == NLock || cur_node.typId == NRLock) && g.onlyICanWalk(tid, ds) {
			ds.can_walk[cur_node_id] = false
			ds.pause_ats[tid] = -1
		} else {
			if DEBUG {
				fmt.Printf("\n!=> cur_node: %s (tid = %d) cannot walk, skip\n", cur_node, tid)
			}
			continue // no matched producer or consumer yet, or reached the end
		}
		if cur_node.gid != tid {
			panic(fmt.Sprintf("cur_node.gid = %d but tid = %d\n", cur_node.gid, tid))
		}
		if DEBUG {
			fmt.Printf("\n*=> cur_node: %s (tid = %d)\n", cur_node, tid)
		}

		// walk this thread in rfg:
		// we cannot skip this is because we haven't handled this NWGWait when we pause there,
		// for other types of nodes in is_visit_last, we already handled them then pause at the node
		if cur_node.typId == NLock || cur_node.typId == NRLock {
			done_visit := ds.visited[cur_node.id]
			if done_visit == 1 {
				g.visit(cur_node, true, ds)
			} else {
				g.visit(cur_node, false, ds)
			}
		} else if cur_node.typId == NWGWait || cur_node.typId == NCase || cur_node.typId == NCaseDone ||
			is_visit_last {
			g.visit(cur_node, false, ds)
		} else {
			g.visit(cur_node, true, ds)
		}
	}
}

func (g *RFGraph) exploreOrderState(ds *detectionState, start_node_id int, cur_tid int) {
	if start_node_id != -1 {
		// continue from a walkable node
		// here we save the id of cur_node (already visited), to skip revisit, inform function visit()
		start_node := g.id2node[start_node_id]
		g.visit(start_node, true, ds)
	}

	// check all threads
	i := 0
	if cur_tid != -1 {
		i = (cur_tid + 1) % len(ds.hip) // starting from next thread
	}

	//  update this order of thread visit to make it random except for the 1st round of visit
	is_visit_last := false
	for ; ; i++ {
		if ds.noPaused() || ds.noWalk() || ds.noPausedIsWalkable() {
			if ds.hasVisitLast() {
				// still can go
				if DEBUG {
					fmt.Printf(" -- going to visit ds.visit_last = %v\n", ds.visit_last)
				}
				for tid, node_id := range ds.visit_last {
					if node_id >= 0 {
						ds.can_walk[node_id] = true
						ds.pause_ats[tid] = node_id
						is_visit_last = true
					}
				}
			} else {
				break // no more walkable Nodes in any threads
			}
			if is_visit_last {
				ds.visit_last = make([]int, len(g.roots)) // reset
			}
		} else {
			if is_visit_last {
				is_visit_last = false
			}
		}
		if i >= len(ds.hip) {
			i = 0 // restart from the first root
		}
		tid := ds.hip[i]

		cur_node_id := ds.pause_ats[tid]
		cur_node := g.id2node[cur_node_id]
		if cur_node_id < 0 || cur_node == nil {
			if DEBUG {
				fmt.Printf("\n!=> cur_node: %s (tid = %d) cannot walk, skip\n", cur_node, tid)
			}
			continue
		}

		can, ok := ds.can_walk[cur_node_id]
		if ok && can {
			if is_visit_last && (ds.pause_ats[tid] == -2 || ds.pause_ats[tid] == -3) {
				continue // already done with tid
			}
			ds.can_walk[cur_node_id] = false
			ds.pause_ats[tid] = -1
		} else if (cur_node.typId == NLock || cur_node.typId == NRLock) && g.onlyICanWalk(tid, ds) {
			ds.can_walk[cur_node_id] = false
			ds.pause_ats[tid] = -1
		} else {
			if DEBUG {
				fmt.Printf("\n!=> cur_node: %s (tid = %d) cannot walk, not my turn\n", cur_node, tid)
			}
			continue // no matched producer or consumer yet, or reached the end
		}
		if cur_node.gid != tid {
			panic(fmt.Sprintf("cur_node.gid = %d but tid = %d\n", cur_node.gid, tid))
		}
		if DEBUG {
			fmt.Printf("\n*=> cur_node: %s (tid = %d)\n", cur_node, tid)
		}

		// walk this thread in rfg:
		// we cannot skip this is because we haven't handled this NWGWait when we pause there,
		// for other types of nodes in is_visit_last, we already handled them then pause at the node
		if cur_node.typId == NWGWait || cur_node.typId == NCase || cur_node.typId == NCaseDone ||
			is_visit_last {
			g.visit(cur_node, false, ds)
		} else {
			g.visit(cur_node, true, ds)
		}
	}
}

// exploreState if no given cur_node_id (i.e., cur_node_id != -1), we are exploring from roots of rfg
// otherwise, explore from given cur_node_id
// cur_tid -> the i for cur_node_id when cur_node_id != -1
func (g *RFGraph) exploreState(ds *detectionState, start_node_id int, cur_tid int) {
	if RANDOM_STATE {
		g.exploreRandomState(ds, start_node_id, cur_tid)
	} else {
		g.exploreOrderState(ds, start_node_id, cur_tid)
	}
}

// updateHIP for a node of type NGoroutine
func (g *RFGraph) updateHIP(gnode *Node, ds *detectionState) {
	ds.hip = append(ds.hip, gnode.gid)
	ds.pause_ats[gnode.gid] = gnode.id
	if gnode.id < 0 {
		return
	}
	ds.can_walk[gnode.id] = true
	ds.visited[gnode.id] = 1 // mark visited
	if DEBUG {
		fmt.Printf("groutine with tid = %d seen. updated slice = %v\n", gnode.gid, ds.hip)
	}
}

func (g *RFGraph) DFSDetectBlockingBugs() {
	if DEBUG {
		fmt.Println("\n\n*********************************** ")
		fmt.Println("Detecting by finding matching producers & consumers ... ")
	}

	// 1st state: initialize
	ds := &detectionState{
		g:                g,
		splitting_points: make([]int, 0),
		pause_ats:        make([]int, len(g.roots)),
		can_walk:         make(map[int]bool),
		pairs:            make(map[int]int),
		lockset:          make(map[int]*Node),
		rlockset:         make(map[int]*Node),
		doublelock:       make(map[int][]*Node),
		twolock:          make(map[int][]*Node),
		triplelocks:      make(map[int][]*Node),
		visited:          make(map[int]int),
		visit_loops:      make(map[string]*Node),
		visit_last:       make([]int, len(g.roots)),
		hip:              make([]int, 0),
		start_node_id:    0,
		start_tid:        0,
	}
	for i := 0; i < len(ds.pause_ats); i++ {
		ds.pause_ats[i] = -1
	}
	// traversal starting from main
	m := g.roots[0]
	g.updateHIP(m, ds)
	g.states = append(g.states, ds)
	results := make([]*detectionState, 0) // store computed states

	// explore
	j := 0
	for len(g.states) > 0 {
		_ds := g.states[0]
		g.states = g.states[1:] // pop
		if DEBUG {
			fmt.Println(strings.Repeat("~", 80))
			fmt.Printf("~~~ exploring a new state (%d): split points = %v\tpairs = %v\n", j, _ds.splitting_points, _ds.pairs)
		}
		g.exploreState(_ds, _ds.start_node_id, _ds.start_tid)
		if DEBUG {
			fmt.Printf("\n~~~ end the current state(%d): splitting points = %v\tpairs = %v\n\n", j, _ds.splitting_points, _ds.pairs)
		}
		results = append(results, _ds)
		j++
	}
	g.states = results

	// rough filter out result
	fmt.Println("\n\n*********************************** ")
	fmt.Println("Filtering Result ... ")
	for _, s := range results {
		if s.noPaused() && len(s.visit_loops) == 0 { // only print out the state when blocking bugs have been detected
			continue
		}

		if s.hasMeaningfulPaused() {
			berr := &BlockError{
				tid2loc: make(map[int]string),
				s:       s,
			}
			for _, id := range s.pause_ats {
				if id == -1 {
					// entering a loop without exit
					continue
				}

				if id != -2 && id != -3 {
					node := g.id2node[id]
					loc, _ := g.getLocAndStmt(node, s)
					berr.tid2loc[id] = loc.String()
				}
			}
			g.berrs = append(g.berrs, berr)
		}
	}

	if DEBUG_EACH_RESULT { // print out result immediately without removing redundancy
		fmt.Println("\n\n*********************************** ")
		fmt.Println("Detection Result: ")
		for i, s := range results {
			if s.noPaused() && len(s.visit_loops) == 0 { // only print out the state when blocking bugs have been detected
				continue
			}

			if s.hasMeaningfulPaused() {
				berr := &BlockError{
					s: s,
				}
				fmt.Println("=========================================")
				fmt.Printf("Detection State %d (Blocking)\n", i)
				fmt.Printf("splitting points: %v\tmatched nodes: %v\n", s.splitting_points, s.pairs)
				for j, id := range s.pause_ats {
					if id == -1 {
						// entering a loop without exit
						continue
					}

					if id != -2 && id != -3 {
						ctx := g.roots[j].cgnode.GetContext()
						node := g.id2node[id]
						loc, stmt := g.getLocAndStmt(node, s)
						berr.tid2loc[id] = loc.String()
						fmt.Printf("tid = %d. Blocked@%s Thread (%s)\n-> Statement %s\n@Location %s\n\n", j, node, ctx, stmt, loc)
					}
				}
				g.berrs = append(g.berrs, berr)
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
					loc, stmt := g.getSourceCode(pos)
					pause_at := g.findWhereLoopPausedAt(node.gid, node, s) // find where loop paused at
					if pause_at == nil {
						fmt.Printf("Cannot exit the %s\n-> Statement %s\n@Location %s\n\n", loop_name, stmt, loc)
						continue
					}
					loc2, stmt2 := g.getLocAndStmt(pause_at, s)
					fmt.Printf("Cannot exit the %s\n-> Statement %s\n@Location %s\npaused at\n-> Statement %s\n@Location %s\n\n", loop_name, stmt, loc, stmt2, loc2)
				}
			}
		}

		if len(g.cferrs) == 0 {
			fmt.Println("*********************************** ")
			return
		}
		fmt.Println("\n\n*********************************** ")
		fmt.Println("Detection Result (Unexpected Exit of Control Flow): ")
		pos2err := make(map[token.Pos]*CFError)
		for _, err := range g.cferrs { // filter out redundant errs
			if _, ok := pos2err[err.inst_pos]; ok {
				continue
			}
			pos2err[err.inst_pos] = err
		}

		i := 0
		for _, err := range pos2err {
			fmt.Printf("%d.\n", i)
			i++
			pos := err.inst_pos
			if err.inst_unlock_pos != token.NoPos {
				pos = err.inst_unlock_pos
			}
			loc, stmt := g.getSourceCode(pos)
			loc2, stmt2 := g.getSourceCode(err.interupt_pos)
			if stmt2 == "" {
				stmt2 = err.stmt_str
			}
			fmt.Printf("Statement %s\n@Location %s\ncannot reach the exit of function due to premature termination at:\nStatement %s\n@Location %s\n",
				stmt, loc, stmt2, loc2)
			fmt.Println("-----------------------------------------")
		}

		fmt.Println("\n\n*********************************** ")
	}
}

func (g *RFGraph) findWhereLoopPausedAt(tid int, loop_enter *Node, ds *detectionState) *Node {
	stack := make([]*Node, 1)
	stack[0] = loop_enter
	for len(stack) > 0 {
		cur_node := stack[0]
		stack = stack[1:] // pop

		if ds.visited[cur_node.id] == 1 {
		} else {
			return cur_node
		}
		for _, kid := range g.adjList[cur_node] {
			if kid.gid != tid {
				continue
			}
			if kid.typId == NSelect {
				for _, c := range kid.cases {
					stack = append(stack, c)
				}
			}
			stack = append(stack, kid)
		}
	}
	return nil
}

func (g *RFGraph) getLocAndStmt(node *Node, ds *detectionState) (token.Position, string) {
	var pos token.Pos
	switch node.typId {
	case NChannel:
		if node.isSender {
			pos = node.ch_send_inst.Pos()
		} else {
			pos = node.ch_rec_inst.Pos()
		}
	case NLock, NRLock:
		switch inst := node.lock_inst.(type) {
		case *ssa.Call, *ssa.Defer:
			pos = inst.Pos()
		default:
			fmt.Println("Wrong type of instruction in node.")
			panic("X")
		}

	case NSelect:
		for _, c := range node.cases {
			for k, v := range ds.pairs {
				if c.id == v || c.id == k {
					if c.case_state == nil {
						pos = c.default_inst.Pos()
						break
					}
					pos = c.case_state.Pos
					break
				}
			}
		}
		if pos == token.NoPos { // pick the first case
			pos = node.cases[0].case_state.Pos
		}

	case NCase:
		if node.case_state == nil {
			pos = node.default_inst.Pos()
			break
		}
		pos = node.case_state.Pos

	case NCaseDone:
		pos = node.done_inst.Pos()

	case NCtxDone:
		pos = node.done_inst.Pos()

	case NWGWait, NCONDWait:
		pos = node.wait_inst.Pos()

	case NWGDone:
		pos = node.wg_done_inst.Pos()

	case NReturn:
		pos = node.ret_inst.Pos()

	case NUnlock, NRUnlock:
		pos = node.unlock_inst.Pos()

	case NClose:
		pos = node.ch_close_inst.Pos()

	default: // TODO: implement in showing result
		fmt.Printf("TODO: implement in showing result: typeid = %v\n", node.typId)
	}

	return g.getSourceCode(pos)
}

func (g *RFGraph) getSourceCode(pos token.Pos) (token.Position, string) {
	loc := g.prog.Fset.Position(pos)
	stmt, _ := getLineNumber(loc.Filename, loc.Line)
	return loc, stmt
}

func getLineNumber(filePath string, lineNum int) (string, error) {
	sourceFile, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(sourceFile)
	lineStr := ""
	for i := 1; scanner.Scan(); i++ {
		if i == lineNum {
			lineStr = scanner.Text()
			break
		}
	}
	err = sourceFile.Close()
	if err != nil {
		return "", err
	}
	return lineStr, err
}

// getCaseBBs for select-case: collect all cases and its consecutive bbs
// TODO: select.done here might need other handling
func getCaseBBs(select_bb *ssa.BasicBlock, h *creatorHelper, is_blocking bool) map[int][]*ssa.BasicBlock {
	// find all "select.body" for cases
	stack := make([]*ssa.BasicBlock, 1)
	bodies := make([]*ssa.BasicBlock, 0) // "select.body", we may see empty body in test cases
	stack[0] = select_bb
	for len(stack) > 0 {
		s := stack[0]
		stack = stack[1:] // pop
		h.skips[s] = true
		// this might be the last select.next block, which can be panic block or default block
		if s.Comment == "select.next" || s.Comment == "select.done" {
			l := len(s.Instrs)
			if _, ok := s.Instrs[l-1].(*ssa.Panic); ok && l == 2 {
				continue // blocking panic bb
			} else if _, ok := s.Instrs[l-1].(*ssa.If); ok && l == 2 {
				// normal select.next, e.g.,
				//   t12 = t10 == 1:int       bool
				//	 if t12 goto 4 else 5
			} else if !is_blocking {
				// has and is default block, which is the last bb to collect
				bodies = append(bodies, s)
				continue
			}
		}

		if len(s.Succs) == 0 {
			continue
		}

		body := s.Succs[0]
		if body.Comment == "select.body" {
			bodies = append(bodies, body)
		} else { // empty body, e.g., cockroach10790
			bodies = append(bodies, nil)
		}
		if len(s.Succs) > 1 {
			next := s.Succs[1]
			if next.Comment == "select.next" || next.Comment == "select.done" {
				stack = append(stack, s.Succs[1]) // push
			}
		}
	}

	// collect all success bbs for each case
	case_bodies := make(map[int][]*ssa.BasicBlock)
	for i, body := range bodies {
		if body == nil {
			case_bodies[i] = nil
			continue
		}

		tmp := make([]*ssa.BasicBlock, 1)
		tmp[0] = body

		// successors
		succs := body.Succs
		for len(succs) > 0 {
			succ := succs[0]
			succs = succs[1:] // pop
			if succ.Index > select_bb.Index && !h.skips[succ] {
				// visit here, only move forward: when bb idxes become larger
				// if jump back, it is the end of current case
				h.skips[succ] = true
				tmp = append(tmp, succ)
				succs = append(succs, succ.Succs...) // push
			}
		}

		case_bodies[i] = tmp
	}

	if DEBUG {
		fmt.Printf("** select @ bb = %d\t", select_bb.Index)
		for i, bbs := range case_bodies {
			fmt.Printf("case %d: include bb = ", i)
			if bbs == nil {
				fmt.Print("empty body; ")
				continue
			}
			for _, bb := range bbs {
				fmt.Printf("%d, ", bb.Index)
			}
			fmt.Print("; ")
		}
		fmt.Println()
	}

	return case_bodies
}

// getROCBBs for ROC: collect all bbs inside a ROC loop
func getROCBBs(roc_bb *ssa.BasicBlock, h *creatorHelper) []*ssa.BasicBlock {
	body := roc_bb.Succs[0] // one is "rangechan.body", the other is "rangechan.done"
	if body.Comment != "rangechan.body" {
		body = roc_bb.Succs[1]
	}
	h.skips[body] = true
	ret := make([]*ssa.BasicBlock, 1)
	ret[0] = body

	// successors
	succs := body.Succs
	for len(succs) > 0 {
		succ := succs[0]
		succs = succs[1:]              // pop
		if succ.Comment == "recover" { // skip
			continue
		}
		if succ.Index > roc_bb.Index && !h.skips[succ] {
			// visit here, only move forward: when bb idxes become larger
			// if jump back, it is the end of current case
			h.skips[succ] = true
			ret = append(ret, succ)
			succs = append(succs, succ.Succs...) // push
		}
	}

	return ret
}

// indexOf returns the index of an element in a slice, or -1 if not found.
func indexOf(slice []int, element int) int {
	for i, v := range slice {
		if v == element {
			return i
		}
	}
	return -1
}
