package concurrency

import (
	"fmt"
	"github.com/bozhen-liu/gopa/go/pointer"
	"github.com/bozhen-liu/gopa/go/ssa"
	"go/token"
	"strings"
)

func InitializeCFChecker(cur_cgnode *pointer.Node, g *RFGraph) *CFChecker {
	return &CFChecker{
		cgnode: cur_cgnode,
		g:      g,
	}
}

// CFChecker = ControlflowChecker
type CFChecker struct {
	cgnode *pointer.Node
	g      *RFGraph
}

type checkpoint struct {
	inst     ssa.Instruction
	start    int   // which bb does this inst in
	pts      []int // when from lock to unlock: locked objs
	is_defer bool

	// to report the result
	inst_pos token.Pos // unlock or Done() or broadcast
}

// CFError return result
type CFError struct {
	g               *RFGraph
	inst_pos        token.Pos
	inst_unlock_pos token.Pos
	interupt_pos    token.Pos
	stmt_str        string
}

// CheckCompleteFlow discover unexpected exit of control flow:
// * checkpoints
// 1. call to Done() in waitgroup
// 2. defer statement/function
// 3. lock unlock
// 4. (*sync.Cond).Broadcast() : they may exist in a branch that somehow cannot always be triggered
// verify that every control flow within the enclosing code block
// executes Done(), defer, and unlock before leaving the block
// -> check if any panic (for defer) or return (for all) in between
func (c *CFChecker) CheckCompleteFlow() []*CFError {
	bbs := c.cgnode.GetCGNode().GetFunc().Blocks
	checkpoints := make([]*checkpoint, 0)
	outs := make([]ssa.Instruction, 0) // all the bb with returns and panics in this fn
	pts2lock := make(map[string]int)   // lock pts -> index in checkpoints

	markLock := func(inst ssa.Instruction, Call ssa.CallCommon) {
		key := Call.Args[0]
		pts := c.g.getPTS(key, c.cgnode)
		p := &checkpoint{
			inst:     inst,
			start:    inst.Block().Index,
			pts:      pts,
			inst_pos: inst.Pos(),
		}
		checkpoints = append(checkpoints, p)

		str := fmt.Sprintf("%v", pts)
		pts2lock[str] = len(checkpoints) - 1
	}

	markUnlock := func(inst ssa.Instruction, Call ssa.CallCommon, is_defer bool) {
		key := Call.Args[0]
		pts := c.g.getPTS(key, c.cgnode)
		p := &checkpoint{
			inst:     inst,
			start:    inst.Block().Index,
			pts:      pts,
			inst_pos: inst.Pos(),
			is_defer: is_defer,
		}
		checkpoints = append(checkpoints, p)

		str := fmt.Sprintf("%v", pts)
		if idx, ok := pts2lock[str]; ok {
			checkpoints = append(checkpoints[:idx], checkpoints[idx+1:]...)
		}
	}

	markWGDone := func(inst ssa.Instruction, call ssa.CallCommon, is_defer bool) {
		if strings.Contains(call.Args[0].Type().String(), "sync.WaitGroup") { // or *sync.WaitGroup?
			p := &checkpoint{
				inst:     inst,
				start:    inst.Block().Index,
				is_defer: is_defer,
				inst_pos: inst.Pos(),
			}
			checkpoints = append(checkpoints, p)
		}
	}

	markBroadcast := func(inst ssa.Instruction, call ssa.CallCommon) {
		p := &checkpoint{
			inst:     inst,
			start:    inst.Block().Index,
			is_defer: false,
			inst_pos: inst.Pos(),
		}
		checkpoints = append(checkpoints, p)
	}

	for _, cur_bb := range bbs {
		for _, instr := range cur_bb.Instrs {
			switch inst := instr.(type) {
			case *ssa.Call:
				// check if it is a lock/unlock
				Call := inst.Call
				name := Call.Value.Name()
				switch name {
				case "Lock", "RLock":
					markLock(inst, Call)
					break

				//case "Unlock", "RUnlock":
				//	markUnlock(inst, Call, false)
				//	break

				case "Done": // sync.WaitGroup.Done()
					markWGDone(inst, Call, false)
					break

				case "Broadcast", "Signal": // (*sync.Cond).Broadcast, e.g., moby27782
					markBroadcast(inst, Call)
					break
				}

			case *ssa.Defer:
				dCall := inst.Call
				name := dCall.Value.Name()
				if name == "Unlock" || name == "RUnlock" {
					markUnlock(inst, dCall, true)
				} else if name == "Done" {
					markWGDone(inst, dCall, true)
				}
				break

			case *ssa.Return, *ssa.Panic:
				outs = append(outs, inst)
				break
			}
		}
	}

	ret := make([]*CFError, 0)
	for _, cp := range checkpoints {
		if e, ok := c.canReachFromAllPaths(cp, outs, bbs); !ok {
			e.inst_pos = cp.inst_pos
			ret = append(ret, e)
		}
	}
	return ret
}

// canReachFromAllPaths to check if 'target' can be reached from 'start' through all paths
// we only check lock without defer unlock, and defer unlock here
func (c *CFChecker) canReachFromAllPaths(cp *checkpoint, outs []ssa.Instruction, bbs []*ssa.BasicBlock) (*CFError, bool) {
	start := cp.start // bb idx of lock or inst
	is_defer := cp.is_defer
	is_lock := cp.pts != nil

	reached := make([]*ssa.Return, 0) // can reach which out bb
	unlocks := make([]*ssa.Call, 0)   // can reach which unlock bb
	visited := make(map[int]bool)     // visited basic blocks
	queue := []int{start}             // visit starts from start

	for i, _ := range bbs { // mark unvisited
		visited[i] = false
	}

	visited[start] = true
	for len(queue) > 0 {
		cur_bb_idx := queue[0]
		queue = queue[1:]
		cur_bb := bbs[cur_bb_idx]

		if cur_bb.Comment == "recover" {
			continue
		}

		in := isInIntArray(outs, cur_bb_idx) // one out block has been reached.
		for _, instr := range cur_bb.Instrs {
			switch inst := instr.(type) {
			case *ssa.Call:
				if (in && (is_lock && !is_defer)) ||
					(is_defer || (is_lock && !is_defer)) { // collect when the bb that in between cp and out
					// check if it is a lock/unlock
					Call := inst.Call
					name := Call.Value.Name()
					switch name {
					case "Unlock", "RUnlock":
						key := Call.Args[0]
						pts := c.g.getPTS(key, c.cgnode)
						if IntArrayEquals(pts, cp.pts) {
							// matched unlock
							if cur_bb_idx == cp.start {
								//if there is panic between lock and unlock, it should already reported
								// they belong to the same bb, should not have any problem
								return nil, true
							} else {
								// if not using defer, we should see unlock on every exit
								// instead of collecting return, we collect unlock
								unlocks = append(unlocks, inst)
							}
						}
						break
					}
				}

				// check if any inst is panic or return
			case *ssa.Panic:
				if is_defer || (is_lock && !is_defer) { // collect when the bb that in between cp and out
					e := &CFError{
						g:            c.g,
						inst_pos:     cp.inst_pos,
						interupt_pos: inst.Pos(),
						stmt_str:     inst.String(),
					}
					return e, false // cannot reach the target
				}

			case *ssa.Return:
				if (is_lock && !is_defer) && len(unlocks) == 0 {
					e := &CFError{
						g:            c.g,
						inst_pos:     cp.inst_pos,
						interupt_pos: inst.Pos(),
						stmt_str:     inst.String(),
					}
					return e, false // cannot reach the target
				}
				reached = append(reached, inst)
			}
		}

		// for next round
		for _, neighbor := range cur_bb.Succs {
			idx := neighbor.Index
			if !visited[idx] {
				queue = append(queue, idx)
				visited[idx] = true
			} else if idx == start && (is_lock && !is_defer) {
				// we see the lock again without unlock in a loop
				insts := cur_bb.Instrs
				last_inst := insts[len(insts)-1]
				e := &CFError{
					g:            c.g,
					inst_pos:     cp.inst_pos,
					interupt_pos: last_inst.Pos(), // last stmt in this bb
					stmt_str:     last_inst.String() + " (loop)",
				}
				return e, false // cannot reach the target
			}
		}
	}

	if is_defer { // no panic triggered
		return nil, true
	}

	if is_lock {
		// Check if all unlocks are visited. If yes, return true.
		if len(unlocks) == len(outs) {
			return nil, true
		}

		for _, out := range outs {
			if isInCallArray(unlocks, out) || c.happenBeforeLock(cp.inst, out) {
				// reached this exit
			} else {
				e := &CFError{
					g:            c.g,
					inst_pos:     cp.inst_pos,
					interupt_pos: out.Pos(),
					stmt_str:     out.String(),
				}
				return e, false // cannot reach the target
			}
		}
		return nil, true
	}

	// Check if all outs are visited. If yes, return true.
	if len(reached) == len(outs) {
		return nil, true
	}

	for _, out := range outs {
		if isInInstArray(reached, out) {
			// reached this exit
		} else {
			e := &CFError{
				g:            c.g,
				inst_pos:     cp.inst_pos,
				interupt_pos: out.Pos(),
				stmt_str:     out.String(),
			}
			return e, false // cannot reach the target
		}
	}

	return nil, true
}

// happenBeforeLock out happens before start
func (c *CFChecker) happenBeforeLock(inst ssa.Instruction, out ssa.Instruction) bool {
	loc, _ := c.g.getSourceCode(inst.Pos())
	loc2, _ := c.g.getSourceCode(out.Pos())
	return inst.Block().Index > out.Block().Index || loc.Line > loc2.Line
}
