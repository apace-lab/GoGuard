// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mod

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/bozhen-liu/gopa/internal/lsp/cache"
	"github.com/bozhen-liu/gopa/internal/lsp/tests"
	"github.com/bozhen-liu/gopa/internal/span"
	"github.com/bozhen-liu/gopa/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.ExitIfSmallMachine()
	os.Exit(m.Run())
}

func TestModfileRemainsUnchanged(t *testing.T) {
	testenv.NeedsGo1Point(t, 14)

	ctx := tests.Context(t)
	cache := cache.New(ctx, nil)
	session := cache.NewSession(ctx)
	options := tests.DefaultOptions()
	options.TempModfile = true
	options.Env = append(os.Environ(), "GOPACKAGESDRIVER=off", "GOROOT=")

	// Make sure to copy the test directory to a temporary directory so we do not
	// modify the test code or add go.sum files when we run the tests.
	folder, err := tests.CopyFolderToTempDir(filepath.Join("testdata", "unchanged"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(folder)

	before, err := ioutil.ReadFile(filepath.Join(folder, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, release, err := session.NewView(ctx, "diagnostics_test", span.URIFromPath(folder), options)
	release()
	if err != nil {
		t.Fatal(err)
	}
	after, err := ioutil.ReadFile(filepath.Join(folder, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the real go.mod file was changed even when tempModfile=true")
	}
}
