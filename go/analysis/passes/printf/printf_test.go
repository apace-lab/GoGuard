package printf_test

import (
	"testing"

	"github.com/bozhen-liu/gopa/go/analysis/analysistest"
	"github.com/bozhen-liu/gopa/go/analysis/passes/printf"
)

func Test(t *testing.T) {
	testdata := analysistest.TestData()
	printf.Analyzer.Flags.Set("funcs", "Warn,Warnf")
	analysistest.Run(t, testdata, printf.Analyzer, "a", "b", "nofmt")
}
