package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

type ingestionStatusConnector struct {
	*syntheticConnector
	value ingestion.LatencySnapshot
	hook  func()
	calls int
}

func (c *ingestionStatusConnector) IngestionLatency() ingestion.LatencySnapshot {
	c.calls++
	if c.hook != nil {
		c.hook()
	}
	return c.value
}

func ingestionStatusFixture() (*Application, *ingestionStatusConnector) {
	c := &ingestionStatusConnector{syntheticConnector: &syntheticConnector{}, value: ingestion.LatencySnapshot{Running: true}}
	return &Application{runtime: c, queryContext: context.Background()}, c
}

func TestIngestionStatusCurrentConnectorSnapshotOnly(t *testing.T) {
	a, c := ingestionStatusFixture()
	c.hook = func() {
		if !a.data.TryLock() {
			t.Error("snapshot call ran under the application lifecycle lock")
			return
		}
		a.data.Unlock()
	}
	value, err := a.IngestionStatus(context.Background())
	if err != nil || !value.Running || c.calls != 1 {
		t.Fatal("current connector snapshot not returned")
	}
	c.value = ingestion.LatencySnapshot{Unsafe: true}
	value, err = a.IngestionStatus(context.Background())
	if err != nil || value.Running || !value.Unsafe || c.calls != 2 {
		t.Fatal("stopped unsafe connector was relabelled healthy")
	}
	c.value.QueuedCount = -1
	value, err = a.IngestionStatus(context.Background())
	if err == nil || value.Running {
		t.Fatal("invalid source metrics were admitted")
	}
}

func TestIngestionStatusUnavailableAndUnwiredModes(t *testing.T) {
	for _, mode := range []string{"closed", "offline", "snapshot", "demo", "unsupported", "unwired", "typed_nil"} {
		t.Run(mode, func(t *testing.T) {
			a, c := ingestionStatusFixture()
			switch mode {
			case "closed":
				a.closed = true
			case "offline":
				a.offline = true
			case "snapshot":
				a.snapshot = &SnapshotConfig{}
			case "demo":
				a.demo = true
			case "unsupported":
				a.runtime = &syntheticConnector{}
			case "unwired":
				a.runtime = nil
			case "typed_nil":
				a.runtime = (*ingestionStatusConnector)(nil)
			}
			value, err := a.IngestionStatus(context.Background())
			if value.Running || len(value.Phases) != 0 || c.calls != 0 || (err == nil) != (mode == "unwired") {
				t.Fatal("inactive mode fabricated a running snapshot")
			}
		})
	}
}

func TestIngestionStatusCancellationAndTryLocks(t *testing.T) {
	a, c := ingestionStatusFixture()
	if _, err := a.IngestionStatus(nil); err == nil {
		t.Fatal("nil request context admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.IngestionStatus(ctx); !errors.Is(err, context.Canceled) || c.calls != 0 {
		t.Fatal("cancelled request reached metrics source")
	}
	a.data.Lock()
	started := time.Now()
	_, err := a.IngestionStatus(context.Background())
	a.data.Unlock()
	if err == nil || time.Since(started) > time.Second || c.calls != 0 {
		t.Fatal("snapshot waited behind a lifecycle mutation")
	}
	c.hook = a.data.Lock
	_, err = a.IngestionStatus(context.Background())
	a.data.Unlock()
	if err == nil || c.calls != 1 {
		t.Fatal("final lifecycle admission blocked or admitted contention")
	}
}

func TestIngestionStatusLifecycleAndDirectQueryFences(t *testing.T) {
	for _, change := range []string{"closed", "offline", "demo", "snapshot", "replace_connector", "remove_connector", "cancel_query", "replace_query", "nil_query", "noncomparable_query", "cancel_request"} {
		t.Run(change, func(t *testing.T) {
			a, c := ingestionStatusFixture()
			query, cancelQuery := context.WithCancel(context.Background())
			defer cancelQuery()
			a.queryContext = query
			request, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			c.hook = func() {
				a.data.Lock()
				defer a.data.Unlock()
				switch change {
				case "closed":
					a.closed = true
				case "offline":
					a.offline = true
				case "demo":
					a.demo = true
				case "snapshot":
					a.snapshot = &SnapshotConfig{}
				case "replace_connector":
					a.runtime = &ingestionStatusConnector{syntheticConnector: &syntheticConnector{}}
				case "remove_connector":
					a.runtime = nil
				case "cancel_query":
					cancelQuery()
				case "replace_query":
					a.queryContext = context.Background()
				case "nil_query":
					a.queryContext = nil
				case "noncomparable_query":
					a.queryContext = nonComparableTranscriptionQueryContext{Context: context.Background(), marker: []byte("synthetic")}
				case "cancel_request":
					cancelRequest()
				}
			}
			value, err := a.IngestionStatus(request)
			if err == nil || value.Running || c.calls != 1 {
				t.Fatal("retired source or cancelled query published latency")
			}
		})
	}
	for _, kind := range []string{"cancelled", "noncomparable", "nil"} {
		t.Run("initial_query_"+kind, func(t *testing.T) {
			a, c := ingestionStatusFixture()
			query, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "cancelled":
				cancel()
				a.queryContext = query
			case "noncomparable":
				a.queryContext = nonComparableTranscriptionQueryContext{Context: query, marker: []byte("synthetic")}
			case "nil":
				a.queryContext = nil
			}
			value, err := a.IngestionStatus(context.Background())
			if kind == "nil" {
				if err != nil || !value.Running || c.calls != 1 {
					t.Fatal("synthetic nil query lifetime changed the source snapshot")
				}
			} else if err == nil || value.Running || c.calls != 0 {
				t.Fatal("invalid query lifetime reached source snapshot")
			}
		})
	}
}

type nonComparableIngestionConnector struct {
	*syntheticConnector
	marker []byte
}

func (nonComparableIngestionConnector) IngestionLatency() ingestion.LatencySnapshot {
	return ingestion.LatencySnapshot{Running: true}
}

func TestIngestionStatusNonComparableConnectorFailsClosed(t *testing.T) {
	a := &Application{runtime: nonComparableIngestionConnector{syntheticConnector: &syntheticConnector{}, marker: []byte("synthetic")}}
	if value, err := a.IngestionStatus(context.Background()); err == nil || value.Running {
		t.Fatal("noncomparable connector identity was admitted")
	}
}
