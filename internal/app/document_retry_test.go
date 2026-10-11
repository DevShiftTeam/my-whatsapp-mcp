//go:build sqlite_fts5

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

type ownerDocumentRedeliveryConnector struct {
	*downloadWakeConnector
	scope     archive.ReadScope
	ids       []archive.MessageID
	requested int
	entered   chan struct{}
	release   chan struct{}
}

func (r *ownerDocumentRedeliveryConnector) Status() ingestion.Status {
	return ingestion.Status{Connection: "connected", HasSession: true}
}

func (r *ownerDocumentRedeliveryConnector) RequestDocumentRedelivery(ctx context.Context, scope archive.ReadScope, ids []archive.MessageID) (int, error) {
	r.scope = scope
	r.ids = append([]archive.MessageID(nil), ids...)
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return r.requested, nil
}

func ownerDocumentRetryApplication(t *testing.T) (*Application, archive.MessageID, archive.Generation) {
	t.Helper()
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.runtime = nil
		if err := a.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	policy, err := a.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "synthetic-chat", Sender: "synthetic-sender", Message: "unsupported-document"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageUnsupported}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return a, result.Version.MessageID, policy.Generation
}

func TestDocumentRetryApplicationDerivesOwnerScopeAndExactMessages(t *testing.T) {
	a, id, generation := ownerDocumentRetryApplication(t)
	runtime := &ownerDocumentRedeliveryConnector{downloadWakeConnector: &downloadWakeConnector{}, requested: 1}
	a.runtime = runtime
	result, err := a.RetryDocumentDownloads(context.Background(), []archive.MessageID{id})
	if err != nil || result.Queued != 0 || result.Skipped != 1 || result.Requested != 1 || runtime.wakes != 0 {
		t.Fatal("owner document request confused phone acknowledgements with retained download work")
	}
	if runtime.scope.Account != "account" || runtime.scope.Principal != "owner" || runtime.scope.PolicyGeneration != generation || !runtime.scope.AllChats || len(runtime.ids) != 1 || runtime.ids[0] != id {
		t.Fatal("document recovery changed the exact messages or server-owned current owner scope")
	}
	for _, invalid := range [][]archive.MessageID{nil, {id, id}, {"https://private.invalid/file"}} {
		if _, err := a.RetryDocumentDownloads(context.Background(), invalid); !errors.Is(err, localui.ErrDocumentRetryUnavailable) {
			t.Fatal("application accepted a malformed document retry batch")
		}
	}
	a.closed = true
	if _, err := a.RetryDocumentDownloads(context.Background(), []archive.MessageID{id}); !errors.Is(err, localui.ErrDocumentRetryUnavailable) {
		t.Fatal("closed application accepted document recovery")
	}
	a.closed = false
}

func TestDocumentRetryPhoneWaitDoesNotBlockStatus(t *testing.T) {
	a, id, _ := ownerDocumentRetryApplication(t)
	runtime := &ownerDocumentRedeliveryConnector{downloadWakeConnector: &downloadWakeConnector{}, requested: 1, entered: make(chan struct{}), release: make(chan struct{})}
	a.runtime = runtime
	done := make(chan error, 1)
	go func() { _, err := a.RetryDocumentDownloads(context.Background(), []archive.MessageID{id}); done <- err }()
	select {
	case <-runtime.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not enter its bounded phone operation")
	}
	read := make(chan error, 1)
	go func() { _, err := a.Status(context.Background()); read <- err }()
	select {
	case err := <-read:
		close(runtime.release)
		if retryErr := <-done; retryErr != nil {
			t.Fatal(retryErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(runtime.release)
		<-done
		<-read
		t.Fatal("phone I/O blocked an ordinary archive status read")
	}
}
