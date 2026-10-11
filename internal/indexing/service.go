package indexing

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

type Service struct {
	config      ServiceConfig
	stepMu      sync.Mutex
	mu          sync.Mutex
	snapshot    Snapshot
	generation  archive.IndexGeneration
	stepCancel  context.CancelFunc
	dirtyLimit  int // stepMu owns future claims; archive retains current lease authority
	publication PublicationStore
	dirty       *archive.DirtyWork
	derived     *archive.Job
}
type BackgroundService = Service

func NewService(c ServiceConfig) (*Service, error) {
	if nilPort(c.Store) || c.Account == "" || len(c.Account) > 512 || !utf8.ValidString(c.Account) {
		return nil, ErrInvalidConfiguration
	}
	publication, ok := c.Store.(PublicationStore)
	if !ok || nilPort(publication) {
		return nil, ErrInvalidConfiguration
	}
	if c.LeaseTTL == 0 {
		c.LeaseTTL = 2 * time.Minute
	}
	if c.PollInterval == 0 {
		c.PollInterval = time.Second
	}
	if c.RetryBase == 0 {
		c.RetryBase = 5 * time.Second
	}
	if c.RetryMax == 0 {
		c.RetryMax = 5 * time.Minute
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 8
	}
	if c.LeaseTTL < time.Second || c.LeaseTTL > 15*time.Minute || c.PollInterval < 10*time.Millisecond || c.PollInterval > time.Minute || c.RetryBase < time.Second || c.RetryMax < c.RetryBase || c.RetryMax < 30*time.Second || c.RetryMax > time.Hour || c.MaxAttempts < 1 || c.MaxAttempts > 100 {
		return nil, ErrInvalidConfiguration
	}
	v := c.Version
	_, hashErr := hex.DecodeString(v.TokenizerSHA256)
	if v.ID == "" || len(v.ID) > 256 || !utf8.ValidString(v.ID) || v.EncoderProfile == "" || len(v.EncoderProfile) > 256 || v.TokenizerImplementation == "" || len(v.TokenizerImplementation) > 256 || v.PrefixVersion == "" || len(v.PrefixVersion) > 256 || len(v.TokenizerSHA256) != 64 || hashErr != nil || v.ChunkingVersion != ChunkingVersion || v.NormalizationVersion != archive.LexicalNormalizationVersion || v.Dimensions != inference.ProtocolDimension || v.MaxPassageTokens != MaxWindowTokens {
		return nil, ErrInvalidConfiguration
	}
	if nilPort(c.Tokenizer) {
		c.Tokenizer = nil
	} else {
		identity := c.Tokenizer.TokenizerIdentity()
		if identity.ProfileID != v.EncoderProfile || identity.TokenizerSHA256 != v.TokenizerSHA256 || identity.Implementation != v.TokenizerImplementation || identity.PrefixVersion != v.PrefixVersion {
			return nil, ErrInvalidConfiguration
		}
	}
	if nilPort(c.Encoder) {
		c.Encoder = nil
	}
	return &Service{config: c, snapshot: Snapshot{State: "idle", Paused: c.Paused}, dirtyLimit: MaxInputMessages, publication: publication}, nil
}
func NewBackgroundService(c ServiceConfig) (*BackgroundService, error) { return NewService(c) }
func nilPort(port any) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return value.IsNil()
	}
	return false
}
func (s *Service) SetPaused(paused bool) {
	s.mu.Lock()
	s.snapshot.Paused = paused
	cancel := s.stepCancel
	if paused {
		s.snapshot.State = "paused"
	} else if s.snapshot.State == "paused" {
		s.snapshot.State = "idle"
	}
	s.mu.Unlock()
	if paused && cancel != nil {
		cancel()
	}
}
func (s *Service) Snapshot() Snapshot { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot }

// Run starts no native runtime itself. Each bounded Step uses durable archive
// claims; failures leave jobs/dirty leases recoverable without denying sources.
func (s *Service) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	s.mu.Lock()
	if s.snapshot.Running {
		s.mu.Unlock()
		return ErrBusy
	}
	s.snapshot.Running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.snapshot.Running = false; s.mu.Unlock() }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			_, _ = s.Step(ctx)
			timer.Reset(s.config.PollInterval)
		}
	}
}

// Step performs at most one dirty page and one embedding job. CurrentSource is
// deliberately not used to upgrade an old expected source snapshot. Input loads
// and final publications are reauthorized by the single archive writer.
func (s *Service) Step(ctx context.Context) (result StepResult, err error) {
	if ctx == nil {
		return result, ErrInvalidInput
	}
	if !s.stepMu.TryLock() {
		return result, ErrBusy
	}
	defer s.stepMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.stepCancel = cancel
	s.mu.Unlock()
	defer cancel()
	result.State = "idle"
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.snapshot.State = result.State
		s.snapshot.Code = result.Code
		if err != nil {
			s.snapshot.State = "failed"
			s.snapshot.Code = boundaryError(err).Error()
		}
		s.snapshot.Published += uint64(result.Published)
		s.snapshot.Embedded += uint64(result.Embedded)
		s.stepCancel = nil
		if s.snapshot.Paused {
			s.snapshot.State = "paused"
		}
	}()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if s.Snapshot().Paused {
		result.State = "paused"
		return result, nil
	}
	if s.generation.ID == "" {
		g, e := s.config.Store.EnsureIndex(ctx, s.config.Version)
		if e != nil {
			return result, boundaryError(e)
		}
		if g.ID == "" || g.Version != s.config.Version || (g.State != "building" && g.State != "active") {
			return result, ErrInvalidConfiguration
		}
		s.generation = g
	}
	if s.config.Tokenizer == nil {
		result.State = "pending"
		result.Code = "tokenizer_unavailable"
		return result, nil
	}
	windowResult, e := s.windowStep(ctx)
	result = windowResult
	if result.State == "" {
		result.State = "pending"
	}
	if e != nil {
		return result, e
	}
	jobResult, e := s.embeddingStep(ctx)
	result.Embedded = jobResult.Embedded
	if jobResult.State != "" {
		result.State = jobResult.State
	}
	if jobResult.Code != "" {
		result.Code = jobResult.Code
	}
	if e != nil {
		return result, e
	}
	if result.Code == "" && s.dirty == nil && s.derived == nil && s.generation.State == "building" {
		if e = s.config.Store.ActivateIndex(ctx, s.generation.ID); e == nil {
			s.generation.State = "active"
			result.Activated = true
		} else if !superseded(e) {
			return result, boundaryError(e)
		}
	}
	return result, nil
}
func superseded(err error) bool {
	return errors.Is(err, archive.ErrNoWork) || errors.Is(err, archive.ErrSourceUnavailable) || errors.Is(err, archive.ErrPolicyChanged)
}

func sameWork(a, b archive.DirtyWork) bool { a.LeaseUntil = b.LeaseUntil; return a == b }
func sameJob(a, b archive.Job) bool        { a.LeaseUntil = b.LeaseUntil; return reflect.DeepEqual(a, b) }
func validProgress(p archive.PublicationProgress, cursor archive.PublicationCursor) bool {
	if !p.Accepted {
		return !p.Complete && p.Next == ""
	}
	if p.Complete {
		return p.Next == ""
	}
	return p.Next != "" && p.Next != cursor && len(p.Next) <= 256
}

func (s *Service) windowStep(ctx context.Context) (StepResult, error) {
	result := StepResult{State: "idle"}
	if s.dirty == nil {
		work, err := s.config.Store.ClaimDirty(ctx, archive.DirtyClaim{Account: s.config.Account, Generation: s.generation.ID, TTL: s.config.LeaseTTL, Limit: s.dirtyLimit})
		if errors.Is(err, archive.ErrNoWork) {
			return result, nil
		}
		if err != nil {
			return result, boundaryError(err)
		}
		if work.Account != s.config.Account || work.Generation != s.generation.ID || work.ChatID == "" || work.PolicyGeneration == 0 || work.LeaseID == "" || work.LeaseUntil.IsZero() {
			return result, ErrInvalidInput
		}
		s.dirty = &work
	}
	work := *s.dirty
	input, err := s.publication.LoadWindowBatch(ctx, work)
	if superseded(err) {
		s.dirty = nil
		return StepResult{Code: "superseded"}, nil
	}
	if err != nil {
		return result, boundaryError(err)
	}
	if !sameWork(work, input.Work) {
		return result, ErrInvalidInput
	}
	if !input.Ready && (len(input.Messages) != 0 || input.Cursor != "") {
		return result, ErrInvalidInput
	}
	until, err := s.publication.RenewDirty(ctx, work, s.config.LeaseTTL)
	if superseded(err) {
		s.dirty = nil
		return StepResult{Code: "superseded"}, nil
	}
	if err != nil {
		return result, boundaryError(err)
	}
	if !input.Ready {
		return StepResult{State: "pending", Code: "preparing"}, nil
	}
	for _, m := range input.Messages {
		if m.SentAt.Before(work.From) || !m.SentAt.Before(work.Until) {
			return result, ErrInvalidInput
		}
	}
	var documents []Document
	var progress archive.PublicationProgress
	err = s.withLease(ctx, until, func(renewCtx context.Context) (time.Time, error) {
		return s.publication.RenewDirty(renewCtx, work, s.config.LeaseTTL)
	}, func(workCtx context.Context) error {
		var err error
		documents, err = BuildWindowBatch(workCtx, input, s.config.Tokenizer)
		if err != nil {
			return err
		}
		progress, err = s.publication.PublishWindowBatch(workCtx, archive.WindowBatchPublication{Work: work, Version: s.config.Version, Cursor: input.Cursor, Documents: documents})
		return err
	})
	if err != nil {
		if superseded(err) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			s.dirty = nil
			return StepResult{Code: "superseded"}, nil
		}
		if errors.Is(err, archive.ErrLimitExceeded) {
			if s.dirtyLimit > 1 {
				s.dirtyLimit /= 2
			}
			s.dirty = nil
		}
		if errors.Is(err, ErrNotConfigured) {
			return StepResult{State: "pending", Code: "tokenizer_unavailable"}, nil
		}
		if errors.Is(err, errUnsupportedText) || errors.Is(err, ErrLimit) {
			return StepResult{State: "pending", Code: "unsupported"}, nil
		}
		return result, boundaryError(err)
	}
	if !validProgress(progress, input.Cursor) {
		return result, ErrInvalidInput
	}
	if !progress.Accepted {
		s.dirty = nil
		return StepResult{Code: "superseded"}, nil
	}
	result.State, result.Published = "working", len(documents)
	if progress.Complete {
		s.dirty = nil
	} else {
		result.Code = "publication_pending"
	}
	return result, nil
}
func boundaryError(err error) error {
	if err == nil {
		return nil
	}
	for _, fixed := range []error{context.Canceled, context.DeadlineExceeded, ErrInvalidInput, ErrInvalidTokenizer, ErrInvalidConfiguration, ErrLimit, ErrBusy, ErrNotConfigured, archive.ErrArchiveUnavailable, archive.ErrReconciliationRequired, archive.ErrPolicyChanged, archive.ErrSourceUnavailable, archive.ErrNoWork, archive.ErrLimitExceeded, archive.ErrQueueLimitExceeded, archive.ErrInvalidExtension} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return ErrStore
}
