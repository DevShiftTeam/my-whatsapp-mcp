package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types/events"
)

func TestAppStateFailureDiagnosticsAreTypedAndSticky(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_APPSTATE_VALUE"
	for _, tc := range []struct {
		name              appstate.WAPatchName
		err               error
		collection, class string
		full              bool
	}{
		{appstate.WAPatchCriticalBlock, appstate.ErrMismatchingLTHash, "critical_block", "lthash", true},
		{appstate.WAPatchCriticalUnblockLow, appstate.ErrMismatchingPatchMAC, "critical_unblock_low", "patch_mac", false},
		{appstate.WAPatchRegularLow, appstate.ErrMismatchingContentMAC, "regular_low", "content_mac", true},
		{appstate.WAPatchRegularHigh, appstate.ErrMismatchingIndexMAC, "regular_high", "index_mac", false},
		{appstate.WAPatchRegular, context.Canceled, "regular", "canceled", true},
		{appstate.WAPatchName(private), context.DeadlineExceeded, "unknown", "deadline", false},
		{appstate.WAPatchRegular, &json.SyntaxError{Offset: 7}, "regular", "json_syntax", true},
		{appstate.WAPatchRegular, &json.UnmarshalTypeError{Value: private, Type: reflect.TypeOf(0), Field: private}, "regular", "json_type", false},
		{appstate.WAPatchName(private), opaqueConnectionError{}, "unknown", "unknown", true},
		{appstate.WAPatchName(private), nil, "unknown", "unknown", false},
	} {
		s := fixtureStore()
		r := testRuntime(s)
		t.Cleanup(func() { _ = r.Close() })
		if err := r.Connect(context.Background()); err != nil {
			t.Fatal("synthetic connection setup failed")
		}
		baseline := r.Status().Codes
		syncKind := "app_state_incremental_sync"
		if tc.full {
			syncKind = "app_state_full_sync"
		}
		want := append(baseline, "app_state_collection_"+tc.collection, "app_state_error_"+tc.class, syncKind, "app_state_sync_error")
		// Joined wrappers preserve typed errors without ever calling Error().
		r.deliver(&events.AppStateSyncError{Name: tc.name, Error: errors.Join(tc.err, opaqueConnectionError{}), FullSync: tc.full})
		first := r.Status()
		if first.ReconciliationRequired || first.Connection == "reconciliation_required" || !slices.Equal(first.Codes, want) {
			t.Errorf("%s/%s: expected fixed typed gap witness and continued admission, got %v", tc.collection, tc.class, first.Codes)
		}
		if r.runCtx.Err() != nil {
			t.Error("app-state gap canceled readable-message admission")
		}
		// Connection progress and later failures must not overwrite the first witness.
		r.connectionDiagnostic("event", "connected", nil, 0, false)
		r.deliver(&events.Connected{})
		r.deliver(&events.AppStateSyncError{Name: appstate.WAPatchRegularHigh, Error: appstate.ErrMismatchingPatchMAC, FullSync: !tc.full})
		if !slices.Equal(r.Status().Codes, first.Codes) {
			t.Error("later progress replaced first app-state witness")
		}
		encoded, err := json.Marshal(r.Status())
		if err != nil || strings.Contains(string(encoded), private) {
			t.Error("status retained private app-state values")
		}
		if err := r.Close(); err != nil && !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Error("synthetic owner cleanup failed")
		}
		s.mu.Lock()
		if len(s.unsafe) != 0 || len(s.events) != 0 {
			t.Error("coverage diagnostics became a global archive denial")
		}
		gaps := 0
		for _, update := range s.syncs {
			if update.GapCode == "app_state_sync_error" {
				gaps++
				if !update.CoverageUnknown {
					t.Error("app-state gap claimed complete coverage")
				}
			} else if update.GapCode != "" {
				t.Error("diagnostic subcode became a gap command")
			}
		}
		if gaps == 0 {
			t.Error("typed witness was not persisted as the fixed app-state gap")
		}
		s.mu.Unlock()
	}
}
