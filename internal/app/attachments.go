package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

const (
	attachmentDownloadMaximum int64 = 8 << 20
	attachmentInlineMaximum   int64 = 64 << 10
	attachmentTicketMaximum         = 64
	attachmentTicketTTL             = 2 * time.Minute
	attachmentRequestTimeout        = 15 * time.Second
)

type attachmentArchive interface {
	SearchAttachmentsGuarded(context.Context, archive.ReadScope, archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error)
	AttachmentGuarded(context.Context, archive.ReadScope, archive.MediaID) (archive.AttachmentView, archive.ResponseGuard, error)
	CheckResponse(context.Context, archive.ReadScope, archive.ResponseCheck) (archive.ReadSnapshot, error)
	AttachmentResponseCheck(context.Context, archive.ReadScope, archive.ResponseCheck) (archive.ReadSnapshot, error)
	CheckCredentialGrant(context.Context, archive.CredentialGrant) error
}

type attachmentOriginal interface {
	Metadata() media.OriginalMetadata
	Reader() io.ReaderAt
	Recheck(context.Context, archive.ReadScope) error
	Close() error
}

type attachmentOriginalOpener interface {
	Open(context.Context, archive.ReadScope, archive.MediaID) (attachmentOriginal, error)
}

type attachmentOriginals struct{ store *media.Store }

func newAttachmentOriginals(store *media.Store) attachmentOriginalOpener {
	if store == nil {
		return nil
	}
	return attachmentOriginals{store: store}
}
func (o attachmentOriginals) Open(ctx context.Context, scope archive.ReadScope, id archive.MediaID) (attachmentOriginal, error) {
	return o.store.OpenOriginal(ctx, scope, id)
}

type attachmentServiceConfig struct {
	historicalSnapshot bool

	root          context.Context
	archive       attachmentArchive
	originals     attachmentOriginalOpener
	publicURL     string
	nativeClients mcpserver.NativeClientRegistry
	remoteClients mcpserver.RemoteClientRegistry
	current       func() bool
	queue         func(context.Context, archive.CredentialGrant, archive.MessageID) (bool, error)
}

type attachmentTicket struct {
	grant    archive.CredentialGrant
	guard    archive.ResponseGuard
	metadata media.OriginalMetadata
	filename string
	snapshot archive.ReadSnapshot
	expires  time.Time
}

// Each service belongs to one captured archive/resource lifetime. Tickets retain
// the original grant and source guard; redemption cannot refresh their authority.
type attachmentService struct {
	historicalSnapshot bool

	root          context.Context
	archive       attachmentArchive
	originals     attachmentOriginalOpener
	publicURL     string
	nativeClients mcpserver.NativeClientRegistry
	remoteClients mcpserver.RemoteClientRegistry
	current       func() bool
	queue         func(context.Context, archive.CredentialGrant, archive.MessageID) (bool, error)
	slots         chan struct{}
	mu            sync.Mutex
	tickets       map[string]attachmentTicket
	invalidated   bool
	now           func() time.Time
	random        io.Reader
}

var _ mcpserver.Attachments = (*attachmentService)(nil)

func newAttachmentService(c attachmentServiceConfig) (*attachmentService, error) {
	if c.root == nil || attachmentNil(c.archive) || (attachmentNil(c.originals) && !c.historicalSnapshot) || c.current == nil {
		return nil, mcpserver.ErrConfiguration
	}
	if !validAttachmentPublicURL(c.publicURL) {
		return nil, mcpserver.ErrConfiguration
	}
	return &attachmentService{historicalSnapshot: c.historicalSnapshot, root: c.root, archive: c.archive, originals: c.originals, publicURL: c.publicURL, nativeClients: c.nativeClients, remoteClients: c.remoteClients, current: c.current, queue: c.queue, slots: make(chan struct{}, 2), tickets: make(map[string]attachmentTicket), now: time.Now, random: rand.Reader}, nil
}

func validAttachmentPublicURL(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "/mcp" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.String() == value
}

// All transports retain this service. Rebinding its authority atomically clears
// tickets without replacing the instance captured by native MCP handlers.
func (s *attachmentService) setPublicURL(value string) error {
	if !validAttachmentPublicURL(value) {
		return mcpserver.ErrConfiguration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated {
		return mcpserver.ErrUnavailable
	}
	if s.publicURL != value {
		clear(s.tickets)
		s.publicURL = value
	}
	return nil
}
func (s *attachmentService) hasPublicURL() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.invalidated && s.publicURL != ""
}

func attachmentNil(value any) bool {
	if value == nil {
		return true
	}
	r := reflect.ValueOf(value)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

func (s *attachmentService) request(ctx context.Context) (context.Context, func()) {
	bounded, cancel := context.WithTimeout(ctx, attachmentRequestTimeout)
	stop := context.AfterFunc(s.root, cancel)
	if s.root.Err() != nil {
		cancel()
	}
	return bounded, func() { stop(); cancel() }
}

func canonicalAttachmentPrincipal(principal, prefix string) bool {
	nonce, ok := strings.CutPrefix(principal, prefix+"v1/")
	if !ok || len(nonce) != 32 {
		return false
	}
	for _, c := range nonce {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *attachmentService) published(ctx context.Context, grant archive.CredentialGrant) bool {
	s.mu.Lock()
	invalidated := s.invalidated
	s.mu.Unlock()
	if invalidated || ctx.Err() != nil || s.root.Err() != nil || !s.current() || grant.CredentialID == "" || grant.CredentialRevision == 0 || grant.Scope.Account == "" || grant.Scope.Principal == "" || grant.Scope.PolicyGeneration == 0 {
		return false
	}
	principal := grant.Scope.Principal
	if principal == archive.LegacyNativeClientPrincipal || strings.HasPrefix(principal, archive.NativeClientPrincipalPrefix) {
		return (principal == archive.LegacyNativeClientPrincipal || canonicalAttachmentPrincipal(principal, archive.NativeClientPrincipalPrefix)) && !attachmentNil(s.nativeClients) && s.nativeClients.PublishedNativeClient(mcpserver.NativeClientBinding{Account: grant.Scope.Account, Principal: principal, CredentialID: grant.CredentialID})
	}
	if strings.HasPrefix(principal, archive.RemoteClientPrincipalPrefix) {
		return canonicalAttachmentPrincipal(principal, archive.RemoteClientPrincipalPrefix) && !attachmentNil(s.remoteClients) && s.remoteClients.PublishedRemoteClient(mcpserver.RemoteClientBinding{Account: grant.Scope.Account, Principal: principal, CredentialID: grant.CredentialID})
	}
	return true
}

func (s *attachmentService) authorize(ctx context.Context, grant archive.CredentialGrant) error {
	if !s.published(ctx, grant) {
		return mcpserver.ErrUnauthorized
	}
	if err := s.archive.CheckCredentialGrant(ctx, grant); err != nil {
		return err
	}
	return nil
}

func (s *attachmentService) Search(ctx context.Context, grant archive.CredentialGrant, request archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error) {
	ctx, done := s.request(ctx)
	defer done()
	if err := s.authorize(ctx, grant); err != nil {
		return archive.AttachmentPage{}, archive.ResponseGuard{}, err
	}
	page, guard, err := s.archive.SearchAttachmentsGuarded(ctx, grant.Scope, request)
	if err != nil {
		return archive.AttachmentPage{}, archive.ResponseGuard{}, err
	}
	if !s.published(ctx, grant) {
		return archive.AttachmentPage{}, archive.ResponseGuard{}, mcpserver.ErrUnauthorized
	}
	if _, err = s.archive.CheckResponse(ctx, grant.Scope, archive.ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		return archive.AttachmentPage{}, archive.ResponseGuard{}, err
	}
	if ctx.Err() != nil || s.root.Err() != nil {
		return archive.AttachmentPage{}, archive.ResponseGuard{}, mcpserver.ErrUnavailable
	}
	return page, guard, nil
}

func (s *attachmentService) Prepare(ctx context.Context, grant archive.CredentialGrant, id archive.MediaID, fetchIfNeeded, inline bool) (mcpserver.AttachmentDelivery, archive.ResponseGuard, error) {
	ctx, done := s.request(ctx)
	defer done()
	fail := func(err error) (mcpserver.AttachmentDelivery, archive.ResponseGuard, error) {
		return mcpserver.AttachmentDelivery{}, archive.ResponseGuard{}, err
	}
	if err := s.authorize(ctx, grant); err != nil {
		return fail(err)
	}
	view, guard, err := s.archive.AttachmentGuarded(ctx, grant.Scope, id)
	if err != nil {
		return fail(err)
	}
	if view.AttachmentID != id || view.MessageID == "" {
		return fail(mcpserver.ErrUnavailable)
	}
	delivery := mcpserver.AttachmentDelivery{Attachment: view, Snapshot: view.Snapshot, Status: "not_retained"}
	if s.historicalSnapshot {
		if err = s.checkAttachment(ctx, grant, guard, view.Snapshot); err != nil {
			return fail(err)
		}
		delivery.Status = "unavailable"
		delivery.Code = "historical_snapshot_read_only"
		return delivery, guard, nil
	}
	artifact, retained := view.OriginalArtifact()
	if !retained {
		if err = s.checkAttachment(ctx, grant, guard, view.Snapshot); err != nil {
			return fail(err)
		}
		if fetchIfNeeded && s.queue != nil && view.Kind == archive.MessageKind("document") {
			queued, queueErr := s.queue(ctx, grant, view.MessageID)
			if queueErr != nil {
				return fail(queueErr)
			}
			if queued {
				delivery.Status = "queued"
			}
		}
		if err = s.checkAttachment(ctx, grant, guard, view.Snapshot); err != nil {
			return fail(err)
		}
		return delivery, guard, nil
	}
	if artifact.ID == "" || artifact.MediaID != id || artifact.State != "retained" || artifact.Budget != archive.BudgetOriginals || artifact.Bytes <= 0 {
		return fail(mcpserver.ErrUnavailable)
	}
	if artifact.Bytes > attachmentDownloadMaximum {
		delivery.Status = "unavailable"
		delivery.Code = "attachment_too_large"
		if err = s.checkAttachment(ctx, grant, guard, view.Snapshot); err != nil {
			return fail(err)
		}
		return delivery, guard, nil
	}
	if !s.acquire() {
		return fail(mcpserver.ErrUnavailable)
	}
	defer s.release()
	original, err := s.originals.Open(ctx, grant.Scope, id)
	if err != nil {
		return fail(err)
	}
	defer original.Close()
	metadata := original.Metadata()
	if !attachmentMatchesArtifact(metadata, artifact, view.MessageID) {
		return fail(mcpserver.ErrUnavailable)
	}
	if inline && metadata.Bytes <= attachmentInlineMaximum {
		payload, readErr := readAttachment(ctx, original, metadata, attachmentInlineMaximum)
		if readErr != nil {
			return fail(readErr)
		}
		delivery.InlineBase64 = base64.StdEncoding.EncodeToString(payload)
		clear(payload)
	} else if inline {
		delivery.Code = "inline_too_large"
	}
	if err = original.Recheck(ctx, grant.Scope); err != nil {
		return fail(err)
	}
	if err = s.checkAttachment(ctx, grant, guard, view.Snapshot); err != nil {
		return fail(err)
	}
	if s.hasPublicURL() {
		delivery.DownloadURL, delivery.ExpiresAt, err = s.mintTicket(grant, guard, metadata, view.Filename, view.Snapshot)
		if err != nil {
			return fail(err)
		}
	}
	if delivery.DownloadURL == "" && delivery.InlineBase64 == "" {
		delivery.Status = "remote_download_required"
	} else {
		delivery.Status = "ready"
	}
	return delivery, guard, nil
}

// Only the final source/credential fence may authorize captured bytes. Reading
// a new attachment or grant here would accidentally bless an old response.
func (s *attachmentService) checkAttachment(ctx context.Context, grant archive.CredentialGrant, guard archive.ResponseGuard, snapshot archive.ReadSnapshot) error {
	if err := s.authorize(ctx, grant); err != nil {
		return err
	}
	if !s.published(ctx, grant) {
		return mcpserver.ErrUnauthorized
	}
	current, err := s.archive.AttachmentResponseCheck(ctx, grant.Scope, archive.ResponseCheck{Guard: guard, Credential: &grant})
	if err != nil {
		return err
	}
	if current.RecoveryEpoch != snapshot.RecoveryEpoch || current.PolicyGeneration != snapshot.PolicyGeneration || ctx.Err() != nil || s.root.Err() != nil {
		return mcpserver.ErrUnavailable
	}
	return nil
}

func (s *attachmentService) acquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}
func (s *attachmentService) release() { <-s.slots }

func cloneAttachmentGrant(grant archive.CredentialGrant) archive.CredentialGrant {
	grant.Scope.Chats = append([]archive.ChatID(nil), grant.Scope.Chats...)
	return grant
}
func cloneAttachmentGuard(guard archive.ResponseGuard) archive.ResponseGuard {
	guard.Sources = append([]archive.SourceReadVersion(nil), guard.Sources...)
	return guard
}

func (s *attachmentService) mintTicket(grant archive.CredentialGrant, guard archive.ResponseGuard, metadata media.OriginalMetadata, filename string, snapshot archive.ReadSnapshot) (string, time.Time, error) {
	if !validAttachmentMetadata(metadata) {
		return "", time.Time{}, mcpserver.ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated || s.publicURL == "" {
		return "", time.Time{}, mcpserver.ErrUnavailable
	}
	now := s.now()
	for token, ticket := range s.tickets {
		if !now.Before(ticket.expires) {
			delete(s.tickets, token)
		}
	}
	if len(s.tickets) >= attachmentTicketMaximum {
		return "", time.Time{}, mcpserver.ErrUnavailable
	}
	for attempt := 0; attempt < 3; attempt++ {
		var nonce [32]byte
		if _, err := io.ReadFull(s.random, nonce[:]); err != nil {
			return "", time.Time{}, mcpserver.ErrUnavailable
		}
		token := base64.RawURLEncoding.EncodeToString(nonce[:])
		if _, exists := s.tickets[token]; exists {
			continue
		}
		expires := now.Add(attachmentTicketTTL)
		s.tickets[token] = attachmentTicket{grant: cloneAttachmentGrant(grant), guard: cloneAttachmentGuard(guard), metadata: metadata, filename: safeAttachmentFilename(filename), snapshot: snapshot, expires: expires}
		return s.publicURL + "/attachments/" + token, expires, nil
	}
	return "", time.Time{}, mcpserver.ErrUnavailable
}

func (s *attachmentService) ticket(token string) (attachmentTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated {
		return attachmentTicket{}, false
	}
	ticket, ok := s.tickets[token]
	if ok && !s.now().Before(ticket.expires) {
		delete(s.tickets, token)
		return attachmentTicket{}, false
	}
	return ticket, ok
}

func (s *attachmentService) invalidate() {
	s.mu.Lock()
	s.invalidated = true
	clear(s.tickets)
	s.mu.Unlock()
}

func (s *attachmentService) ServeDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	deny := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"attachment_unavailable"}`)
	}
	if !s.hasPublicURL() || r.Method != http.MethodGet || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		deny()
		return
	}
	token, ok := strings.CutPrefix(r.URL.Path, "/mcp/attachments/")
	if !ok || len(token) != 43 {
		deny()
		return
	}
	nonce, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != token {
		deny()
		return
	}
	ticket, ok := s.ticket(token)
	if !ok || !validAttachmentMetadata(ticket.metadata) {
		deny()
		return
	}
	ctx, done := s.request(r.Context())
	defer done()
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline)
	if err = s.authorize(ctx, ticket.grant); err != nil || !s.acquire() {
		deny()
		return
	}
	defer s.release()
	original, err := s.originals.Open(ctx, ticket.grant.Scope, ticket.metadata.MediaID)
	if err != nil {
		deny()
		return
	}
	defer original.Close()
	if !sameAttachmentOriginal(original.Metadata(), ticket.metadata) {
		deny()
		return
	}
	payload, err := readAttachment(ctx, original, ticket.metadata, attachmentDownloadMaximum)
	if err != nil {
		deny()
		return
	}
	defer clear(payload)
	contentType := attachmentContentType(ticket.metadata.Format)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": safeAttachmentFilename(ticket.filename)})
	if err = original.Recheck(ctx, ticket.grant.Scope); err != nil {
		deny()
		return
	}
	if err = s.checkAttachment(ctx, ticket.grant, ticket.guard, ticket.snapshot); err != nil || !s.now().Before(ticket.expires) {
		deny()
		return
	}
	// Never queue after the final source/credential fence: another response
	// can hold this gate through a slow flush while authority is withdrawn.
	// Deny contention so a retry must perform fresh authorization. Once held,
	// keep authority stable through the bounded response write and flush.
	if !s.mu.TryLock() {
		deny()
		return
	}
	defer s.mu.Unlock()
	if _, exists := s.tickets[token]; s.invalidated || !exists || !s.now().Before(ticket.expires) {
		deny()
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.FormatInt(ticket.metadata.Bytes, 10))
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(payload); err != nil {
		return
	}
	// The retained inode and both cleanup guards remain held until the actual
	// response writer has completed its flush, including a failed flush.
	_ = controller.Flush()
}

func validAttachmentMetadata(metadata media.OriginalMetadata) bool {
	if metadata.MediaID == "" || metadata.ArtifactID == "" || metadata.Source.Account == "" || metadata.Source.Version.MessageID == "" || metadata.Source.Version.Deleted || metadata.Source.Version.Excluded || metadata.Bytes <= 0 || metadata.Bytes > attachmentDownloadMaximum || len(metadata.SHA256) != 64 {
		return false
	}
	digest, err := hex.DecodeString(metadata.SHA256)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == metadata.SHA256
}

func readAttachment(ctx context.Context, original attachmentOriginal, metadata media.OriginalMetadata, maximum int64) ([]byte, error) {
	if !validAttachmentMetadata(metadata) || metadata.Bytes > maximum {
		return nil, mcpserver.ErrUnavailable
	}
	payload := make([]byte, int(metadata.Bytes))
	reader := io.NewSectionReader(original.Reader(), 0, metadata.Bytes)
	for offset := 0; offset < len(payload); {
		if ctx.Err() != nil {
			clear(payload)
			return nil, mcpserver.ErrUnavailable
		}
		end := min(offset+(64<<10), len(payload))
		if _, err := io.ReadFull(reader, payload[offset:end]); err != nil {
			clear(payload)
			return nil, mcpserver.ErrUnavailable
		}
		offset = end
	}
	digest := sha256.Sum256(payload)
	if ctx.Err() != nil || hex.EncodeToString(digest[:]) != metadata.SHA256 {
		clear(payload)
		return nil, mcpserver.ErrUnavailable
	}
	return payload, nil
}

func sameAttachmentSource(a, b archive.SourceRef) bool {
	return a.Account == b.Account && a.ContentHash == b.ContentHash && a.Version.MessageID == b.Version.MessageID && a.Version.Revision == b.Version.Revision && a.Version.PolicyGeneration == b.Version.PolicyGeneration && a.Version.Deleted == b.Version.Deleted && a.Version.Excluded == b.Version.Excluded && a.Version.ExpiresAt.Equal(b.Version.ExpiresAt)
}
func sameAttachmentOriginal(a, b media.OriginalMetadata) bool {
	return validAttachmentMetadata(a) && a.MediaID == b.MediaID && a.ArtifactID == b.ArtifactID && a.SHA256 == b.SHA256 && a.Bytes == b.Bytes && a.Format == b.Format && sameAttachmentSource(a.Source, b.Source)
}
func attachmentMatchesArtifact(metadata media.OriginalMetadata, artifact archive.MediaArtifact, message archive.MessageID) bool {
	return validAttachmentMetadata(metadata) && metadata.MediaID == artifact.MediaID && metadata.ArtifactID == artifact.ID && metadata.SHA256 == artifact.SHA256 && metadata.Bytes == artifact.Bytes && (artifact.ActualFormat == "" || metadata.Format == artifact.ActualFormat) && metadata.Source.Version.MessageID == message && sameAttachmentSource(metadata.Source, artifact.Source)
}

func attachmentContentType(format string) string {
	switch format {
	case "pdf":
		return "application/pdf"
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "ogg":
		return "audio/ogg"
	case "mp3":
		return "audio/mpeg"
	case "mp4", "m4a":
		return "audio/mp4"
	case "wav":
		return "audio/wav"
	case "webm":
		return "audio/webm"
	default:
		return "application/octet-stream"
	}
}

func safeAttachmentFilename(value string) string {
	// Retain ordinary Hebrew/Unicode names, but remove path separators, control
	// bytes and invisible format characters that can disguise extensions.
	var safe strings.Builder
	for _, c := range value {
		if c == '/' || c == '\\' || unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			continue
		}
		if safe.Len()+utf8.RuneLen(c) > 180 {
			break
		}
		safe.WriteRune(c)
	}
	name := strings.Trim(safe.String(), " .")
	if name == "" {
		return "attachment"
	}
	return name
}
