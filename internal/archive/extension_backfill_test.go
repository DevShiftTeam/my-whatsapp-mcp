//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func selectedBackfillPort(t *testing.T, s *Store) SelectedBackfillStore {
	t.Helper()
	port, ok := any(s).(SelectedBackfillStore)
	if !ok {
		t.Fatal("Store does not implement the selected-backfill port")
	}
	return port
}

func backfillChat(t *testing.T, s *Store, source string) ChatID {
	t.Helper()
	var id ChatID
	if err := s.readDB.QueryRow("SELECT id FROM chats WHERE account='account' AND source_chat=?", source).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSelectedBackfillStartsAtNewestDiscoveryAnchorBeforeAnyMessageIsAdmitted(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	oldest := readerEvent("discovered-oldest", "selected", "discarded body", now.Add(-time.Hour))
	newest := readerEvent("discovered-newest", "selected", "discarded body", now.Add(-time.Minute))
	readerApply(t, s, p, oldest)
	readerApply(t, s, p, newest)
	chat := backfillChat(t, s, "selected")
	p, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{chat}}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var admitted int
	if err = s.readDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&admitted); err != nil || admitted != 0 {
		t.Fatalf("discovery admitted message bodies: %d %v", admitted, err)
	}
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{chat}})
	if err != nil || len(anchors) != 1 || anchors[0].Key != newest.Key {
		t.Fatalf("first selection skipped recent history by starting before the oldest discarded discovery: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillContinuesBeforeOldestAdmittedMessage(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	readerApply(t, s, p, readerEvent("discarded-older", "selected", "discarded body", now.Add(-4*time.Hour)))
	readerApply(t, s, p, readerEvent("discovered-newest", "selected", "discarded body", now.Add(-time.Minute)))
	chat := backfillChat(t, s, "selected")
	p, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{chat}}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	readerApply(t, s, p, readerEvent("admitted-newer", "selected", "admitted body", now.Add(-time.Hour)))
	oldest := readerEvent("admitted-oldest", "selected", "admitted body", now.Add(-2*time.Hour))
	readerApply(t, s, p, oldest)
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{chat}})
	if err != nil || len(anchors) != 1 || anchors[0].Key != oldest.Key {
		t.Fatalf("discarded discovery displaced the admitted history frontier: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillKeepsAdmittedFrontierAfterDiscoveryAnchorIsPruned(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	oldest := readerEvent("admitted-oldest", "selected", "admitted body", now.Add(-2*time.Hour))
	readerApply(t, s, p, oldest)
	readerApply(t, s, p, readerEvent("admitted-newer", "selected", "admitted body", now.Add(-time.Hour)))
	// saveAnchor retains only the newest discovery keys. Model that pruning
	// directly so this small fixture does not need 1,001 message bodies.
	if _, err := s.writeDB.Exec("DELETE FROM source_anchors WHERE account=? AND chat=? AND message=?", oldest.Key.Account, oldest.Key.Chat, oldest.Key.Message); err != nil {
		t.Fatal(err)
	}
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{backfillChat(t, s, "selected")}})
	if err != nil || len(anchors) != 1 || anchors[0].Key != oldest.Key {
		t.Fatalf("pruning discovery metadata moved the admitted history frontier forward: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillActiveFrontierExcludesExpiredDeletedAndUnknownDateMessages(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	expired := readerEvent("expired", "selected", "expired body", now.Add(-4*time.Hour))
	expired.ExpiresAt = now.Add(time.Minute)
	readerApply(t, s, p, expired)
	deleted := readerEvent("deleted", "selected", "deleted body", now.Add(-3*time.Hour))
	readerApply(t, s, p, deleted)
	deleted.Kind, deleted.SourceRevision, deleted.Body = EventDelete, 2, ""
	readerApply(t, s, p, deleted)
	valid := readerEvent("active", "selected", "admitted body", now.Add(-2*time.Hour))
	readerApply(t, s, p, valid)
	unknown := readerEvent("unknown-date", "selected", "discarded body", time.Time{})
	unknown.OriginalDateKnown = false
	readerApply(t, s, p, unknown)
	now = now.Add(2 * time.Minute)
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{backfillChat(t, s, "selected")}})
	if err != nil || len(anchors) != 1 || anchors[0].Key != valid.Key {
		t.Fatalf("ineligible message metadata displaced the active frontier: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillFiltersSelectionAndPolicyBeforeRankAndLimit(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	quiet := readerEvent("quiet-oldest", "quiet", "private body", now.Add(-4*time.Hour))
	readerApply(t, s, p, quiet)
	for i := 0; i < 40; i++ {
		readerApply(t, s, p, readerEvent(fmt.Sprintf("noise-%02d", i), "noisy", "private body", now.Add(-time.Hour).Add(time.Duration(i)*time.Minute)))
		readerApply(t, s, p, readerEvent(fmt.Sprintf("denied-%02d", i), "denied", "private body", now.Add(-10*time.Hour).Add(time.Duration(i)*time.Minute)))
	}
	quietChat, noisyChat := backfillChat(t, s, "quiet"), backfillChat(t, s, "noisy")
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{quietChat, noisyChat}}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	port := selectedBackfillPort(t, s)
	anchors, err := port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{quietChat}, Limit: 1})
	if err != nil || len(anchors) != 1 || anchors[0].Key != quiet.Key {
		t.Fatalf("requested quiet chat was crowded out: %#v %v", anchors, err)
	}
	anchors, err = port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, AllSelected: true, Limit: 2})
	if err != nil || len(anchors) != 2 || anchors[0].Key.Message != "quiet-oldest" || anchors[1].Key.Message != "noise-00" {
		t.Fatalf("denied/noisy anchors displaced one-per-chat selection: %#v %v", anchors, err)
	}
	anchors, err = port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{backfillChat(t, s, "denied")}})
	if err != nil || len(anchors) != 0 {
		t.Fatalf("current policy admitted a denied explicit chat: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillReturnsDeterministicOldestKnownAnchorPerChat(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	sent := now.Add(-time.Hour)
	for _, e := range []SourceEvent{
		readerEvent("newer", "b", "private body", now.Add(-time.Minute)),
		readerEvent("z", "b", "private body", sent),
		readerEvent("a", "b", "private body", sent),
		readerEvent("first", "a", "private body", sent),
	} {
		readerApply(t, s, p, e)
	}
	fromMe := readerEvent("a", "b", "private body", sent)
	fromMe.Key.FromMe = true
	readerApply(t, s, p, fromMe)
	unknown := readerEvent("unknown-date", "a", "must never define an anchor", time.Time{})
	unknown.OriginalDateKnown = false
	readerApply(t, s, p, unknown)
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{backfillChat(t, s, "b"), backfillChat(t, s, "a"), backfillChat(t, s, "b")}})
	if err != nil || len(anchors) != 2 {
		t.Fatalf("oldest anchor inventory: %#v %v", anchors, err)
	}
	if anchors[0].Key.Chat != "a" || anchors[0].Key.Message != "first" || anchors[1].Key.Chat != "b" || anchors[1].Key.Message != "a" || anchors[1].Key.FromMe || !anchors[0].OriginalSentAt.Equal(sent) || !anchors[1].OriginalSentAt.Equal(sent) {
		t.Fatalf("oldest/tie ordering was not deterministic: %#v", anchors)
	}
}

func TestSelectedBackfillCanonicalAliasesAndCurrentDateBounds(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	phone, lid := "123@s.whatsapp.net", "456@lid"
	readerApply(t, s, p, readerEvent("before-import", phone, "private body", now.Add(-3*time.Hour)))
	readerApply(t, s, p, readerEvent("outside-rolling", lid, "private body", now.Add(-90*time.Minute)))
	valid := readerEvent("valid-oldest", lid, "private body", now.Add(-30*time.Minute))
	valid.Key.Sender = lid
	readerApply(t, s, p, valid)
	readerApply(t, s, p, readerEvent("inside-import", phone, "private body", now.Add(-10*time.Minute)))
	phoneChat, oldLIDChat := backfillChat(t, s, phone), backfillChat(t, s, lid)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateSince, ImportStart: now.Add(-2 * time.Hour), ChatMode: ChatsSelective, IncludedChats: []ChatID{oldLIDChat}, RollingRetention: time.Hour}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: phone, LID: lid, Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	port := selectedBackfillPort(t, s)
	anchors, err := port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{oldLIDChat}})
	if err != nil || len(anchors) != 1 || anchors[0].Key.Chat != phone || anchors[0].Key.Sender != phone || anchors[0].Key.Message != "valid-oldest" || !anchors[0].OriginalSentAt.Equal(valid.OriginalSentAt) {
		t.Fatalf("aliases/date policy were applied after ranking: %#v %v", anchors, err)
	}
	anchors, err = port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{oldLIDChat, phoneChat}})
	if err != nil || len(anchors) != 1 || anchors[0].Key.Message != "valid-oldest" {
		t.Fatalf("alias IDs created two ranked chats: %#v %v", anchors, err)
	}
	// Both -30m and -10m satisfy rolling retention; only ImportStart distinguishes them.
	p, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateSince, ImportStart: now.Add(-20 * time.Minute), ChatMode: ChatsSelective, IncludedChats: []ChatID{phoneChat}, RollingRetention: time.Hour}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	anchors, err = port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{oldLIDChat}})
	if err != nil || len(anchors) != 1 || anchors[0].Key.Message != "inside-import" {
		t.Fatalf("ImportStart did not filter before oldest ranking: %#v %v", anchors, err)
	}
	now = now.Add(time.Hour)
	anchors, err = port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, AllSelected: true})
	if err != nil || len(anchors) != 0 {
		t.Fatalf("rolling policy used an old clock: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillIsMetadataOnlyAndNeverInventsAnchors(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("deleted-anchor", "known", "deleted body", now.Add(-time.Hour))
	id := readerApply(t, s, p, e)
	e.Kind, e.SourceRevision, e.Body = EventDelete, 2, ""
	readerApply(t, s, p, e)
	if err := s.SetSourceNames(context.Background(), SourceNames{Account: "account", Chat: "no-anchor", ChatName: "metadata only"}); err != nil {
		t.Fatal(err)
	}
	anchors, err := selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{backfillChat(t, s, "known"), backfillChat(t, s, "no-anchor"), "unknown-opaque-id"}})
	if err != nil || len(anchors) != 1 || anchors[0].Key != e.Key {
		t.Fatalf("metadata incorrectly required source availability or invented an anchor: %#v %v", anchors, err)
	}
	anchors, err = selectedBackfillPort(t, s).SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{"known"}})
	if err != nil || len(anchors) != 0 {
		t.Fatalf("a protocol chat identity was accepted as an opaque chat ID: %#v %v", anchors, err)
	}
	if _, err = s.Context(context.Background(), readerScope(p, backfillChat(t, s, "known")), ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("anchor accidentally became source-body authority: %v", err)
	}
}

func TestSelectedBackfillRejectsInvalidSelectionAndStaleAuthority(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	readerApply(t, s, p, readerEvent("known", "known", "private body", now))
	chat := backfillChat(t, s, "known")
	port := selectedBackfillPort(t, s)
	base := BackfillSelection{Account: "account", PolicyGeneration: p.Generation, Chats: []ChatID{chat}}
	t.Run("caller slice cannot retarget selection", func(t *testing.T) {
		readerApply(t, s, p, readerEvent("other", "other", "private body", now))
		other := backfillChat(t, s, "other")
		selection := base
		selection.Chats = append([]ChatID(nil), base.Chats...)
		originalClock := s.options.Now
		defer func() { s.options.Now = originalClock }()
		var calls atomic.Int64
		s.options.Now = func() time.Time {
			if calls.Add(1) == 1 {
				selection.Chats[0] = other
			}
			return now
		}
		anchors, err := port.SelectedBackfillAnchors(context.Background(), selection)
		if err != nil || len(anchors) != 1 || anchors[0].Key.Message != "known" {
			t.Fatalf("caller mutation retargeted the validated selection: %#v %v", anchors, err)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(*BackfillSelection)
		want   error
	}{
		{"missing account", func(v *BackfillSelection) { v.Account = "" }, ErrUnauthorized},
		{"invalid account", func(v *BackfillSelection) { v.Account = "account\x00" }, ErrUnauthorized},
		{"unbound account", func(v *BackfillSelection) { v.Account = "another-account" }, ErrUnauthorized},
		{"missing generation", func(v *BackfillSelection) { v.PolicyGeneration = 0 }, ErrUnauthorized},
		{"stale generation", func(v *BackfillSelection) { v.PolicyGeneration++ }, ErrPolicyChanged},
		{"empty finite selection", func(v *BackfillSelection) { v.Chats = nil }, ErrInvalidQuery},
		{"all plus finite selection", func(v *BackfillSelection) { v.AllSelected = true }, ErrInvalidQuery},
		{"too many chats", func(v *BackfillSelection) {
			v.Chats = make([]ChatID, 33)
			for i := range v.Chats {
				v.Chats[i] = chat
			}
		}, ErrInvalidQuery},
		{"empty chat", func(v *BackfillSelection) { v.Chats = []ChatID{""} }, ErrInvalidQuery},
		{"overlong chat ID", func(v *BackfillSelection) { v.Chats = []ChatID{ChatID(strings.Repeat("x", 129))} }, ErrInvalidQuery},
		{"NUL chat", func(v *BackfillSelection) { v.Chats = []ChatID{"bad\x00id"} }, ErrInvalidQuery},
		{"invalid UTF8 chat", func(v *BackfillSelection) { v.Chats = []ChatID{ChatID(string([]byte{0xff}))} }, ErrInvalidQuery},
		{"negative limit", func(v *BackfillSelection) { v.Limit = -1 }, ErrInvalidQuery},
		{"oversized limit", func(v *BackfillSelection) { v.Limit = 33 }, ErrInvalidQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.change(&v)
			anchors, err := port.SelectedBackfillAnchors(context.Background(), v)
			if !errors.Is(err, tc.want) || len(anchors) != 0 {
				t.Fatalf("invalid selection returned metadata: %#v %v", anchors, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if anchors, err := port.SelectedBackfillAnchors(ctx, base); !errors.Is(err, context.Canceled) || len(anchors) != 0 {
		t.Fatalf("cancelled read returned metadata: %#v %v", anchors, err)
	}
	if err := s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if anchors, err := port.SelectedBackfillAnchors(context.Background(), base); !errors.Is(err, ErrReconciliationRequired) || len(anchors) != 0 {
		t.Fatalf("unsafe account returned metadata: %#v %v", anchors, err)
	}
}

func TestSelectedBackfillDefaultsAndCapsOneAnchorPerChatAt32(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var chats []ChatID
	for i := 0; i < 33; i++ {
		name := fmt.Sprintf("chat-%02d", i)
		readerApply(t, s, p, readerEvent(name, name, "private body", now.Add(time.Duration(i-33)*time.Minute)))
		chats = append(chats, backfillChat(t, s, name))
	}
	port := selectedBackfillPort(t, s)
	for _, selection := range []BackfillSelection{
		{Account: "account", PolicyGeneration: p.Generation, AllSelected: true},
		{Account: "account", PolicyGeneration: p.Generation, AllSelected: true, Limit: 32},
		{Account: "account", PolicyGeneration: p.Generation, Chats: chats[:32], Limit: 32},
	} {
		anchors, err := port.SelectedBackfillAnchors(context.Background(), selection)
		if err != nil || len(anchors) != 32 || anchors[0].Key.Message != "chat-00" || anchors[31].Key.Message != "chat-31" {
			t.Fatalf("default/max limit lost oldest distinct chats: %#v %v", anchors, err)
		}
	}
}

func TestSelectedBackfillRechecksCurrentAuthorityBeforeReturningMetadata(t *testing.T) {
	for _, change := range []string{"policy", "alias", "unsafe", "rolling"} {
		t.Run(change, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			readerApply(t, s, p, readerEvent("known", "456@lid", "private body", now.Add(-30*time.Minute)))
			if change == "rolling" {
				var err error
				p, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Hour}, p.Generation)
				if err != nil {
					t.Fatal(err)
				}
			}
			port := selectedBackfillPort(t, s)
			var calls atomic.Int64
			var changeErr error
			s.options.Now = func() time.Time {
				if calls.Add(1) == 2 {
					switch change {
					case "policy":
						_, changeErr = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
					case "alias":
						changeErr = s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping})
					case "unsafe":
						changeErr = s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing"))
					case "rolling":
						return now.Add(2 * time.Hour)
					}
				}
				return now
			}
			anchors, err := port.SelectedBackfillAnchors(context.Background(), BackfillSelection{Account: "account", PolicyGeneration: p.Generation, AllSelected: true})
			want := ErrPolicyChanged
			if change == "unsafe" {
				want = ErrReconciliationRequired
			}
			if changeErr != nil || calls.Load() < 2 || !errors.Is(err, want) || len(anchors) != 0 {
				t.Fatalf("current %s change escaped final metadata check: %#v %v, change %v, clock calls %d", change, anchors, err, changeErr, calls.Load())
			}
		})
	}
}
