package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type attachmentTestArchive struct {
	mu        sync.Mutex
	grant     archive.CredentialGrant
	view      archive.AttachmentView
	page      archive.AttachmentPage
	guard     archive.ResponseGuard
	denied    bool
	checkErr  error
	checkHook func()
	checks    int
	searched  archive.AttachmentSearchRequest
}

func (a *attachmentTestArchive) CheckCredentialGrant(_ context.Context, g archive.CredentialGrant) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.denied || g.CredentialID != a.grant.CredentialID || g.CredentialRevision != a.grant.CredentialRevision || g.Scope.Account != a.grant.Scope.Account || g.Scope.Principal != a.grant.Scope.Principal {
		return archive.ErrUnauthorized
	}
	return nil
}
func (a *attachmentTestArchive) SearchAttachmentsGuarded(_ context.Context, _ archive.ReadScope, q archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error) {
	a.searched = q
	return a.page, a.guard, nil
}
func (a *attachmentTestArchive) AttachmentGuarded(_ context.Context, _ archive.ReadScope, _ archive.MediaID) (archive.AttachmentView, archive.ResponseGuard, error) {
	return a.view, a.guard, nil
}
func (a *attachmentTestArchive) CheckResponse(_ context.Context, _ archive.ReadScope, q archive.ResponseCheck) (archive.ReadSnapshot, error) {
	a.mu.Lock()
	a.checks++
	hook, err := a.checkHook, a.checkErr
	a.mu.Unlock()
	if q.Credential == nil || q.Credential.CredentialID != a.grant.CredentialID {
		return archive.ReadSnapshot{}, archive.ErrUnauthorized
	}
	if len(q.Guard.Sources) != len(a.guard.Sources) {
		return archive.ReadSnapshot{}, archive.ErrSourceUnavailable
	}
	if hook != nil {
		hook()
	}
	return a.view.Snapshot, err
}
func (a *attachmentTestArchive) AttachmentResponseCheck(ctx context.Context, scope archive.ReadScope, q archive.ResponseCheck) (archive.ReadSnapshot, error) {
	return a.CheckResponse(ctx, scope, q)
}

type attachmentTestOriginal struct {
	metadata   media.OriginalMetadata
	payload    []byte
	closed     bool
	recheckErr error
	onRead     func()
	onRecheck  func()
	mutateRead bool
}

func (o *attachmentTestOriginal) Metadata() media.OriginalMetadata { return o.metadata }
func (o *attachmentTestOriginal) Reader() io.ReaderAt              { return o }
func (o *attachmentTestOriginal) ReadAt(p []byte, off int64) (int, error) {
	if o.onRead != nil {
		o.onRead()
	}
	n, err := bytes.NewReader(o.payload).ReadAt(p, off)
	if o.mutateRead && n > 0 {
		p[0] ^= 1
	}
	return n, err
}
func (o *attachmentTestOriginal) Recheck(context.Context, archive.ReadScope) error {
	if o.onRecheck != nil {
		o.onRecheck()
	}
	return o.recheckErr
}
func (o *attachmentTestOriginal) Close() error { o.closed = true; return nil }

type attachmentTestOriginals struct {
	mu           sync.Mutex
	makeOriginal func() *attachmentTestOriginal
	opened       []*attachmentTestOriginal
	err          error
}

func (o *attachmentTestOriginals) Open(_ context.Context, _ archive.ReadScope, _ archive.MediaID) (attachmentOriginal, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, o.err
	}
	result := o.makeOriginal()
	o.opened = append(o.opened, result)
	return result, nil
}

type attachmentTestRegistry struct{ published bool }

func (r *attachmentTestRegistry) PublishedRemoteClient(mcpserver.RemoteClientBinding) bool {
	return r.published
}
func (r *attachmentTestRegistry) PublishedNativeClient(mcpserver.NativeClientBinding) bool {
	return r.published
}

func attachmentServiceFixture(t *testing.T) (*attachmentService, *attachmentTestArchive, *attachmentTestOriginals, *attachmentTestRegistry, string) {
	t.Helper()
	payload := []byte("%PDF-1.7\nsynthetic attachment\n%%EOF\n")
	digest := sha256.Sum256(payload)
	grant := archive.CredentialGrant{CredentialID: "credential-synthetic", CredentialRevision: 1, Scope: archive.ReadScope{Account: "synthetic-account", Principal: archive.RemoteClientPrincipalPrefix + "v1/0123456789abcdef0123456789abcdef", PolicyGeneration: 1, AllChats: true}}
	source := archive.SourceRef{Account: grant.Scope.Account, Version: archive.SourceVersion{MessageID: "synthetic-message", Revision: 2, PolicyGeneration: 1}, ContentHash: "source-hash"}
	metadata := media.OriginalMetadata{MediaID: "synthetic-attachment", ArtifactID: "synthetic-artifact", Source: source, SHA256: hex.EncodeToString(digest[:]), Format: "pdf", Bytes: int64(len(payload))}
	a := &attachmentTestArchive{grant: grant, view: archive.AttachmentView{AttachmentID: metadata.MediaID, MessageID: source.Version.MessageID, Kind: archive.MessageKind("document"), Filename: "test.pdf", MIMEType: "application/pdf", Availability: "retained", Snapshot: archive.ReadSnapshot{PolicyGeneration: 1, RecoveryEpoch: 4}}, guard: archive.ResponseGuard{PolicyGeneration: 1, Sources: []archive.SourceReadVersion{{Ref: source}}}}
	originals := &attachmentTestOriginals{makeOriginal: func() *attachmentTestOriginal {
		return &attachmentTestOriginal{metadata: metadata, payload: append([]byte(nil), payload...)}
	}}
	registry := &attachmentTestRegistry{published: true}
	s, err := newAttachmentService(attachmentServiceConfig{root: context.Background(), archive: a, originals: originals, publicURL: "https://archive.example/mcp", nativeClients: registry, remoteClients: registry, current: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	s.tickets[token] = attachmentTicket{grant: grant, guard: a.guard, metadata: metadata, filename: "test.pdf", snapshot: a.view.Snapshot, expires: time.Now().Add(time.Minute)}
	return s, a, originals, registry, token
}

func attachmentGET(s *attachmentService, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeDownload(w, httptest.NewRequest(http.MethodGet, "https://archive.example/mcp/attachments/"+token, nil))
	return w
}

func TestAttachmentDownloadKeepsOwnershipThroughWriteAndFlush(t *testing.T) {
	s, a, originals, _, token := attachmentServiceFixture(t)
	w := &attachmentOwnershipWriter{t: t, originals: originals, header: make(http.Header)}
	s.ServeDownload(w, httptest.NewRequest(http.MethodGet, "https://archive.example/mcp/attachments/"+token, nil))
	if w.status != http.StatusOK || !w.flushed || a.checks != 1 {
		t.Fatalf("status=%d flushed=%t checks=%d", w.status, w.flushed, a.checks)
	}
	if len(originals.opened) != 1 || !originals.opened[0].closed {
		t.Fatal("original ownership was not released after flush")
	}
	if !bytes.Equal(w.body.Bytes(), originals.opened[0].payload) {
		t.Fatal("download bytes differ")
	}
	if w.header.Get("Cache-Control") != "no-store" || w.header.Get("X-Content-Type-Options") != "nosniff" || w.header.Get("Content-Type") != "application/pdf" {
		t.Fatal("missing attachment response protections")
	}
}

type attachmentOwnershipWriter struct {
	t         *testing.T
	originals *attachmentTestOriginals
	header    http.Header
	status    int
	body      bytes.Buffer
	flushed   bool
}

func (w *attachmentOwnershipWriter) Header() http.Header    { return w.header }
func (w *attachmentOwnershipWriter) WriteHeader(status int) { w.status = status }
func (w *attachmentOwnershipWriter) Write(p []byte) (int, error) {
	if len(w.originals.opened) != 1 || w.originals.opened[0].closed {
		w.t.Fatal("original was closed before writing")
	}
	return w.body.Write(p)
}
func (w *attachmentOwnershipWriter) Flush() {
	if w.originals.opened[0].closed {
		w.t.Fatal("original was closed before flushing")
	}
	w.flushed = true
}

func TestAttachmentDownloadRetainsOriginalGrantAndGuard(t *testing.T) {
	cases := []struct {
		name   string
		change func(*attachmentService, *attachmentTestArchive, *attachmentTestOriginals, *attachmentTestRegistry)
	}{
		{"credential revoked", func(_ *attachmentService, a *attachmentTestArchive, _ *attachmentTestOriginals, _ *attachmentTestRegistry) {
			a.denied = true
		}},
		{"credential revision changed", func(_ *attachmentService, a *attachmentTestArchive, _ *attachmentTestOriginals, _ *attachmentTestRegistry) {
			a.grant.CredentialRevision++
		}},
		{"client unpublished", func(_ *attachmentService, _ *attachmentTestArchive, _ *attachmentTestOriginals, r *attachmentTestRegistry) {
			r.published = false
		}},
		{"lifecycle replaced", func(s *attachmentService, _ *attachmentTestArchive, _ *attachmentTestOriginals, _ *attachmentTestRegistry) {
			s.current = func() bool { return false }
		}},
		{"source changed", func(_ *attachmentService, a *attachmentTestArchive, _ *attachmentTestOriginals, _ *attachmentTestRegistry) {
			a.checkErr = archive.ErrSourceUnavailable
		}},
		{"recovery epoch changed", func(_ *attachmentService, a *attachmentTestArchive, _ *attachmentTestOriginals, _ *attachmentTestRegistry) {
			a.view.Snapshot.RecoveryEpoch++
		}},
		{"original changed", func(_ *attachmentService, _ *attachmentTestArchive, o *attachmentTestOriginals, _ *attachmentTestRegistry) {
			prior := o.makeOriginal
			o.makeOriginal = func() *attachmentTestOriginal {
				result := prior()
				result.recheckErr = media.ErrSourceChanged
				return result
			}
		}},
		{"replacement artifact", func(_ *attachmentService, _ *attachmentTestArchive, o *attachmentTestOriginals, _ *attachmentTestRegistry) {
			prior := o.makeOriginal
			o.makeOriginal = func() *attachmentTestOriginal {
				result := prior()
				result.metadata.ArtifactID = "replacement"
				return result
			}
		}},
		{"replacement source", func(_ *attachmentService, _ *attachmentTestArchive, o *attachmentTestOriginals, _ *attachmentTestRegistry) {
			prior := o.makeOriginal
			o.makeOriginal = func() *attachmentTestOriginal {
				result := prior()
				result.metadata.Source.Version.Revision++
				return result
			}
		}},
		{"mutated buffered bytes", func(_ *attachmentService, _ *attachmentTestArchive, o *attachmentTestOriginals, _ *attachmentTestRegistry) {
			prior := o.makeOriginal
			o.makeOriginal = func() *attachmentTestOriginal { result := prior(); result.mutateRead = true; return result }
		}},
		{"expiry during verification", func(s *attachmentService, _ *attachmentTestArchive, o *attachmentTestOriginals, _ *attachmentTestRegistry) {
			prior := o.makeOriginal
			o.makeOriginal = func() *attachmentTestOriginal {
				result := prior()
				result.onRecheck = func() { s.now = func() time.Time { return time.Now().Add(time.Hour) } }
				return result
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, a, originals, registry, token := attachmentServiceFixture(t)
			c.change(s, a, originals, registry)
			w := attachmentGET(s, token)
			if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"attachment_unavailable"}` {
				t.Fatalf("leaked attachment response: status=%d body=%q", w.Code, w.Body.String())
			}
			for _, original := range originals.opened {
				if !original.closed {
					t.Fatal("failed download leaked ownership")
				}
			}
		})
	}
}

func TestAttachmentDownloadRechecksAfterReading(t *testing.T) {
	for _, stage := range []string{"read", "recheck"} {
		t.Run(stage, func(t *testing.T) {
			s, a, originals, _, token := attachmentServiceFixture(t)
			prior := originals.makeOriginal
			originals.makeOriginal = func() *attachmentTestOriginal {
				result := prior()
				withdraw := func() { a.mu.Lock(); a.denied = true; a.mu.Unlock() }
				if stage == "read" {
					result.onRead = withdraw
				} else {
					result.onRecheck = withdraw
				}
				return result
			}
			w := attachmentGET(s, token)
			if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "%PDF") {
				t.Fatal("withdrawn original escaped final credential check")
			}
		})
	}
}

func TestAttachmentDownloadTicketExpiryCanonicalPathAndRetry(t *testing.T) {
	s, _, _, _, token := attachmentServiceFixture(t)
	if first, retry := attachmentGET(s, token), attachmentGET(s, token); first.Code != 200 || retry.Code != 200 {
		t.Fatal("valid host retry was refused")
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if w := attachmentGET(s, token); w.Code != 404 {
		t.Fatal("expired ticket was admitted")
	}
	for _, path := range []string{"/mcp/attachments/" + token + "?x=1", "/mcp/attachments/" + token + "/", "/mcp/attachments/%2e%2e", "/attachments/" + token, "/mcp/attachments/" + strings.Repeat("a", 43)} {
		w := httptest.NewRecorder()
		s.ServeDownload(w, httptest.NewRequest(http.MethodGet, "https://archive.example"+path, nil))
		if w.Code != 404 {
			t.Fatalf("noncanonical ticket path admitted: %q", path)
		}
	}
}

func TestAttachmentDownloadBudgetAndSlotAdmission(t *testing.T) {
	s, _, originals, _, token := attachmentServiceFixture(t)
	s.slots <- struct{}{}
	s.slots <- struct{}{}
	if w := attachmentGET(s, token); w.Code != 404 || len(originals.opened) != 0 {
		t.Fatal("busy download consumed media ownership")
	}
	<-s.slots
	<-s.slots
	ticket := s.tickets[token]
	ticket.metadata.Bytes = attachmentDownloadMaximum + 1
	s.tickets[token] = ticket
	if w := attachmentGET(s, token); w.Code != 404 || len(originals.opened) != 0 {
		t.Fatal("oversized ticket allocated an original")
	}
}

func TestAttachmentDownloadRootCancellation(t *testing.T) {
	s, _, originals, _, token := attachmentServiceFixture(t)
	root, cancel := context.WithCancel(context.Background())
	s.root = root
	cancel()
	if w := attachmentGET(s, token); w.Code != 404 || len(originals.opened) != 0 {
		t.Fatal("closed app still supplied originals")
	}
}

func TestAttachmentSearchEmptyPageKeepsOriginalGuard(t *testing.T) {
	s, a, _, _, _ := attachmentServiceFixture(t)
	request := archive.AttachmentSearchRequest{Query: "synthetic invoice", Filename: ".pdf", Before: 2, After: 3, Limit: 10}
	_, guard, err := s.Search(context.Background(), a.grant, request)
	if err != nil || a.checks != 1 || a.searched.Query != request.Query || len(guard.Sources) != len(a.guard.Sources) {
		t.Fatalf("search lost frame authority: %v", err)
	}
	a.checkErr = archive.ErrSourceUnavailable
	if _, _, err = s.Search(context.Background(), a.grant, request); err == nil {
		t.Fatal("invalidated empty search guard was accepted")
	}
}

func TestAttachmentPrepareQueuesOnlyRequestedAuthenticatedDescriptor(t *testing.T) {
	s, a, originals, _, _ := attachmentServiceFixture(t)
	a.view.Availability = "not_retained"
	a.grant.CredentialRevision = 7
	a.grant.Scope.AllChats = false
	a.grant.Scope.Chats = []archive.ChatID{"synthetic-scoped-chat"}
	a.grant.Scope.From = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a.grant.Scope.Until = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a.grant.Scope.Rolling = 30 * 24 * time.Hour
	calls := 0
	s.queue = func(_ context.Context, grant archive.CredentialGrant, id archive.MessageID) (bool, error) {
		calls++
		if grant.CredentialID != a.grant.CredentialID || grant.CredentialRevision != a.grant.CredentialRevision || !reflect.DeepEqual(grant.Scope, a.grant.Scope) || id != a.view.MessageID {
			t.Fatal("queue lost authenticated source identity")
		}
		return true, nil
	}
	result, _, err := s.Prepare(context.Background(), a.grant, a.view.AttachmentID, false, false)
	if err != nil || result.Status != "not_retained" || calls != 0 || len(originals.opened) != 0 {
		t.Fatalf("unexpected unrequested fetch: %v", err)
	}
	result, _, err = s.Prepare(context.Background(), a.grant, a.view.AttachmentID, true, false)
	if err != nil || result.Status != "queued" || calls != 1 || result.DownloadURL != "" || result.InlineBase64 != "" {
		t.Fatalf("authenticated queue failed: %v", err)
	}
	a.denied = true
	if _, _, err = s.Prepare(context.Background(), a.grant, a.view.AttachmentID, true, false); err == nil || calls != 1 {
		t.Fatal("revoked credential queued media")
	}
}

func TestAttachmentServiceRejectsUnsafePublicURL(t *testing.T) {
	s, a, originals, registry, _ := attachmentServiceFixture(t)
	_ = s
	for _, value := range []string{"http://archive.example/mcp", "https://user:password@archive.example/mcp", "https://archive.example/mcp?key=secret", "https://archive.example/mcp#fragment", "https://archive.example/%6dcp", "https://archive.example/mcp/", "https://archive.example/other"} {
		if _, err := newAttachmentService(attachmentServiceConfig{root: context.Background(), archive: a, originals: originals, publicURL: value, nativeClients: registry, remoteClients: registry, current: func() bool { return true }}); !errors.Is(err, mcpserver.ErrConfiguration) {
			t.Fatalf("unsafe public URL accepted: %q (%v)", value, err)
		}
	}
}

func TestAttachmentFilenameCannotInjectHeaders(t *testing.T) {
	s, _, _, _, token := attachmentServiceFixture(t)
	ticket := s.tickets[token]
	ticket.filename = "../../\u202eevil\r\nX-Evil: injected\\report.pdf"
	s.tickets[token] = ticket
	w := attachmentGET(s, token)
	if w.Code != 200 || strings.ContainsAny(w.Header().Get("Content-Disposition"), "\r\n\u202e") || w.Header().Get("X-Evil") != "" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("unsafe download filename escaped")
	}
}

func TestAttachmentTicketsHaveBoundedLifetimeCountAndCopiedAuthority(t *testing.T) {
	s, a, originals, _, token := attachmentServiceFixture(t)
	initial := s.tickets[token]
	for i := 1; i < attachmentTicketMaximum; i++ {
		url, expires, err := s.mintTicket(a.grant, a.guard, originals.makeOriginal().metadata, "synthetic.pdf", a.view.Snapshot)
		if err != nil || !strings.HasPrefix(url, "https://archive.example/mcp/attachments/") || expires.After(time.Now().Add(attachmentTicketTTL+time.Second)) {
			t.Fatalf("ticket mint failed: %v", err)
		}
	}
	if _, _, err := s.mintTicket(a.grant, a.guard, initial.metadata, "synthetic.pdf", a.view.Snapshot); err == nil || len(s.tickets) != attachmentTicketMaximum {
		t.Fatal("ticket count limit was exceeded")
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	grant := a.grant
	grant.Scope.Chats = []archive.ChatID{"original-chat"}
	guard := a.guard
	guard.Sources = append([]archive.SourceReadVersion(nil), a.guard.Sources...)
	url, _, err := s.mintTicket(grant, guard, initial.metadata, "synthetic.pdf", a.view.Snapshot)
	if err != nil || len(s.tickets) != 1 {
		t.Fatal("expired tickets were not reclaimed")
	}
	grant.Scope.Chats[0] = "replacement-chat"
	guard.Sources[0].Ref.ContentHash = "replacement-content"
	created := s.tickets[strings.TrimPrefix(url, "https://archive.example/mcp/attachments/")]
	if created.grant.Scope.Chats[0] != "original-chat" || created.guard.Sources[0].Ref.ContentHash != initial.metadata.Source.ContentHash {
		t.Fatal("ticket borrowed mutable authority slices")
	}
}

func TestAttachmentServiceInvalidationRevokesTicketsAndNewReads(t *testing.T) {
	s, a, originals, _, token := attachmentServiceFixture(t)
	s.invalidate()
	if len(s.tickets) != 0 {
		t.Fatal("invalidated service retained bearer tickets")
	}
	if w := attachmentGET(s, token); w.Code != 404 || len(originals.opened) != 0 {
		t.Fatal("invalidated lifetime served media")
	}
	if _, _, err := s.Search(context.Background(), a.grant, archive.AttachmentSearchRequest{}); err == nil {
		t.Fatal("invalidated lifetime searched old archive")
	}
	if _, _, err := s.mintTicket(a.grant, a.guard, originals.makeOriginal().metadata, "synthetic.pdf", a.view.Snapshot); err == nil {
		t.Fatal("invalidated lifetime published a new ticket")
	}
}

func TestAttachmentHistoricalUnsetFormatRetainsVerifiedOriginalIdentity(t *testing.T) {
	_, _, originals, _, _ := attachmentServiceFixture(t)
	metadata := originals.makeOriginal().metadata
	artifact := archive.MediaArtifact{ID: metadata.ArtifactID, MediaID: metadata.MediaID, Source: metadata.Source, SHA256: metadata.SHA256, Bytes: metadata.Bytes, Budget: archive.BudgetOriginals, State: "retained"}
	if !attachmentMatchesArtifact(metadata, artifact, metadata.Source.Version.MessageID) {
		t.Fatal("historical unset format blocked byte-verified original")
	}
	artifact.ActualFormat = metadata.Format
	if !attachmentMatchesArtifact(metadata, artifact, metadata.Source.Version.MessageID) {
		t.Fatal("matching declared format blocked verified original")
	}
	artifact.ActualFormat = "png"
	if attachmentMatchesArtifact(metadata, artifact, metadata.Source.Version.MessageID) {
		t.Fatal("different persisted format was accepted")
	}
	artifact.ActualFormat = ""
	artifact.SHA256 = strings.Repeat("0", 64)
	if attachmentMatchesArtifact(metadata, artifact, metadata.Source.Version.MessageID) {
		t.Fatal("unset format bypassed original byte identity")
	}
}

// The archive issues the private artifact witness and the actual media store
// publishes authenticated bytes. This does not construct a WhatsApp connector.
func attachmentRetainedFixture(t *testing.T, size int, publicURL string) (*attachmentService, archive.CredentialGrant, archive.MediaID, []byte) {
	t.Helper()
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if errors.Is(err, archive.ErrFTS5Unavailable) {
		t.Skip("retained fixture needs SQLite FTS5")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(ctx); err != nil {
			t.Errorf("synthetic archive cleanup: %v", err)
		}
	})
	policy, err := a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if size < 16 {
		t.Fatal("synthetic PDF needs bounded header and EOF")
	}
	payload := bytes.Repeat([]byte(" "), size)
	copy(payload, "%PDF-1.7\n")
	copy(payload[len(payload)-7:], "\n%%EOF\n")
	digest := sha256.Sum256(payload)
	descriptor, err := proto.Marshal(&waE2E.DocumentMessage{DirectPath: proto.String("/synthetic-document.enc"), MediaKey: bytes.Repeat([]byte{4}, 32), FileSHA256: digest[:], FileEncSHA256: bytes.Repeat([]byte{5}, 32), FileLength: proto.Uint64(uint64(len(payload)))})
	if err != nil {
		t.Fatal(err)
	}
	event := archive.SourceEvent{Key: archive.SourceKey{Account: "synthetic-attachment-account", Chat: "synthetic-chat@s.whatsapp.net", Sender: "synthetic-sender@s.whatsapp.net", Message: "synthetic-document-message"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageDocument, Caption: "synthetic invoice", Media: []archive.MediaReference{{Kind: "document", ProtocolReference: descriptor, Filename: "חשבונית synthetic.pdf", MimeType: "application/pdf", DeclaredBytes: int64(len(payload)), Availability: "not_retained"}}}
	applied, err := a.store.ApplySourceEvent(ctx, event, policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatalf("synthetic attachment source: %#v %v", applied, err)
	}
	id := applied.MediaIDs[0]
	downloader, err := a.resources.media.MediaAdmitter(ctx, ingestion.MediaDownloadRequest{Key: event.Key, Version: applied.Version, MediaID: id, Kind: archive.MessageDocument, DeclaredBytes: int64(len(payload)), ProtocolSHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = downloader.Close() })
	if _, err = downloader.File().Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = downloader.Complete(ctx, ingestion.VerifiedMedia{Key: event.Key, Version: applied.Version, Kind: archive.MessageDocument, SHA256: digest, Bytes: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if err = downloader.Close(); err != nil {
		t.Fatal(err)
	}
	tokenDigest := sha256.Sum256([]byte("synthetic attachment credential token"))
	created, err := a.store.CreateCredential(ctx, archive.CredentialCreate{Label: "synthetic attachment client", Principal: archive.RemoteClientPrincipalPrefix + "v1/0123456789abcdef0123456789abcdef", Digest: tokenDigest, Policy: archive.CredentialPolicy{Account: event.Key.Account, AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := a.store.ResolveCredentialID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	registry := &attachmentTestRegistry{published: true}
	s, err := newAttachmentService(attachmentServiceConfig{root: a.queryContext, archive: a.store, originals: newAttachmentOriginals(a.resources.media), publicURL: publicURL, nativeClients: registry, remoteClients: registry, current: func() bool { return !a.closed }})
	if err != nil {
		t.Fatal(err)
	}
	return s, grant, id, payload
}

func TestAttachmentRetainedPrepareCreatesGuardedTicketAndExactInline(t *testing.T) {
	s, grant, id, payload := attachmentRetainedFixture(t, 128, "https://archive.example/mcp")
	delivery, guard, err := s.Prepare(context.Background(), grant, id, false, true)
	if err != nil || delivery.Status != "ready" || delivery.DownloadURL == "" || delivery.ExpiresAt.IsZero() || delivery.Attachment.AttachmentID != id {
		t.Fatalf("retained prepare failed: %#v %v", delivery, err)
	}
	inline, err := base64.StdEncoding.DecodeString(delivery.InlineBase64)
	if err != nil || !bytes.Equal(inline, payload) {
		t.Fatal("inline bytes differ from authenticated original", err)
	}
	if _, err = s.archive.AttachmentResponseCheck(context.Background(), grant.Scope, archive.ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatal("original prepare guard was lost", err)
	}
	ticket := strings.TrimPrefix(delivery.DownloadURL, "https://archive.example/mcp/attachments/")
	w := attachmentGET(s, ticket)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), payload) || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("prepare to original download failed: status=%d", w.Code)
	}
	if err = s.archive.(*archive.Store).RevokeCredential(context.Background(), grant.CredentialID, grant.CredentialRevision); err != nil {
		t.Fatal(err)
	}
	if retry := attachmentGET(s, ticket); retry.Code != http.StatusNotFound || strings.Contains(retry.Body.String(), "%PDF") {
		t.Fatal("existing ticket survived credential revocation")
	}
}

func TestAttachmentRetainedInlineBoundariesAndNativeOnlyFallback(t *testing.T) {
	for _, tc := range []struct {
		name, publicURL, status, code string
		size                          int
		inline, wantInline, wantURL   bool
	}{
		{"exact inline maximum", "https://archive.example/mcp", "ready", "", int(attachmentInlineMaximum), true, true, true},
		{"larger inline uses URL", "https://archive.example/mcp", "ready", "inline_too_large", int(attachmentInlineMaximum + 1), true, false, true},
		{"URL only", "https://archive.example/mcp", "ready", "", 128, false, false, true},
		{"native inline", "", "ready", "", 128, true, true, false},
		{"native needs remote download", "", "remote_download_required", "", 128, false, false, false},
		{"native oversized inline", "", "remote_download_required", "inline_too_large", int(attachmentInlineMaximum + 1), true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, grant, id, payload := attachmentRetainedFixture(t, tc.size, tc.publicURL)
			delivery, _, err := s.Prepare(context.Background(), grant, id, false, tc.inline)
			if err != nil || delivery.Status != tc.status || delivery.Code != tc.code || (delivery.InlineBase64 != "") != tc.wantInline || (delivery.DownloadURL != "") != tc.wantURL {
				t.Fatalf("wrong retained boundary: status=%q code=%q inline=%t url=%t err=%v", delivery.Status, delivery.Code, delivery.InlineBase64 != "", delivery.DownloadURL != "", err)
			}
			if tc.wantInline {
				decoded, err := base64.StdEncoding.DecodeString(delivery.InlineBase64)
				if err != nil || !bytes.Equal(decoded, payload) {
					t.Fatal("boundary inline changed original bytes", err)
				}
			}
		})
	}
}

type attachmentFailureWriter struct {
	header    http.Header
	originals *attachmentTestOriginals
	t         *testing.T
	writeErr  bool
	flushed   bool
}

func (w *attachmentFailureWriter) Header() http.Header { return w.header }
func (w *attachmentFailureWriter) WriteHeader(int)     {}
func (w *attachmentFailureWriter) Write(p []byte) (int, error) {
	if w.originals.opened[0].closed {
		w.t.Fatal("ownership ended before failing write")
	}
	if w.writeErr {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}
func (w *attachmentFailureWriter) FlushError() error {
	w.flushed = true
	if w.originals.opened[0].closed {
		w.t.Fatal("ownership ended before failing flush")
	}
	return io.ErrClosedPipe
}

func TestAttachmentDownloadWriteAndFlushFailureReleaseOwnership(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		t.Run(stage, func(t *testing.T) {
			s, _, originals, _, token := attachmentServiceFixture(t)
			writer := &attachmentFailureWriter{header: make(http.Header), originals: originals, t: t, writeErr: stage == "write"}
			s.ServeDownload(writer, httptest.NewRequest(http.MethodGet, "https://archive.example/mcp/attachments/"+token, nil))
			if len(originals.opened) != 1 || !originals.opened[0].closed || len(s.slots) != 0 {
				t.Fatal("failed response retained media or slot ownership")
			}
			if (stage == "flush") != writer.flushed {
				t.Fatal("unexpected flush after write failure")
			}
		})
	}
}

// A second request must not queue its already-checked authority behind a slow
// response. Withdrawal while queued cannot be repaired by ticket TTL alone.
func TestAttachmentFinalGateDeniesContentionAfterAuthorityWithdrawal(t *testing.T) {
	for _, source := range []bool{false, true} {
		t.Run(map[bool]string{false: "credential_revocation", true: "source_invalidation"}[source], func(t *testing.T) {
			s, a, _, _, token := attachmentServiceFixture(t)
			checked, releaseCheck := make(chan struct{}), make(chan struct{})
			atGate, releaseGate := make(chan struct{}), make(chan struct{})
			firstWrite, releaseWrite := make(chan struct{}), make(chan struct{})
			var checks atomic.Int32
			var clockGate atomic.Bool
			a.checkHook = func() {
				if checks.Add(1) == 1 {
					close(checked)
					<-releaseCheck
					clockGate.Store(true)
				}
			}
			s.now = func() time.Time {
				if clockGate.Swap(false) {
					close(atGate)
					<-releaseGate
				}
				return time.Now()
			}
			var checkOnce, gateOnce, writeOnce sync.Once
			allowCheck := func() { checkOnce.Do(func() { close(releaseCheck) }) }
			allowGate := func() { gateOnce.Do(func() { close(releaseGate) }) }
			allowWrite := func() { writeOnce.Do(func() { close(releaseWrite) }) }
			defer allowWrite()
			defer allowGate()
			defer allowCheck()
			second := httptest.NewRecorder()
			secondDone := make(chan struct{})
			go func() {
				defer close(secondDone)
				s.ServeDownload(second, httptest.NewRequest(http.MethodGet, "https://archive.example/mcp/attachments/"+token, nil))
			}()
			select {
			case <-checked:
			case <-time.After(time.Second):
				t.Fatal("second request did not reach final source check")
			}
			first := &attachmentBlockedWriter{ResponseRecorder: httptest.NewRecorder(), entered: firstWrite, release: releaseWrite}
			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				s.ServeDownload(first, httptest.NewRequest(http.MethodGet, "https://archive.example/mcp/attachments/"+token, nil))
			}()
			select {
			case <-firstWrite:
			case <-time.After(time.Second):
				t.Fatal("first response did not hold commit gate")
			}
			allowCheck()
			select {
			case <-atGate:
			case <-time.After(time.Second):
				t.Fatal("second response did not finish source authorization")
			}
			a.mu.Lock()
			if source {
				a.checkErr = archive.ErrSourceUnavailable
			} else {
				a.denied = true
			}
			a.mu.Unlock()
			allowGate()
			deniedWithoutQueue := false
			select {
			case <-secondDone:
				deniedWithoutQueue = true
			case <-time.After(time.Second):
			}
			allowWrite()
			for _, done := range []<-chan struct{}{firstDone, secondDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("fixture response failed to join")
				}
			}
			if !deniedWithoutQueue || second.Code != http.StatusNotFound || strings.Contains(second.Body.String(), "%PDF") {
				t.Fatalf("stale authorization queued behind response: prompt=%v status=%d", deniedWithoutQueue, second.Code)
			}
		})
	}
}

type attachmentBlockedWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release <-chan struct{}
}

func (w *attachmentBlockedWriter) Write(payload []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(payload)
}
