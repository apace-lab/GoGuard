// The findcall command runs the findcall analyzer.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/passes/findcall"
	"github.com/bozhen-liu/gopa/go/analysis/singlechecker"
)

func main() { singlechecker.Main(findcall.Analyzer) }
