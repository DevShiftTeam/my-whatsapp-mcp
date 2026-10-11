package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type DoctorReport struct {
	Archive          string           `json:"archive"`
	Connection       string           `json:"connection"`
	KeywordAvailable bool             `json:"keyword_available"`
	Semantic         archive.Progress `json:"semantic"`
	Media            archive.Progress `json:"media"`
	NativeClients    string           `json:"native_clients"`
	FreeBytes        int64            `json:"free_bytes"`
	ReservedBytes    int64            `json:"reserved_bytes"`
	NativeAcceptance string           `json:"native_acceptance"`
}

func (a *Application) Doctor(ctx context.Context) (DoctorReport, error) {
	status, err := a.Status(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.resources == nil {
		return DoctorReport{}, archive.ErrArchiveUnavailable
	}
	disk, err := a.resources.disk.Snapshot()
	if err != nil {
		return DoctorReport{}, err
	}
	_, scopeErr := a.scope(ctx)
	if scopeErr != nil && !errors.Is(scopeErr, archive.ErrUnauthorized) {
		return DoctorReport{}, scopeErr
	}
	keywordAvailable := !status.ReconciliationRequired && scopeErr == nil && status.Keyword.State == "ready"
	report := DoctorReport{Archive: "owned", Connection: string(status.Connection), KeywordAvailable: keywordAvailable, Semantic: status.Semantic, Media: status.Media, FreeBytes: disk.FreeBytes, ReservedBytes: disk.ReservedBytes, NativeAcceptance: "NOT_RUN"}
	report.NativeClients = "ready"
	if a.nativeRecoveryPending || a.authState.NativeInvalid {
		report.NativeClients = "recovery_pending"
	}
	return report, nil
}
