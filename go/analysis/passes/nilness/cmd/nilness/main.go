// The nilness command applies the github.com/april1989/origin-go-tools/go/analysis/passes/nilness
// analysis to the specified packages of Go source code.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/passes/nilness"
	"github.com/bozhen-liu/gopa/go/analysis/singlechecker"
)

func main() { singlechecker.Main(nilness.Analyzer) }
