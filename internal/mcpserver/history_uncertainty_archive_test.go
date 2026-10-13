//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestHistoryUncertaintyMCPKeepsScopedReadsAndGenericWarning(t *testing.T) {
	f := openActualFixture(t)
	ctx := context.Background()
	observation, err := f.store.BeginObservation(ctx, f.account)
	if err != nil {
		t.Fatal(err)
	}
	defer f.store.EndObservation(ctx, observation)
	if err := f.store.RecordHistoryUncertainty(ctx, f.account, archive.HistoryUncertaintyUnsupportedControl, observation); err != nil {
		t.Fatal(err)
	}
	server, _ := httpFixture(t, f.store, f.store, nil)
	code, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "archive-match"}), nil)
	if code != http.StatusOK {
		t.Fatalf("stored scoped archive unavailable: %d", code)
	}
	assertPartialGapLabel(t, partialGapPayload(t, body))
	for _, forbidden := range []string{"forbidden-body-canary", "old-client-filter-canary", "total_observations", "unsupported_control", "raw_stub", "coverage_epoch"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("scope/private uncertainty field escaped: %s", forbidden)
		}
	}
	raw, err := f.store.UnknownHistoryGapDiagnostics(ctx, f.account)
	if err != nil || raw.TotalObservations != 0 {
		t.Fatalf("generic gap fabricated legacy raw observations: %+v %v", raw, err)
	}
}

func TestHistoryUncertaintyMCPFinalDeliveryRejectsNewAndRepeatedGap(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "repeated"}[repeated], func(t *testing.T) {
			f := openActualFixture(t)
			ctx := context.Background()
			observation, err := f.store.BeginObservation(ctx, f.account)
			if err != nil {
				t.Fatal(err)
			}
			defer f.store.EndObservation(ctx, observation)
			if repeated {
				if err = f.store.RecordHistoryUncertainty(ctx, f.account, archive.HistoryUncertaintyUnsupportedNotice, observation); err != nil {
					t.Fatal(err)
				}
			}
			committed := false
			credentials := &interleavedCredentials{Credentials: f.store, mutate: func() error {
				err := f.store.RecordHistoryUncertainty(ctx, f.account, archive.HistoryUncertaintyUnsupportedMetadata, observation)
				committed = err == nil
				return err
			}}
			server, _ := httpFixture(t, f.store, credentials, nil)
			code, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "archive-match"}), nil)
			if !committed || code != http.StatusConflict || strings.Contains(body, "permitted original target") {
				t.Fatalf("stale response escaped committed uncertainty: committed=%v code=%d", committed, code)
			}
		})
	}
}
