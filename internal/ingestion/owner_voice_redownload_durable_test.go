//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestOwnerVoiceDurableDeliveredReplyPreservesParentAndBodyReceipts(t *testing.T) {
	ctx := context.Background()
	source, sourceDir := durableFixture(t)
	if err := source.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "archive")
	receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
	if err != nil {
		t.Fatal(err)
	}
	store := durableStore(t, destination)
	selection, err := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{Policy: archive.RebuildHistoryDelivered, CommandID: "synthetic-owner-voice-delivered", AcceptDeliveredSnapshot: true})
	if err != nil || selection == nil {
		t.Fatal(err)
	}
	r := durableRuntime(t, store)
	r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, DeviceID: receipt.DeviceID, Boundary: receipt.Boundary, Requests: 32}
	r.config.RebuildHistoryPolicy = selection.Policy
	if err = configureRebuild(ctx, &r.config, store); err != nil {
		t.Fatal(err)
	}
	record := historyRecord("owner-voice", "unused")
	record.Message.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/synthetic/owner-voice"), MediaKey: bytes.Repeat([]byte{0x11}, 32), FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64), Mimetype: proto.String("audio/ogg")}}
	if err = r.historyFrom(ctx, deliveredNotification(r, record)); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: durableAccount, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: initial.Source, MediaID: initial.Lease.MediaID, Budget: archive.BudgetOriginals, MaximumBytes: 128, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.PublishOriginal(ctx, archive.OriginalPublication{Reservation: old, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatal("fixture publication", err)
	}
	p, err := store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err = store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := r.client.ParseWebMessage(types.EmptyJID, record.Message)
	if err != nil {
		t.Fatal(err)
	}
	key, err := r.envelopeKey(ctx, parsed.Info)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.CurrentSource(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.SourceMetadata(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := store.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := store.RebuildDispatchState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	received, applied := r.Status().Received, r.Status().Applied
	r.client.Store.LIDs = &voiceRedeliveryLIDStore{mappings: make(map[types.JID]types.JID)}
	r.status.Connection = "connected"
	r.runCtx = ctx
	r.sendRebuildRequest = func(sendCtx context.Context, id string, _ *waE2E.Message) error {
		_, known, err := store.LookupOwnerVoiceRequest(sendCtx, id)
		if err != nil || !known {
			t.Fatal("request not durable before send", err)
		}
		response, err := r.client.ParseWebMessage(types.EmptyJID, proto.Clone(record.Message).(*waWeb.WebMessageInfo))
		if err != nil {
			return err
		}
		response.UnavailableRequestID = id
		if err = r.message(sendCtx, response); err != nil {
			return err
		}
		return r.message(sendCtx, response)
	}
	scope := archive.ReadScope{Account: durableAccount, Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	if count, err := r.RequestOwnerVoiceRedownloads(ctx, scope); err != nil || count != 1 {
		t.Fatalf("owner request: count=%d err=%v", count, err)
	}
	after, err := store.CurrentSource(ctx, key)
	if err != nil || before != after {
		t.Fatal("descriptor reply changed source", err)
	}
	afterMeta, err := store.SourceMetadata(ctx, key)
	if err != nil || !reflect.DeepEqual(meta, afterMeta) {
		t.Fatal("descriptor reply changed parent metadata", err)
	}
	afterProgress, err := store.RebuildProgress(ctx)
	if err != nil || !reflect.DeepEqual(progress, afterProgress) {
		t.Fatal("descriptor reply changed rebuild body receipts", err)
	}
	afterDispatch, err := store.RebuildDispatchState(ctx)
	if err != nil || !reflect.DeepEqual(dispatch, afterDispatch) {
		t.Fatal("descriptor reply changed rebuild dispatch accounting", err)
	}
	if r.Status().Received != received || r.Status().Applied != applied {
		t.Fatal("descriptor reply incremented positive counters")
	}
	status, err := store.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || status.WaitingCleanup != 1 {
		t.Fatalf("descriptor acceptance not durable before cleanup: %+v %v", status, err)
	}
}

func TestOwnerVoiceNormalArchivePreservesAuthenticatedNegativeControls(t *testing.T) {
	for _, kind := range []string{"expiry", "view_once"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			web := historyRecord("negative-owner-voice", "").Message
			web.MessageTimestamp = proto.Uint64(uint64(time.Now().Add(-time.Minute).Unix()))
			web.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/synthetic/normal-voice"), MediaKey: bytes.Repeat([]byte{0x11}, 32), FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64), Mimetype: proto.String("audio/ogg")}}
			event, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.message(ctx, event); err != nil {
				t.Fatal(err)
			}
			work, err := store.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: durableAccount, TTL: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			old, err := store.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: work.Source, MediaID: work.Lease.MediaID, Budget: archive.BudgetOriginals, MaximumBytes: 128, TTL: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := store.PublishOriginal(ctx, archive.OriginalPublication{Reservation: old, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
				t.Fatal(err)
			}
			p, err := store.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err = store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, p.Generation)
			if err != nil {
				t.Fatal(err)
			}
			scope := archive.ReadScope{Account: durableAccount, Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
			intents, err := store.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
			if err != nil || len(intents) != 1 {
				t.Fatal("prepare negative fixture", err)
			}
			intent := intents[0]
			if err = store.RegisterOwnerVoiceRequest(ctx, archive.OwnerVoiceRequestRegistration{IntentID: intent.ID, RequestID: "negative-request", Source: intent.Source}); err != nil {
				t.Fatal(err)
			}
			if kind == "view_once" {
				if ok, err := store.AcceptOwnerVoiceDescriptor(ctx, archive.OwnerVoiceDescriptorReply{RequestID: "negative-request", Key: intent.Key, OriginalSentAt: intent.OriginalSentAt, SourceRevision: intent.SourceRevision, Media: func() archive.MediaReference {
					e := archive.SourceEvent{}
					p, _ := r.projectBody(ctx, &e, event.Message)
					defer wipePending(p)
					return e.Media[0]
				}()}); err != nil || !ok {
					t.Fatal("consume fixture request", err)
				}
				web.Message.AudioMessage.ViewOnce = proto.Bool(true)
			} else {
				if _, err = store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, p.Generation); err != nil {
					t.Fatal("advance policy before negative reply", err)
				}
				web.EphemeralExpirationTimestamp = proto.Uint64(uint64(time.Now().Add(-time.Second).Unix()))
			}
			response, err := r.client.ParseWebMessage(types.EmptyJID, web)
			if err != nil {
				t.Fatal(err)
			}
			response.UnavailableRequestID = "negative-request"
			received := r.Status().Received
			if err = r.message(ctx, response); err != nil {
				t.Fatalf("normal negative control rejected: %v", err)
			}
			if r.Status().ReconciliationRequired || r.Status().Received != received {
				t.Fatal("valid negative control poisoned archive or admitted body")
			}
			current, err := store.CurrentSource(ctx, intent.Key)
			if err == nil && !current.Version.Excluded && !current.Version.Deleted && (current.Version.ExpiresAt.IsZero() || current.Version.ExpiresAt.After(time.Now())) {
				t.Fatal("negative control left original readable")
			}
			if _, err = store.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: durableAccount, TTL: 2 * time.Minute}); !errors.Is(err, archive.ErrNoWork) {
				t.Fatal("negative control left recovery download eligible", err)
			}
		})
	}
}
