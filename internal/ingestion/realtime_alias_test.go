//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func realtimeAliasText(id, body, shape string) *events.Message {
	event := liveMessage(id, body)
	lid, pn := types.NewJID("900", types.HiddenUserServer), types.NewJID("200", types.DefaultUserServer)
	switch shape {
	case "sender":
		event.Info.Sender, event.Info.SenderAlt = lid, pn
	case "recipient":
		event.Info.Chat, event.Info.RecipientAlt = lid, pn
	case "both":
		event.Info.Chat, event.Info.Sender = lid, lid
		event.Info.SenderAlt, event.Info.RecipientAlt = pn, pn
	case "pn_sender":
		event.Info.SenderAlt = lid
	}
	return event
}

func realtimeAliasMCP(t *testing.T, store *archive.Store, r *Runtime) (http.Handler, string, archive.ChatID) {
	t.Helper()
	if err := r.message(t.Context(), liveMessage("alias-prefix", "aliasprefixreceipt")); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, store, "aliasprefixreceipt")
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("synthetic alias grant seed was not readable")
	}
	chat := page.Hits[0].ChatID
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32))
	if _, err := store.CreateCredential(t.Context(), archive.CredentialCreate{Label: "Synthetic committed alias", Principal: "client:alias", Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{chat}}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	handler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: time.Second}, store, store)
	if err != nil {
		t.Fatal(err)
	}
	return handler, token, chat
}

func realtimeAliasVisible(t *testing.T, handler http.Handler, token, query string, chat archive.ChatID) bool {
	t.Helper()
	var page struct {
		Hits []struct {
			ChatID archive.ChatID `json:"chat_id"`
		} `json:"hits"`
	}
	deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": query, "limit": 1}), &page)
	if len(page.Hits) == 0 {
		return false
	}
	if len(page.Hits) != 1 || page.Hits[0].ChatID != chat {
		t.Fatal("committed alias changed the scoped chat authority")
	}
	return true
}

func TestRealtimeCommittedAlternateIDsScopedMCPWithinOneSecond(t *testing.T) {
	for _, shape := range []string{"sender", "recipient", "both", "pn_sender"} {
		t.Run(shape, func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			handler, token, chat := realtimeAliasMCP(t, store, r)
			if err := store.ApplyAlias(t.Context(), archive.SourceAlias{Account: durableAccount, PhoneJID: "200@s.whatsapp.net", LID: "900@lid", Provenance: archive.AliasConnectorMapping}); err != nil {
				t.Fatal(err)
			}
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "aliashistoryreceipt")))
			awaitHistoryDrain(t, barrier.entered, "held history before real writer admission")
			started := time.Now()
			r.deliver(realtimeAliasText("alias-independent", "aliasfreshreceipt", shape))
			visible := false
			for time.Since(started) < time.Second {
				if realtimeAliasVisible(t, handler, token, "aliasfreshreceipt", chat) {
					visible = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			t.Logf("committed_alias_shape=%s fresh_visible=%t elapsed_ms=%d", shape, visible, time.Since(started).Milliseconds())
			if !visible {
				t.Error("ordinary plaintext with a committed PN/LID pair waited behind historical positives")
			}
			if r.Status().HistoryChunks != 0 {
				t.Error("history drained before the committed-alias visibility assertion")
			}
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !realtimeAliasVisible(t, handler, token, "aliasfreshreceipt", chat) {
				t.Fatal("alternate-ID source was lost after ordered drain")
			}
		})
	}
}

func TestRealtimeUncommittedAndConflictingAlternateIDsRemainOrdered(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_pair", true: "conflicting_pair"}[conflict], func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			handler, token, chat := realtimeAliasMCP(t, store, r)
			if conflict {
				if err := store.ApplyAlias(t.Context(), archive.SourceAlias{Account: durableAccount, PhoneJID: "300@s.whatsapp.net", LID: "900@lid", Provenance: archive.AliasConnectorMapping}); err != nil {
					t.Fatal(err)
				}
			}
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "aliashistoryreceipt")))
			awaitHistoryDrain(t, barrier.entered, "held history")
			r.deliver(realtimeAliasText("alias-source-ordered", "aliasorderedreceipt", "both"))
			r.deliver(liveMessage("after-alias-source", "aliasfollowingreceipt"))
			realtimeAssertBlocked(t, store, "aliasorderedreceipt")
			realtimeAssertBlocked(t, store, "aliasfollowingreceipt")
			canonical, err := store.Canonical(t.Context(), durableAccount, "900@lid")
			want := "900@lid"
			if conflict {
				want = "300@s.whatsapp.net"
			}
			if err != nil || canonical != want || r.Status().ReconciliationRequired || r.Status().Dropped != 0 {
				t.Fatal("optional classification installed or rejected an uncommitted/conflicting alias out of order")
			}
			release()
			if conflict {
				if err := r.Disconnect(t.Context()); !errors.Is(err, archive.ErrReconciliationRequired) {
					t.Fatal("ordered conflicting alias lost its strict source fence")
				}
				result := deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "aliasorderedreceipt"})
				if result.status == http.StatusOK {
					t.Fatal("source conflict left the scoped archive readable")
				}
				return
			}
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !realtimeAliasVisible(t, handler, token, "aliasorderedreceipt", chat) || !realtimeAliasVisible(t, handler, token, "aliasfollowingreceipt", chat) {
				t.Fatal("fallback dropped a valid alias-bearing source or its suffix")
			}
		})
	}
}

// Only delay synthetic canonical lookups. No real SQLite lock is held.
type realtimeAliasProbeBarrier struct {
	*realtimeApplyBarrier
	probeEntered, probeRelease chan struct{}
	probeOnce                  sync.Once
}

func (s *realtimeAliasProbeBarrier) Canonical(ctx context.Context, account, identity string) (string, error) {
	if identity == "900@lid" {
		s.probeOnce.Do(func() { close(s.probeEntered) })
		select {
		case <-s.probeRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.Store.Canonical(ctx, account, identity)
}

func TestRealtimeAliasProbeTicketGatesLaterCallbacksAndEarlierUnpreparedHistory(t *testing.T) {
	store, _ := durableFixture(t)
	barrier := &realtimeAliasProbeBarrier{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}, probeEntered: make(chan struct{}), probeRelease: make(chan struct{})}
	r := durableRuntime(t, barrier)
	var historyOnce, probeOnce sync.Once
	releaseHistory := func() { historyOnce.Do(func() { close(barrier.release) }) }
	releaseProbe := func() { probeOnce.Do(func() { close(barrier.probeRelease) }) }
	t.Cleanup(releaseHistory)
	t.Cleanup(releaseProbe)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	handler, token, chat := realtimeAliasMCP(t, store, r)
	if err := store.ApplyAlias(t.Context(), archive.SourceAlias{Account: durableAccount, PhoneJID: "200@s.whatsapp.net", LID: "900@lid", Provenance: archive.AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
	awaitHistoryDrain(t, barrier.entered, "first prepared chunk")
	r.deliver(realtimeHistory(r, historyRecord("before-alias-probe", "beforealiasprobereceipt")))
	aliasDone := queueBurstDelivery(r, realtimeAliasText("probed-alias", "probedaliasreceipt", "both"))
	awaitHistoryDrain(t, barrier.probeEntered, "read-only committed-alias lookup")
	laterDone := queueBurstDelivery(r, liveMessage("after-alias-probe", "afteraliasprobereceipt"))
	select {
	case <-laterDone:
		t.Fatal("later callback overtook an unresolved classification ticket")
	case <-time.After(20 * time.Millisecond):
	}
	if snapshot := r.IngestionLatency(); !snapshot.Valid() || snapshot.Unsafe {
		t.Fatal("classification probe held Runtime.mu or escaped the admission ledgers")
	}
	r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "alias-prefix", Timestamp: time.Unix(2000, 0)})
	if r.Status().Controls != 1 || r.Status().ReconciliationRequired {
		t.Fatal("classification lookup delayed a received control")
	}
	releaseProbe()
	queueBurstAwaitDelivery(t, aliasDone)
	queueBurstAwaitDelivery(t, laterDone)
	realtimeAssertBlocked(t, store, "probedaliasreceipt")
	realtimeAssertBlocked(t, store, "afteraliasprobereceipt")
	releaseHistory()
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !realtimeAliasVisible(t, handler, token, "probedaliasreceipt", chat) || !realtimeAliasVisible(t, handler, token, "afteraliasprobereceipt", chat) {
		t.Fatal("owned classification or an earlier source prefix lost valid work")
	}
}

type realtimeAliasRecheckMiss struct {
	*realtimeApplyBarrier
	mu        sync.Mutex
	reads     int
	rechecked chan struct{}
}

func (s *realtimeAliasRecheckMiss) Canonical(ctx context.Context, account, identity string) (string, error) {
	if identity == "900@lid" {
		s.mu.Lock()
		s.reads++
		miss := s.reads == 2
		s.mu.Unlock()
		if miss {
			close(s.rechecked)
			return "", context.DeadlineExceeded
		}
	}
	return s.Store.Canonical(ctx, account, identity)
}

func TestRealtimeCommittedAliasRecheckMissPreservesCompleteSourceOrder(t *testing.T) {
	store, _ := durableFixture(t)
	barrier := &realtimeAliasRecheckMiss{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}, rechecked: make(chan struct{})}
	r := durableRuntime(t, barrier)
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	t.Cleanup(release)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	handler, token, chat := realtimeAliasMCP(t, store, r)
	if err := store.ApplyAlias(t.Context(), archive.SourceAlias{Account: durableAccount, PhoneJID: "200@s.whatsapp.net", LID: "900@lid", Provenance: archive.AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
	awaitHistoryDrain(t, barrier.entered, "held prepared history")
	r.deliver(realtimeAliasText("rechecked-alias", "recheckedaliasreceipt", "sender"))
	awaitHistoryDrain(t, barrier.rechecked, "pre-publication alias recheck")
	r.deliver(liveMessage("after-rechecked-alias", "afterrecheckedreceipt"))
	realtimeAssertBlocked(t, store, "recheckedaliasreceipt")
	realtimeAssertBlocked(t, store, "afterrecheckedreceipt")
	if r.Status().Dropped != 0 || r.Status().ReconciliationRequired {
		t.Fatal("an optional scheduling lookup miss became a source failure")
	}
	release()
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !realtimeAliasVisible(t, handler, token, "recheckedaliasreceipt", chat) || !realtimeAliasVisible(t, handler, token, "afterrecheckedreceipt", chat) {
		t.Fatal("pre-publication fallback discarded valid source work")
	}
}
