// The shadow command runs the shadow analyzer.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/passes/shadow"
	"github.com/bozhen-liu/gopa/go/analysis/singlechecker"
)

func main() { singlechecker.Main(shadow.Analyzer) }
