//go:build sqlite_fts5

package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
)

func TestConnectionDiagnosticsAreSanitizedAndReachOwnerState(t *testing.T) {
	ctx := context.Background()
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, store archive.ConnectorStore, callbacks ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: store, callbacks: callbacks}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: privateTestRoot(t)}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	runtime.snapshot.Connection = "disconnected"
	runtime.snapshot.HasSession = true
	runtime.snapshot.Codes = []string{"archive_unavailable", "PRIVATE_SOURCE_TEXT", "archive_unavailable"}
	cases := []struct {
		name  string
		input ingestion.ConnectionDiagnostic
		want  archive.ConnectionDiagnostic
	}{
		{
			name:  "early startup failure",
			input: ingestion.ConnectionDiagnostic{Phase: "session_missing", Event: "none", ErrorClass: "source_unavailable", SocketStateKnown: true},
			want:  archive.ConnectionDiagnostic{Phase: "session_missing", Event: "none", ErrorClass: "source_unavailable", SocketStateKnown: true},
		},
		{
			name:  "bounded public HTTP reason",
			input: ingestion.ConnectionDiagnostic{Phase: "connect", Event: "connect_failure", ErrorClass: "http_status", Reason: 401, SocketStateKnown: true, SocketConnected: true},
			want:  archive.ConnectionDiagnostic{Phase: "connect", Event: "connect_failure", ErrorClass: "http_status", Reason: 401, SocketStateKnown: true, SocketConnected: true},
		},
		{
			name:  "unknown values cannot escape",
			input: ingestion.ConnectionDiagnostic{Phase: "PRIVATE_PHASE", Event: "PRIVATE_EVENT", ErrorClass: "PRIVATE_ERROR", Reason: 67234589, SocketConnected: true, SocketAuthenticated: true, OnConnect: true},
			want:  archive.ConnectionDiagnostic{},
		},
		{
			name:  "logged out reason and phase remain bounded",
			input: ingestion.ConnectionDiagnostic{Phase: "session_deleted", Event: "logged_out", ErrorClass: "device_deleted", Reason: 401, OnConnect: true},
			want:  archive.ConnectionDiagnostic{Phase: "session_deleted", Event: "logged_out", ErrorClass: "device_deleted", Reason: 401, OnConnect: true},
		},
		{
			name:  "reason requires a recognized protocol context",
			input: ingestion.ConnectionDiagnostic{Phase: "policy", Event: "none", ErrorClass: "unknown", Reason: 401},
			want:  archive.ConnectionDiagnostic{Phase: "policy", Event: "none", ErrorClass: "unknown"},
		},
		{
			name:  "recovery network failure remains finite",
			input: ingestion.ConnectionDiagnostic{Phase: "recovery_start", Event: "none", ErrorClass: "network_unavailable", Reason: 503, OnConnect: true},
			want:  archive.ConnectionDiagnostic{Phase: "recovery_start", Event: "none", ErrorClass: "network_unavailable"},
		},
		{
			name:  "recovery policy guard reaches owner",
			input: ingestion.ConnectionDiagnostic{Phase: "recovery_policy", Event: "none", ErrorClass: "source_unavailable"},
			want:  archive.ConnectionDiagnostic{Phase: "recovery_policy", Event: "none", ErrorClass: "source_unavailable"},
		},
		{
			name:  "recovery refusal reaches owner",
			input: ingestion.ConnectionDiagnostic{Phase: "recovery_refused", Event: "none", ErrorClass: "source_unavailable"},
			want:  archive.ConnectionDiagnostic{Phase: "recovery_refused", Event: "none", ErrorClass: "source_unavailable"},
		},
		{
			name:  "CAT failure cannot export a raw error or reason",
			input: ingestion.ConnectionDiagnostic{Phase: "event", Event: "cat_refresh_error", ErrorClass: "unknown", Reason: 503, OnConnect: true},
			want:  archive.ConnectionDiagnostic{Phase: "event", Event: "cat_refresh_error", ErrorClass: "unknown"},
		},
		{
			name:  "unemitted recovery stop is not admitted",
			input: ingestion.ConnectionDiagnostic{Phase: "recovery_stop", Event: "PRIVATE_EVENT", ErrorClass: "PRIVATE_ERROR", Reason: 401, OnConnect: true},
			want:  archive.ConnectionDiagnostic{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Do not invoke StatusChanged: real startup failure diagnostics must be
			// read from the current connector even before a connection callback.
			runtime.snapshot.ConnectionDiagnostic = tc.input
			status, err := a.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(status.ConnectionDiagnostic, tc.want) {
				t.Fatal("owner status dropped or exposed an unvalidated connection diagnostic")
			}
			if !reflect.DeepEqual(status.Codes, []string{"archive_unavailable"}) || status.ReconciliationRequired {
				t.Fatal("healthy runtime failure codes were dropped or misclassified")
			}
			pairing, err := a.PairingState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !pairing.HasSession || pairing.State != "disconnected" || pairing.QRAvailable || !reflect.DeepEqual(pairing.ConnectionDiagnostic, tc.want) {
				t.Fatal("owner linking state dropped saved-session or safe diagnostic metadata")
			}
		})
	}
	runtime.snapshot.HasSession = false
	pairing, err := a.PairingState(ctx)
	if err != nil || pairing.HasSession {
		t.Fatal("owner linking state retained a deleted or absent saved session")
	}
	// Sampling the current connector must not erase a previously received safety
	// denial while its callbacks and status snapshot are being reconciled.
	a.statusChanged(ingestion.Status{Connection: "reconciliation_required", ReconciliationRequired: true, Codes: []string{"queue_limit_exceeded"}})
	status, err := a.Status(ctx)
	if err != nil || !status.ReconciliationRequired || status.Connection != "reconciliation_required" {
		t.Fatal("current connector status erased a cached safety denial")
	}
}
