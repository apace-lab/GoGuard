// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"

	"github.com/bozhen-liu/gopa/internal/lsp/protocol"
	"github.com/bozhen-liu/gopa/internal/lsp/source"
)

func (s *Server) references(ctx context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	snapshot, fh, ok, release, err := s.beginFileRequest(ctx, params.TextDocument.URI, source.Go)
	defer release()
	if !ok {
		return nil, err
	}
	references, err := source.References(ctx, snapshot, fh, params.Position, params.Context.IncludeDeclaration)
	if err != nil {
		return nil, err
	}
	var locations []protocol.Location
	for _, ref := range references {
		refRange, err := ref.Range()
		if err != nil {
			return nil, err
		}
		locations = append(locations, protocol.Location{
			URI:   protocol.URIFromSpanURI(ref.URI()),
			Range: refRange,
		})
	}
	return locations, nil
}
