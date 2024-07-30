// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fillreturns_test

import (
	"testing"

	"github.com/bozhen-liu/gopa/go/analysis/analysistest"
	"github.com/bozhen-liu/gopa/internal/lsp/analysis/fillreturns"
)

func Test(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.RunWithSuggestedFixes(t, testdata, fillreturns.Analyzer, "a")
}
