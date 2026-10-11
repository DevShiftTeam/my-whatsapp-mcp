// Package transcription connects source-bound voice extraction jobs to an
// authenticated batch service on this computer. It does not download audio URLs.
package transcription

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

const (
	maximumAudioBytes      = 25 << 20
	maximumTextBytes       = 64 << 10
	maximumResponseBytes   = 72 << 10
	maximumInfoBytes       = 64 << 10
	maximumDurationSeconds = 20 * 60
	defaultTimeout         = 10 * time.Minute
)

var (
	ErrInvalidRequest  = errors.New("transcription request invalid")
	ErrInvalidResponse = errors.New("transcription response invalid")
	ErrUnavailable     = errors.New("transcription unavailable")
	ErrBusy            = errors.New("transcription busy")
	ErrUnauthorized    = errors.New("transcription authentication failed")
	ErrModelChanged    = errors.New("transcription model changed")
	tokenPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
	languagePattern    = regexp.MustCompile(`^[a-z]{2,3}$`)
)

type Options struct {
	URL, Token, Language string
	Timeout              time.Duration
}

// Backend freezes the verified model recipe for the lifetime of a primary
// extraction version. The application owns job leases and commit authorization.
type Backend struct {
	options   Options
	baseURL   string
	client    *http.Client
	transport *http.Transport
	version   archive.ExtractionVersion
	identity  string
	gate      chan struct{}
	mu        sync.Mutex
	closed    bool
}

func normalizedOptions(o Options) (Options, string, error) {
	if o.Timeout == 0 {
		o.Timeout = defaultTimeout
	}
	if o.Language == "" {
		o.Language = "auto"
	}
	if !tokenPattern.MatchString(o.Token) || (o.Language != "auto" && !languagePattern.MatchString(o.Language)) || o.Timeout < 10*time.Second || o.Timeout > 30*time.Minute {
		return Options{}, "", ErrInvalidRequest
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return Options{}, "", ErrInvalidRequest
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 || u.Host != net.JoinHostPort(u.Hostname(), strconv.Itoa(port)) {
		return Options{}, "", ErrInvalidRequest
	}
	u.Path = ""
	return o, u.String(), nil
}

// ValidateOptions checks configuration without opening files or making requests.
func ValidateOptions(o Options) error { _, _, err := normalizedOptions(o); return err }

func New(ctx context.Context, options Options) (*Backend, error) {
	if ctx == nil {
		return nil, ErrInvalidRequest
	}
	o, base, err := normalizedOptions(options)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:              nil,
		DialContext:        (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression: true, MaxConnsPerHost: 2, MaxIdleConns: 1,
		MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout:  o.Timeout,
		MaxResponseHeaderBytes: 16 << 10,
	}
	b := &Backend{options: o, baseURL: base, transport: transport, gate: make(chan struct{}, 1)}
	b.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	info, err := b.fetchInfo(ctx)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	b.identity = info.Identity
	b.version, err = extractionVersion(info)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, ErrInvalidResponse
	}
	return b, nil
}

func (b *Backend) Version() archive.ExtractionVersion { return b.version }

// Ready checks availability without changing the frozen version or claiming a job.
func (b *Backend) Ready(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	if b.isClosed() {
		return ErrUnavailable
	}
	info, err := b.fetchInfo(ctx)
	if err != nil {
		return err
	}
	if info.Identity != b.identity {
		return ErrModelChanged
	}
	return nil
}

func (b *Backend) isClosed() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.closed }

func (b *Backend) Close(context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.transport.CloseIdleConnections()
	return nil
}

type originalInput interface {
	Metadata() media.OriginalMetadata
	Open() (*os.File, error)
}

func (b *Backend) ExtractPrimary(ctx context.Context, work archive.ExtractionWork, original *media.Original) (archive.ExtractionResult, error) {
	if original == nil {
		return archive.ExtractionResult{}, ErrInvalidRequest
	}
	return b.extractPrimary(ctx, work, original)
}

func validWork(work archive.ExtractionWork, version archive.ExtractionVersion) error {
	if work.ID == "" || work.Lease.JobID != work.ID || work.Lease.LeaseID == "" || work.LeaseUntil.IsZero() || !time.Now().Before(work.LeaseUntil) || work.MediaID == "" || work.ArtifactID == "" || !validDigest(work.ContentHash) || !validDigest(work.OriginalSHA256) {
		return ErrInvalidRequest
	}
	if work.Version != version {
		return media.ErrSourceChanged
	}
	source := work.Source
	if source.Account == "" || source.Version.MessageID == "" || source.Version.Revision == 0 || source.Version.PolicyGeneration == 0 || source.Version.Deleted || source.Version.Excluded || !validDigest(source.ContentHash) || (!source.Version.ExpiresAt.IsZero() && !time.Now().Before(source.Version.ExpiresAt)) {
		return media.ErrSourceChanged
	}
	return nil
}

func sameSource(a, b archive.SourceRef) bool {
	x, y := a.Version, b.Version
	return a.Account == b.Account && a.ContentHash == b.ContentHash && x.MessageID == y.MessageID && x.Revision == y.Revision && x.PolicyGeneration == y.PolicyGeneration && x.Deleted == y.Deleted && x.Excluded == y.Excluded && x.ExpiresAt.Equal(y.ExpiresAt)
}

func sameMetadata(a, b media.OriginalMetadata) bool {
	return a.MediaID == b.MediaID && a.ArtifactID == b.ArtifactID && sameSource(a.Source, b.Source) && a.SHA256 == b.SHA256 && a.Bytes == b.Bytes && a.Format == b.Format
}

func validOriginal(work archive.ExtractionWork, meta media.OriginalMetadata) error {
	if meta.Bytes <= 0 || meta.Bytes > maximumAudioBytes {
		return media.ErrLimit
	}
	if meta.MediaID != work.MediaID || meta.ArtifactID != work.ArtifactID || meta.SHA256 != work.OriginalSHA256 || !sameSource(meta.Source, work.Source) {
		return media.ErrSourceChanged
	}
	switch meta.Format {
	case "wav", "flac", "mp3", "m4a", "ogg", "opus":
		return nil
	default:
		return media.ErrUnsupported
	}
}

func (b *Backend) extractPrimary(ctx context.Context, work archive.ExtractionWork, original originalInput) (archive.ExtractionResult, error) {
	if ctx == nil || original == nil {
		return archive.ExtractionResult{}, ErrInvalidRequest
	}
	if b.isClosed() {
		return archive.ExtractionResult{}, ErrUnavailable
	}
	if err := validWork(work, b.version); err != nil {
		return archive.ExtractionResult{}, err
	}
	meta := original.Metadata()
	if err := validOriginal(work, meta); err != nil {
		return archive.ExtractionResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.options.Timeout)
	defer cancel()
	if !meta.Source.Version.ExpiresAt.IsZero() {
		var expireCancel context.CancelFunc
		ctx, expireCancel = context.WithDeadline(ctx, meta.Source.Version.ExpiresAt)
		defer expireCancel()
	}
	select {
	case b.gate <- struct{}{}:
		defer func() { <-b.gate }()
	case <-ctx.Done():
		return archive.ExtractionResult{}, ctx.Err()
	}
	if b.isClosed() {
		return archive.ExtractionResult{}, ErrUnavailable
	}
	data, err := copyOriginal(ctx, original, meta)
	if err != nil {
		return archive.ExtractionResult{}, err
	}
	upload := newUpload(data)
	// A RoundTripper can close the request body after Do returns. Cancel network
	// work and join that ownership acknowledgment before wiping bytes or releasing
	// the single-upload gate.
	defer func() {
		cancel()
		<-upload.done
		clear(data)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/v1/transcribe", upload)
	if err != nil {
		_ = upload.Close()
		return archive.ExtractionResult{}, ErrInvalidRequest
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Authorization", "Bearer "+b.options.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Audio-SHA256", meta.SHA256)
	req.Header.Set("X-Audio-Format", meta.Format)
	req.Header.Set("X-Transcription-Identity", b.identity)
	response, err := b.client.Do(req)
	if err != nil {
		return archive.ExtractionResult{}, requestError(ctx)
	}
	defer response.Body.Close()
	if err := responseStatus(response.StatusCode); err != nil {
		return archive.ExtractionResult{}, err
	}
	var output transcribeResponse
	if err := readJSON(response, maximumResponseBytes, &output); err != nil {
		if ctx.Err() != nil {
			return archive.ExtractionResult{}, ctx.Err()
		}
		return archive.ExtractionResult{}, err
	}
	if output.Identity != b.identity {
		return archive.ExtractionResult{}, ErrModelChanged
	}
	if err := validateTranscript(output, b.options.Language, meta.SHA256); err != nil {
		return archive.ExtractionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return archive.ExtractionResult{}, err
	}
	if !sameMetadata(meta, original.Metadata()) {
		return archive.ExtractionResult{}, media.ErrSourceChanged
	}
	if err := verifyOriginal(ctx, original, meta); err != nil {
		return archive.ExtractionResult{}, err
	}
	result := archive.ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: archive.ExtractionSilent}
	if output.Silent {
		return result, nil
	}
	result.Outcome = archive.ExtractionText
	result.Documents = []archive.Document{transcriptDocument(work, output)}
	return result, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

type uploadBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed bool
	done   chan struct{}
}

func newUpload(data []byte) *uploadBody {
	return &uploadBody{reader: bytes.NewReader(data), done: make(chan struct{})}
}

func (b *uploadBody) Read(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(data)
}

func (b *uploadBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.done)
	}
	return nil
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func openOriginal(original originalInput, meta media.OriginalMetadata) (*os.File, error) {
	f, err := original.Open()
	if err != nil {
		return nil, media.ErrSourceChanged
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != meta.Bytes {
		_ = f.Close()
		return nil, media.ErrSourceChanged
	}
	return f, nil
}

func copyOriginal(ctx context.Context, original originalInput, meta media.OriginalMetadata) ([]byte, error) {
	f, err := openOriginal(original, meta)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, meta.Bytes+1))
	if err != nil {
		clear(data)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, media.ErrSourceChanged
	}
	if int64(len(data)) != meta.Bytes || digest(data) != meta.SHA256 {
		clear(data)
		return nil, media.ErrSourceChanged
	}
	return data, nil
}

func verifyOriginal(ctx context.Context, original originalInput, meta media.OriginalMetadata) error {
	f, err := openOriginal(original, meta)
	if err != nil {
		return err
	}
	defer f.Close()
	h := newDigest()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, reader: f}, meta.Bytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return media.ErrSourceChanged
	}
	if n != meta.Bytes || digestString(h.Sum(nil)) != meta.SHA256 {
		return media.ErrSourceChanged
	}
	return nil
}

func validateTranscript(output transcribeResponse, language, sha string) error {
	if output.AudioSHA256 != sha || !languagePattern.MatchString(output.Language) || (language != "auto" && output.Language != language) || !utf8.ValidString(output.Text) || strings.ContainsRune(output.Text, '\x00') || len(output.Text) > maximumTextBytes || math.IsNaN(output.Start) || math.IsInf(output.Start, 0) || math.IsNaN(output.End) || math.IsInf(output.End, 0) || output.Start != 0 || output.End <= 0 || output.End > maximumDurationSeconds {
		return ErrInvalidResponse
	}
	if output.Silent {
		if output.Text != "" {
			return ErrInvalidResponse
		}
		return nil
	}
	if strings.TrimSpace(output.Text) == "" {
		return ErrInvalidResponse
	}
	return nil
}

func transcriptDocument(work archive.ExtractionWork, response transcribeResponse) archive.Document {
	provenance := archive.DerivedProvenance{Kind: archive.DocumentTranscript, MediaID: work.MediaID, ProfileID: work.Version.ProfileID, ExtractorVersion: work.Version.ExtractorVersion, PromptVersion: work.Version.PromptVersion, StartMS: 0, EndMS: int64(math.Ceil(response.End * 1000)), OffsetKind: "slice"}
	encoded, _ := canonicalJSON(struct {
		Version     archive.ExtractionVersion
		ContentHash string
		Job         archive.JobID
		Source      archive.SourceRef
		Provenance  archive.DerivedProvenance
		Text        string
	}{work.Version, work.ContentHash, work.ID, work.Source, provenance, response.Text})
	return archive.Document{ID: archive.DocumentID(digest(encoded)), Kind: archive.DocumentTranscript, Text: response.Text, Sources: []archive.SourceRef{work.Source}, Provenance: provenance}
}

func requestError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}

func responseStatus(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusTooManyRequests:
		return ErrBusy
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusConflict:
		return ErrModelChanged
	case http.StatusBadRequest:
		return media.ErrInvalidRequest
	case http.StatusRequestEntityTooLarge:
		return media.ErrLimit
	case http.StatusUnsupportedMediaType:
		return media.ErrUnsupported
	case http.StatusUnprocessableEntity:
		return media.ErrCorrupt
	default:
		return ErrUnavailable
	}
}
