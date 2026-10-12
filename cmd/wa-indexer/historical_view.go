package main

import (
	"path/filepath"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
)

func historicalViewOptions(opts options, explicit bool) (*app.HistoricalViewConfig, error) {
	if !explicit {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, opts.historicalViewAt)
	if err != nil || at.IsZero() || at.After(time.Now()) || !filepath.IsAbs(opts.historicalViewDir) || filepath.Clean(opts.historicalViewDir) != opts.historicalViewDir || !filepath.IsAbs(opts.dataDir) || filepath.Clean(opts.dataDir) != opts.dataDir || opts.dataDir == opts.historicalViewDir || opts.demo || opts.openExisting || opts.historicalSnapshotAt != "" || len(opts.historicalViewID) != 16 {
		return nil, errArguments
	}
	for _, c := range opts.historicalViewID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, errArguments
		}
	}
	return &app.HistoricalViewConfig{DataDir: opts.historicalViewDir, SnapshotID: opts.historicalViewID, CapturedAt: at}, nil
}
