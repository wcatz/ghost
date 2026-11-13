package main

import (
	"context"
	"io"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

func runImportCore(ctx context.Context, store *memory.Store, path string, opts portable.ImportOptions, out io.Writer) error {
	a, err := openImportArtifact(path)
	if err != nil {
		return err
	}
	return runImportReader(ctx, store, path, a, opts, out)
}

func discardStreams() upgradeStreams { return upgradeStreams{out: io.Discard, err: io.Discard} }
