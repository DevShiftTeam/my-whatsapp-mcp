package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

// This explicit command is the only headless stdout path for a browser login
// capability. A private stale file is only a candidate, never login authority.
func ownerLink(ctx context.Context, opts options, stdout io.Writer) error {
	candidate, err := app.OwnerAttachURL(app.Config{DataDir: opts.dataDir, Demo: opts.demo})
	if err != nil {
		return localui.ErrOwnerAttach
	}
	bootstrap, err := localui.AttachWithConnect(ctx, candidate, opts.demo, opts.connect)
	if err != nil {
		return localui.ErrOwnerAttach
	}
	if _, err := fmt.Fprintln(stdout, bootstrap); err != nil {
		return localui.ErrOwnerAttach
	}
	return nil
}
