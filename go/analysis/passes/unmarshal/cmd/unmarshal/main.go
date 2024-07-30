// The unmarshal command runs the unmarshal analyzer.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/passes/unmarshal"
	"github.com/bozhen-liu/gopa/go/analysis/singlechecker"
)

func main() { singlechecker.Main(unmarshal.Analyzer) }
