//go:build sqlite_fts5

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestOwnerVoiceRecoveryPhysicalPublicationAndCurrentExtraction(t *testing.T) {
	ctx := context.Background()
	s, paths, _ := testStore(t, newRepository(), Options{})
	repo, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	s.repo = repo
	policy, err := repo.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := waveFixture()
	hash := sha256.Sum256(data)
	descriptor := &waE2E.AudioMessage{DirectPath: proto.String("/synthetic/old-voice"), MediaKey: bytes.Repeat([]byte{0x11}, 32), FileSHA256: hash[:], FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(uint64(len(data)))}
	encoded, err := proto.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	source := archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "voice-original"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Hour).Truncate(time.Second), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageVoice, Media: []archive.MediaReference{{Kind: "voice", MimeType: "audio/wav", DeclaredBytes: int64(len(data)), ProtocolReference: encoded, Availability: "not_retained"}}}
	applied, err := repo.ApplySourceEvent(ctx, source, policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatal(err)
	}
	q := DownloadRequest{Key: source.Key, Version: applied.Version, MediaID: applied.MediaIDs[0], Kind: archive.MessageVoice, DeclaredBytes: int64(len(data)), ProtocolSHA256: hash}
	publish := func(q DownloadRequest) {
		t.Helper()
		d, err := s.AdmitDownload(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.File().Write(data); err != nil {
			t.Fatal(err)
		}
		if err = d.Complete(ctx, ingestion.VerifiedMedia{Key: q.Key, Version: q.Version, Kind: q.Kind, SHA256: hash, Bytes: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
			t.Fatal(err)
		}
		if err = d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	publish(q)
	scope := archive.ReadScope{Account: "account", Principal: "owner", PolicyGeneration: policy.Generation, AllChats: true}
	old, err := repo.AuthorizeOriginal(ctx, scope, q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(paths.MediaDir, artifactName(old.ID, ".original"))
	if _, err = os.Stat(oldPath); err != nil {
		t.Fatal("initial physical original absent", err)
	}
	policy, err = repo.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope.PolicyGeneration = policy.Generation
	intents, err := repo.PrepareOwnerVoiceRedownloads(ctx, scope, 8)
	if err != nil || len(intents) != 1 {
		t.Fatal("prepare", err)
	}
	intent := intents[0]
	if err = repo.RegisterOwnerVoiceRequest(ctx, archive.OwnerVoiceRequestRegistration{IntentID: intent.ID, RequestID: "fresh-physical-request", Source: intent.Source}); err != nil {
		t.Fatal(err)
	}
	// The phone can rotate transport credentials; the prior verified plaintext
	// SHA remains the integrity pin and the parent identity is unchanged.
	descriptor.DirectPath = proto.String("/synthetic/fresh-voice")
	fresh, err := proto.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	reference := source.Media[0]
	reference.ProtocolReference = fresh
	if ok, err := repo.AcceptOwnerVoiceDescriptor(ctx, archive.OwnerVoiceDescriptorReply{RequestID: "fresh-physical-request", Key: source.Key, OriginalSentAt: source.OriginalSentAt, SourceRevision: source.SourceRevision, Media: reference}); err != nil || !ok {
		t.Fatal("fresh descriptor", err)
	}
	if _, err = repo.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: "account", TTL: 2 * time.Minute}); !errors.Is(err, archive.ErrNoWork) {
		t.Fatal("claimed before physical cleanup", err)
	}
	if _, err = s.Reconcile(ctx, 16); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old original survived cleanup", err)
	}
	work, err := repo.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: "account", TTL: 2 * time.Minute})
	if err != nil || work.Lease.IntentID != intent.ID {
		t.Fatal("owner claim", err)
	}
	input, err := repo.LoadOriginalDownload(ctx, work.Lease)
	if err != nil || !bytes.Equal(input.ProtocolReference, fresh) {
		t.Fatal("fresh descriptor not loaded", err)
	}
	q.Version = work.Source.Version
	q.OwnerDownloadLease = work.Lease
	publish(q)
	if ok, err := repo.FinishOriginalDownload(ctx, work.Lease); err != nil || !ok {
		t.Fatal("owner completion", err)
	}
	current, err := repo.AuthorizeOriginal(ctx, scope, q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID == old.ID || current.ReservationID == old.ReservationID || current.Source != intent.Source || current.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("publication reused old artifact or changed source/hash")
	}
	if _, err = os.Stat(filepath.Join(paths.MediaDir, artifactName(current.ID, ".original"))); err != nil {
		t.Fatal("new physical original absent", err)
	}
	version := archive.ExtractionVersion{ID: "synthetic-new-voice", Kind: archive.JobTranscription, ProfileID: "synthetic-voice", ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: strings.Repeat("b", 64), ExtractorVersion: "fixture-v1", PromptVersion: "none-v1", Language: "auto"}
	extraction, err := repo.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal("current extraction unavailable", err)
	}
	if extraction.ArtifactID != current.ID || extraction.Source != intent.Source || extraction.OriginalSHA256 != current.SHA256 {
		t.Fatal("extraction restored an obsolete source/artifact tuple")
	}
	original, err := s.OpenOriginal(ctx, scope, q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic provider reads the recovered bytes through the normal guarded
	// original handle. No model, network or real recording participates.
	fakeProvider := func(input io.Reader) (string, error) {
		received, err := io.ReadAll(input)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(received, data) {
			t.Fatal("provider received wrong original")
		}
		return "syntheticrecoveredvoice nonempty transcript", nil
	}
	transcript, err := fakeProvider(io.NewSectionReader(original.Reader(), 0, int64(len(data))))
	if err != nil {
		t.Fatal(err)
	}
	if err = original.Recheck(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	doc := archive.Document{ID: "synthetic-recovered-transcript", Kind: archive.DocumentTranscript, Text: transcript, Sources: []archive.SourceRef{extraction.Source}, Provenance: archive.DerivedProvenance{Kind: archive.DocumentTranscript, MediaID: extraction.MediaID, ProfileID: version.ProfileID, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}}
	if ok, err := repo.CommitExtraction(ctx, archive.ExtractionResult{Lease: extraction.Lease, ContentHash: extraction.ContentHash, Outcome: archive.ExtractionText, Documents: []archive.Document{doc}}); err != nil || !ok {
		t.Fatal("transcript publication", err)
	}
	pair, err := repo.CandidatePair(ctx, scope, archive.CandidatePairRequest{Search: archive.SearchRequest{Query: "syntheticrecoveredvoice", Limit: 10}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 {
		t.Fatal("published transcript not searchable", err)
	}
	page, err := repo.HydrateDocuments(ctx, scope, archive.HydrateRequest{Documents: []archive.DocumentRef{pair.Keyword[0].Ref}, Snapshot: pair.Snapshot})
	if err != nil || len(page.Documents) != 1 || len(page.Documents[0].Excerpts) != 1 || page.Documents[0].Excerpts[0].Text != transcript {
		t.Fatal("published transcript not hydrated", err)
	}
	if _, err = repo.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: page.Guard}); err != nil {
		t.Fatal("transcript response guard", err)
	}
	if _, err = repo.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: version, TTL: time.Minute}); !errors.Is(err, archive.ErrNoWork) {
		t.Fatal("completed transcript was requeued", err)
	}
	after, err := repo.CurrentSource(ctx, source.Key)
	if err != nil || after != intent.Source {
		t.Fatal("transcription changed parent source", err)
	}
	status, err := repo.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || status.Completed != 1 {
		t.Fatal("new original completion absent", err)
	}
}
