// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"

	"github.com/bozhen-liu/gopa/internal/lsp/protocol"
	"github.com/bozhen-liu/gopa/internal/lsp/source"
)

func (s *Server) rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	snapshot, fh, ok, release, err := s.beginFileRequest(ctx, params.TextDocument.URI, source.Go)
	defer release()
	if !ok {
		return nil, err
	}
	edits, err := source.Rename(ctx, snapshot, fh, params.Position, params.NewName)
	if err != nil {
		return nil, err
	}

	var docChanges []protocol.TextDocumentEdit
	for uri, e := range edits {
		fh, err := snapshot.GetFile(ctx, uri)
		if err != nil {
			return nil, err
		}
		docChanges = append(docChanges, documentChanges(fh, e)...)
	}
	return &protocol.WorkspaceEdit{
		DocumentChanges: docChanges,
	}, nil
}

func (s *Server) prepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.Range, error) {
	snapshot, fh, ok, release, err := s.beginFileRequest(ctx, params.TextDocument.URI, source.Go)
	defer release()
	if !ok {
		return nil, err
	}
	// Do not return errors here, as it adds clutter.
	// Returning a nil result means there is not a valid rename.
	item, err := source.PrepareRename(ctx, snapshot, fh, params.Position)
	if err != nil {
		return nil, nil // ignore errors
	}
	// TODO(suzmue): return ident.Name as the placeholder text.
	return &item.Range, nil
}
