package concurrency

import (
	"fmt"
	"github.com/bozhen-liu/gopa/go/ssa"
	"reflect"
	"sort"
)

func IntArrayEquals(a []int, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	// Sort both arrays
	sort.Ints(a)
	sort.Ints(b)

	// Compare the sorted slices
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func IntMapEquals(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}

	for key, value1 := range a {
		if value2, ok := b[key]; ok {
			if !reflect.DeepEqual(value1, value2) {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

// IntArrayContains  a contains b
func IntArrayContains(a []int, b []int) bool {
	// Create maps to count element occurrences
	countMap1 := make(map[int]int)
	countMap2 := make(map[int]int)

	// Count occurrences in arr1
	for _, num := range a {
		countMap1[num]++
	}

	// Count occurrences in arr2
	for _, num := range b {
		countMap2[num]++
	}

	// Check if countMap1 contains all elements of countMap2
	for num, count := range countMap2 {
		if countMap1[num] < count {
			return false
		}
	}
	return true
}

// IntMapContains a contains b
func IntMapContains(a, b map[int]int) bool {
	for key, value := range a {
		// Check if the key exists in a and has the same value
		val, ok := b[key]
		if !ok {
			continue
		}
		if val != value {
			return false
		}
	}
	// All key-value pairs in b were found in a
	return true
}

func isInInstArray(a []*ssa.Return, b ssa.Instruction) bool {
	for _, v := range a {
		if v.Block().Index == b.Block().Index { // TODO: or compare inst?
			return true
		}
	}
	return false
}

func isInCallArray(a []*ssa.Call, b ssa.Instruction) bool {
	for _, v := range a {
		if v.Block().Index == b.Block().Index { // TODO: or compare inst?
			return true
		}
	}
	return false
}

func isInIntArray(a []ssa.Instruction, b int) bool {
	for _, v := range a {
		if v.Block().Index == b {
			return true
		}
	}
	return false
}

func hasRepeatState(points []int) bool {
	size := len(points)
	if size < 4 {
		return false
	}
	last2 := points[size-2:]
	llast2 := points[size-4 : size-2]
	return fmt.Sprint(last2) == fmt.Sprint(llast2)
}
