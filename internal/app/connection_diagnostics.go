package app

import (
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
)

// safeConnectionDiagnostic admits only closed operational metadata. A faulty
// connector cannot turn an owner status response into a raw error channel.
func safeConnectionDiagnostic(value ingestion.ConnectionDiagnostic) archive.ConnectionDiagnostic {
	var safe archive.ConnectionDiagnostic
	switch value.Phase {
	case "start", "runtime_closed", "session_close_failed", "reconciliation_required", "optional_join", "prior_stop", "policy", "session_missing", "session_deleted", "observation", "qr_channel", "connect", "post_pair_stop", "post_pair_start", "awaiting_authentication", "connected", "event", "recovery_start", "recovery_policy", "recovery_refused":
		safe.Phase = value.Phase
	}
	switch value.Event {
	case "none", "connected", "disconnected", "logged_out", "manual_login_reconnect", "connect_failure", "client_outdated", "temporary_ban", "stream_error", "stream_replaced", "cat_refresh_error":
		safe.Event = value.Event
	}
	switch value.ErrorClass {
	case "none", "canceled", "deadline", "dial", "http_status", "network_timeout", "network_unavailable", "already_connected", "source_unavailable", "device_deleted", "unknown":
		safe.ErrorClass = value.ErrorClass
	}
	if safe.ErrorClass == "http_status" && value.Reason >= 100 && value.Reason <= 599 {
		safe.Reason = value.Reason
	} else if safe.Event == "connect_failure" || safe.Event == "logged_out" {
		switch value.Reason {
		case 400, 401, 402, 403, 405, 406, 409, 413, 414, 415, 418, 500, 501, 503:
			safe.Reason = value.Reason
		}
	}
	safe.OnConnect = safe.Event == "logged_out" && value.OnConnect
	if value.SocketStateKnown {
		safe.SocketStateKnown = true
		safe.SocketConnected = value.SocketConnected
		safe.SocketAuthenticated = value.SocketAuthenticated
	}
	return safe
}

func safeDiagnosticCodes(archiveCodes, runtimeCodes []string, reconciliation bool) []string {
	codes := []string{}
	if reconciliation {
		codes = append(codes, "reconciliation_required")
	}
	for _, source := range [][]string{archiveCodes, runtimeCodes} {
		// Bound work even if a faulty connector returns an oversized slice.
		if len(source) > 32 {
			source = source[:32]
		}
		for _, code := range source {
			if !archive.IsDiagnosticFailureCode(code) {
				continue
			}
			duplicate := false
			for _, existing := range codes {
				duplicate = duplicate || existing == code
			}
			if !duplicate {
				codes = append(codes, code)
			}
		}
	}
	return codes
}
