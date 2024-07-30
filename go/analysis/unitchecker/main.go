// +build ignore

// This file provides an example command for static checkers
// conforming to the github.com/april1989/origin-go-tools/go/analysis API.
// It serves as a model for the behavior of the cmd/vet tool in $GOROOT.
// Being based on the unitchecker driver, it must be run by go vet:
//
//   $ go build -o unitchecker main.go
//   $ go vet -vettool=unitchecker my/project/...
//
// For a checker also capable of running standalone, use multichecker.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/unitchecker"

	"github.com/bozhen-liu/gopa/go/analysis/passes/asmdecl"
	"github.com/bozhen-liu/gopa/go/analysis/passes/assign"
	"github.com/bozhen-liu/gopa/go/analysis/passes/atomic"
	"github.com/bozhen-liu/gopa/go/analysis/passes/bools"
	"github.com/bozhen-liu/gopa/go/analysis/passes/buildtag"
	"github.com/bozhen-liu/gopa/go/analysis/passes/cgocall"
	"github.com/bozhen-liu/gopa/go/analysis/passes/composite"
	"github.com/bozhen-liu/gopa/go/analysis/passes/copylock"
	"github.com/bozhen-liu/gopa/go/analysis/passes/errorsas"
	"github.com/bozhen-liu/gopa/go/analysis/passes/httpresponse"
	"github.com/bozhen-liu/gopa/go/analysis/passes/loopclosure"
	"github.com/bozhen-liu/gopa/go/analysis/passes/lostcancel"
	"github.com/bozhen-liu/gopa/go/analysis/passes/nilfunc"
	"github.com/bozhen-liu/gopa/go/analysis/passes/printf"
	"github.com/bozhen-liu/gopa/go/analysis/passes/shift"
	"github.com/bozhen-liu/gopa/go/analysis/passes/stdmethods"
	"github.com/bozhen-liu/gopa/go/analysis/passes/structtag"
	"github.com/bozhen-liu/gopa/go/analysis/passes/tests"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unmarshal"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unreachable"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unsafeptr"
	"github.com/bozhen-liu/gopa/go/analysis/passes/unusedresult"
)

func main() {
	unitchecker.Main(
		asmdecl.Analyzer,
		assign.Analyzer,
		atomic.Analyzer,
		bools.Analyzer,
		buildtag.Analyzer,
		cgocall.Analyzer,
		composite.Analyzer,
		copylock.Analyzer,
		errorsas.Analyzer,
		httpresponse.Analyzer,
		loopclosure.Analyzer,
		lostcancel.Analyzer,
		nilfunc.Analyzer,
		printf.Analyzer,
		shift.Analyzer,
		stdmethods.Analyzer,
		structtag.Analyzer,
		tests.Analyzer,
		unmarshal.Analyzer,
		unreachable.Analyzer,
		unsafeptr.Analyzer,
		unusedresult.Analyzer,
	)
}
