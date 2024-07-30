// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The ifaceassert command runs the ifaceassert analyzer.
package main

import (
	"github.com/bozhen-liu/gopa/go/analysis/passes/ifaceassert"
	"github.com/bozhen-liu/gopa/go/analysis/singlechecker"
)

func main() { singlechecker.Main(ifaceassert.Analyzer) }
