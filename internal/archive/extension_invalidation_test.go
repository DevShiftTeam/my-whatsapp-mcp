//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceTransactionPublishesOnlyPermittedMediaAssociations(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("media-association", "original image caption", 1, EventOriginal)
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("allowed synthetic protocol key"), Availability: "not_retained"}}
	r := apply(t, s, e, p.Generation)
	if len(r.MediaIDs) != 1 {
		t.Fatalf("event did not return transaction-owned media association: %d", len(r.MediaIDs))
	}
	duplicate := apply(t, s, e, p.Generation)
	if len(duplicate.MediaIDs) != 1 || duplicate.MediaIDs[0] != r.MediaIDs[0] {
		t.Fatal("duplicate source did not retain exact association identity")
	}
	e.Key.Message = "view-once"
	e.Media[0].ViewOnce = true
	e.Media[0].ProtocolReference = []byte("forbidden view once key")
	r = apply(t, s, e, p.Generation)
	if len(r.MediaIDs) != 0 {
		t.Fatal("view-once source retained media authority")
	}
	var keys int
	if err := s.readDB.QueryRow("SELECT COUNT(*) FROM extension_media_associations WHERE message_id=? AND length(protocol_reference)>0", r.Version.MessageID).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("view-once keys persisted: %d %v", keys, err)
	}
}
func TestSourceChangeAtomicallyPurgesEveryDependentExtension(t *testing.T) {
	for _, kind := range []string{"edit", "invalidation", "delete", "stale_security", "policy", "clear", "alias", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			s, clock, _ := indexTestStore(t)
			p := testPolicy(t, s)
			ctx := context.Background()
			e := event("source-invalidation", "synthetic dependent source", 1, EventOriginal)
			if kind == "alias" {
				e.Key.Chat = "123@lid"
				e.Key.Sender = "123@lid"
			}
			if kind == "expiry" {
				e.ExpiresAt = time.UnixMilli(clock.Load()).Add(time.Minute)
			}
			applied := apply(t, s, e, p.Generation)
			g, err := s.EnsureIndex(ctx, indexTestVersion("invalidate-v1"))
			if err != nil {
				t.Fatal(err)
			}
			j := indexTestPublishedJob(t, s, g)
			if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
				t.Fatalf("fixture vector: %v %v", ok, err)
			}
			_ = applied
			switch kind {
			case "edit":
				m := e
				m.Kind = EventEditContent
				m.SourceRevision = 2
				m.Body = "new content"
				apply(t, s, m, p.Generation)
			case "invalidation":
				m := e
				m.Kind = EventEditInvalidation
				m.SourceRevision = 2
				m.Body = ""
				apply(t, s, m, p.Generation)
			case "delete":
				m := e
				m.Kind = EventDelete
				m.SourceRevision = 2
				m.Body = ""
				apply(t, s, m, p.Generation)
			case "stale_security":
				m := e
				m.SourceRevision = 0
				m.ExpiresAt = time.UnixMilli(clock.Load()).Add(time.Minute)
				apply(t, s, m, p.Generation)
			case "policy":
				if _, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation); err != nil {
					t.Fatal(err)
				}
			case "clear":
				if err = s.ClearChat(ctx, e.Key.Account, e.Key.Chat, e.OriginalSentAt); err != nil {
					t.Fatal(err)
				}
			case "alias":
				if err = s.ApplyAlias(ctx, SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "123@lid", Provenance: AliasConnectorMapping}); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				clock.Add(int64((2 * time.Minute) / time.Millisecond))
				if err = s.SweepExpired(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var documents, vectors, fts, jobs int
			if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_documents").Scan(&documents); err != nil {
				t.Fatal(err)
			}
			if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_vectors").Scan(&vectors); err != nil {
				t.Fatal(err)
			}
			if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_document_fts").Scan(&fts); err != nil {
				t.Fatal(err)
			}
			if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_jobs WHERE state IN ('pending','leased')").Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if documents != 0 || vectors != 0 || fts != 0 || jobs != 0 {
				t.Fatalf("dependent publication survived source %s: docs=%d vectors=%d fts=%d jobs=%d", kind, documents, vectors, fts, jobs)
			}
		})
	}
}

func TestSourceClearAndSweepCannotConsumeNewBackfillDirtyWork(t *testing.T) {
	for _, kind := range []string{"clear", "sweep"} {
		t.Run(kind, func(t *testing.T) {
			s, clock, _ := indexTestStore(t)
			policy := testPolicy(t, s)
			ctx := context.Background()
			start := time.UnixMilli(clock.Load()).Add(-30 * time.Minute)
			for i, body := range []string{"alpha", "bravo", "charlie"} {
				e := event(body, body, 1, EventOriginal)
				e.OriginalSentAt = start.Add(time.Duration(i) * 4 * time.Minute)
				if i == 0 && kind == "sweep" {
					e.ExpiresAt = time.UnixMilli(clock.Load()).Add(time.Minute)
				}
				apply(t, s, e, policy.Generation)
			}
			generation, err := s.EnsureIndex(ctx, indexTestVersion("backfill-v1"))
			if err != nil {
				t.Fatal(err)
			}
			publication := indexTestPublication(t, s, generation, 256)
			merged := Document{ID: "old-boundary", Kind: DocumentWindow, Text: "alpha\nbravo\ncharlie", PassageTokens: 4, Provenance: DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}}
			for _, document := range publication.Documents {
				merged.Sources = append(merged.Sources, document.Sources...)
				merged.Spans = append(merged.Spans, document.Spans...)
			}
			publication.Documents = []Document{merged}
			if ok, e := s.PublishWindows(ctx, publication); e != nil || !ok {
				t.Fatalf("initial boundary publication: %v %v", ok, e)
			}
			if kind == "clear" {
				err = s.ClearChat(ctx, "account", "chat", start)
			} else {
				clock.Add(int64(2 * time.Minute / time.Millisecond))
				err = s.SweepExpired(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			old := indexTestPublication(t, s, generation, 1)
			var committedRevision ArchiveRevision
			if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&committedRevision); err != nil {
				t.Fatal(err)
			}
			if old.Work.DirtyRevision != committedRevision {
				t.Fatalf("%s dirty token is ahead of its source transaction: %d != %d", kind, old.Work.DirtyRevision, committedRevision)
			}
			backfill := event("backfill", "backfill body", 1, EventOriginal)
			backfill.OriginalSentAt = start.Add(2 * time.Minute)
			apply(t, s, backfill, policy.Generation)
			if _, err = s.PublishWindows(ctx, old); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_dirty WHERE generation=?", generation.ID).Scan(&remaining); err != nil || remaining == 0 {
				t.Fatalf("old work acknowledged new backfill: %d %v", remaining, err)
			}
			clock.Add(int64(2 * time.Minute / time.Millisecond))
			work, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: generation.ID, TTL: time.Minute, Limit: 256})
			if err != nil {
				t.Fatal(err)
			}
			input, err := s.LoadWindowMessages(ctx, work)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range input {
				if message.Body == backfill.Body {
					return
				}
			}
			t.Fatal("new source behind the old claimed cursor was skipped")
		})
	}
}

func TestSourceInvalidationPurgesOriginalAndDerivedFTSPhysicalCanary(t *testing.T) {
	s, _, _ := indexTestStore(t)
	policy := testPolicy(t, s)
	e := event("physical-purge", "archivephysicalpurgecanaryuniqueterm", 1, EventOriginal)
	apply(t, s, e, policy.Generation)
	generation, err := s.EnsureIndex(context.Background(), indexTestVersion("physical-purge-v1"))
	if err != nil {
		t.Fatal(err)
	}
	indexTestPublishedJob(t, s, generation)
	deleted := e
	deleted.Kind, deleted.SourceRevision, deleted.Body = EventDelete, 2, ""
	apply(t, s, deleted, policy.Generation)
	if _, err = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(s.privateDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(e.Body)) {
		t.Fatal("private canary survives source/FTS deletion in SQLite file")
	}
}
