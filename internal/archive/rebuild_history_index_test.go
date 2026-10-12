//go:build sqlite_fts5

package archive

import (
	"context"
	"strings"
	"testing"
)

func TestDeliveredHistoryBodyReceiptLookupUsesBoundedIndex(t *testing.T) {
	s, _, e := deliveredWriterFixture(t)
	ctx := context.Background()
	rows, err := s.readDB.QueryContext(ctx, "EXPLAIN QUERY PLAN "+deliveredBodyExistsQuery, e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, e.Kind, e.SourceRevision)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	used := false
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		if strings.Contains(detail, "rebuild_history_body_lookup") && strings.Contains(detail, "account=? AND message=? AND from_me=? AND kind=? AND protocol_revision=?") {
			used = true
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatalf("body receipt lookup lacks bounded account/message/direction/kind/revision index; plan=%q", plan)
	}
}

func TestDeliveredHistoryBodyReceiptLookupPreservesFullKeyAndAliasIdentity(t *testing.T) {
	s, a, e := deliveredWriterFixture(t)
	ctx := context.Background()
	e.Key.Message = "same-delivered-message-id"
	e.DeliveredHistory = &a
	events := []SourceEvent{e, e, e}
	events[1].Key.Chat = "other-delivered-chat"
	events[2].Key.Sender = "other-delivered-sender"
	for _, candidate := range events {
		result, err := s.ApplySourceEvent(ctx, candidate, a.PolicyGeneration)
		if err != nil || result.Outcome != OutcomeApplied {
			t.Fatal("distinct full key not admitted", result, err)
		}
		found, err := deliveredBodyExists(ctx, s.readDB, candidate)
		if err != nil || !found {
			t.Fatal("exact full receipt missing", found, err)
		}
	}
	unknown := e
	unknown.Key.Sender = "unseen-delivered-sender"
	if found, err := deliveredBodyExists(ctx, s.readDB, unknown); err != nil || found {
		t.Fatal("same message id gained another sender's receipt", found, err)
	}
	unknown = e
	unknown.Key.FromMe = !unknown.Key.FromMe
	if found, err := deliveredBodyExists(ctx, s.readDB, unknown); err != nil || found {
		t.Fatal("direction not bound", found, err)
	}
	unknown = e
	unknown.SourceRevision++
	if found, err := deliveredBodyExists(ctx, s.readDB, unknown); err != nil || found {
		t.Fatal("revision not bound", found, err)
	}
	aliasEvent := e
	aliasEvent.Key.Chat = "900100@lid"
	aliasEvent.Key.Sender = "900100@lid"
	aliasEvent.Key.Message = "alias-delivered-id"
	if result, err := s.ApplySourceEvent(ctx, aliasEvent, a.PolicyGeneration); err != nil || result.Outcome != OutcomeApplied {
		t.Fatal("alias original", result, err)
	}
	if err := s.ApplyAlias(ctx, SourceAlias{Account: e.Key.Account, PhoneJID: "15550000100@s.whatsapp.net", LID: "900100@lid", Provenance: AliasConnectorMapping}); err != nil {
		t.Fatal(err)
	}
	aliasEvent.Key.Chat = "15550000100@s.whatsapp.net"
	aliasEvent.Key.Sender = "15550000100@s.whatsapp.net"
	if found, err := deliveredBodyExists(ctx, s.readDB, aliasEvent); err != nil || !found {
		t.Fatal("canonical alias receipt lost", found, err)
	}
	if _, err := s.ApplySourceEvent(ctx, aliasEvent, a.PolicyGeneration); err != nil {
		t.Fatal(err)
	}
	var spent, receipts int
	if err := s.readDB.QueryRow("SELECT body_spent,(SELECT count(*) FROM rebuild_history_bodies) FROM rebuild_history_selection WHERE id=1").Scan(&spent, &receipts); err != nil || spent != 4 || receipts != 4 {
		t.Fatal("full-key/alias credit identity changed", spent, receipts, err)
	}
}
