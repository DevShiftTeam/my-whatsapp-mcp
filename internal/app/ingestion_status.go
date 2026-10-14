package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var errIngestionStatusUnavailable = errors.New("ingestion status unavailable")

var _ localui.IngestionApplication = (*Application)(nil)

type ingestionLatencyConnector interface {
	IngestionLatency() ingestion.LatencySnapshot
}

func sameIngestionConnector(current, captured connector) bool {
	if current == nil || captured == nil {
		return current == nil && captured == nil
	}
	left, right := reflect.ValueOf(current), reflect.ValueOf(captured)
	// Real connectors are owned pointers. Fail closed for a typed-nil pointer
	// or an implementation whose interface identity cannot be compared safely.
	return !(left.Kind() == reflect.Pointer && left.IsNil()) && !(right.Kind() == reflect.Pointer && right.IsNil()) && left.Comparable() && right.Comparable() && current == captured
}

// IngestionStatus reads only the current connector's in-memory diagnostics. It
// never queries the archive, SDK session, network, or a replacement connector.
func (a *Application) IngestionStatus(ctx context.Context) (ingestion.LatencySnapshot, error) {
	if ctx == nil {
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ingestion.LatencySnapshot{}, err
	}
	if !a.data.TryRLock() {
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	if a.closed || a.offline || a.demo || a.snapshot != nil {
		a.data.RUnlock()
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	runtime, queryContext := a.runtime, a.queryContext
	if !sameTranscriptionQueryContext(queryContext, queryContext) {
		a.data.RUnlock()
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	if queryContext != nil {
		if err := queryContext.Err(); err != nil {
			a.data.RUnlock()
			return ingestion.LatencySnapshot{}, err
		}
	}
	if runtime == nil {
		a.data.RUnlock()
		return ingestion.LatencySnapshot{}, nil
	}
	source, ok := runtime.(ingestionLatencyConnector)
	if !ok || !sameIngestionConnector(runtime, runtime) {
		a.data.RUnlock()
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	a.data.RUnlock()
	// Even this cheap optional method runs outside lifecycle locks. No app lock
	// is held while entering another component, and no new work is scheduled.
	value := source.IngestionLatency()
	if !value.Valid() {
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	if !a.data.TryRLock() {
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	defer a.data.RUnlock()
	if a.closed || a.offline || a.demo || a.snapshot != nil || !sameIngestionConnector(a.runtime, runtime) || !sameTranscriptionQueryContext(a.queryContext, queryContext) {
		return ingestion.LatencySnapshot{}, errIngestionStatusUnavailable
	}
	// Check cancellation directly, rather than relying on an asynchronous
	// callback to propagate a retired query lifetime before response delivery.
	if queryContext != nil {
		if err := queryContext.Err(); err != nil {
			return ingestion.LatencySnapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ingestion.LatencySnapshot{}, err
	}
	return value, nil
}
