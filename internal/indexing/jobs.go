package indexing

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

func (s *Service) embeddingStep(ctx context.Context) (StepResult, error) {
	var job archive.Job
	var err error
	if s.derived != nil {
		job = *s.derived
	} else {
		for _, kind := range []archive.JobKind{archive.JobTextEmbedding, archive.JobDerivedEmbedding} {
			job, err = s.config.Store.ClaimJob(ctx, archive.JobClaim{Account: s.config.Account, Kind: kind, TTL: s.config.LeaseTTL})
			if err == nil {
				break
			}
			if !errors.Is(err, archive.ErrNoWork) {
				return StepResult{}, boundaryError(err)
			}
		}
		if errors.Is(err, archive.ErrNoWork) {
			return StepResult{}, nil
		}
	}
	if !validClaim(job, s.config.Account) {
		s.derived = nil
		return s.retry(ctx, job, "invalid_input", true)
	}
	if job.Generation != s.generation.ID || job.Version != s.config.Version {
		s.derived = nil
		return s.retry(ctx, job, "model_unavailable", false)
	}
	var documents []archive.Document
	var batch archive.DerivedBatchInput
	if job.Kind == archive.JobDerivedEmbedding {
		batch, err = s.publication.LoadDerivedBatch(ctx, job.Lease)
		if superseded(err) {
			s.derived = nil
			return StepResult{Code: "superseded"}, nil
		}
		if err != nil {
			return StepResult{}, boundaryError(err)
		}
		if !sameJob(batch.Job, job) || validateDerivedInput(batch, s.config.Account) != nil {
			s.derived = nil
			return s.retry(ctx, job, "invalid_input", true)
		}
		digest, digestErr := archive.CanonicalOriginWorkDigest(job.Generation, job.Version, batch.Origin)
		if digestErr != nil || digest != job.ContentHash {
			s.derived = nil
			return s.retry(ctx, job, "invalid_input", true)
		}
		s.derived = &job
	} else {
		input, loadErr := s.config.Store.LoadJobInput(ctx, job.Lease)
		if superseded(loadErr) {
			return StepResult{Code: "superseded"}, nil
		}
		if loadErr != nil {
			return StepResult{}, boundaryError(loadErr)
		}
		if !sameJob(input.Job, job) || validateJobInput(input, s.config.Account) != nil {
			return s.retry(ctx, job, "invalid_input", true)
		}
		digest, digestErr := archive.CanonicalWorkDigest(job.Generation, job.Version, job.Kind, job.MediaID, job.Sources, input.Documents)
		if digestErr != nil || digest != job.ContentHash {
			return s.retry(ctx, job, "invalid_input", true)
		}
		documents = input.Documents
	}
	if s.config.Encoder == nil {
		s.derived = nil
		return s.retry(ctx, job, "model_unavailable", false)
	}
	until, err := s.config.Store.RenewJob(ctx, job.Lease, s.config.LeaseTTL)
	if superseded(err) {
		s.derived = nil
		return StepResult{Code: "superseded"}, nil
	}
	if err != nil {
		return StepResult{}, boundaryError(err)
	}
	if job.Kind == archive.JobDerivedEmbedding && !batch.Ready {
		return StepResult{State: "pending", Code: "preparing"}, nil
	}
	var embeddings []archive.Embedding
	err = s.withLease(ctx, until, func(renewCtx context.Context) (time.Time, error) {
		return s.config.Store.RenewJob(renewCtx, job.Lease, s.config.LeaseTTL)
	}, func(workCtx context.Context) error {
		var err error
		if job.Kind == archive.JobDerivedEmbedding {
			documents, err = derivedBatchDocuments(workCtx, s.config.Tokenizer, batch)
			if err != nil {
				return err
			}
		}
		embeddings, err = s.encodeDocuments(workCtx, job, documents)
		return err
	})
	if superseded(err) {
		s.derived = nil
		return StepResult{Code: "superseded"}, nil
	}
	if err != nil {
		s.derived = nil
		code, terminal := embeddingFailure(err)
		return s.retry(ctx, job, code, terminal)
	}
	result := archive.JobResult{Lease: job.Lease, Generation: job.Generation, Version: job.Version, Sources: job.Sources, ContentHash: job.ContentHash, Embeddings: embeddings}
	if job.Kind == archive.JobDerivedEmbedding {
		result.Documents = documents
		progress, commitErr := s.publication.CommitDerivedBatch(ctx, archive.DerivedBatchPublication{Result: result, Cursor: batch.Cursor})
		if commitErr != nil {
			return StepResult{}, boundaryError(commitErr)
		}
		if !validProgress(progress, batch.Cursor) {
			return StepResult{}, ErrInvalidInput
		}
		if !progress.Accepted {
			s.derived = nil
			return StepResult{Code: "superseded"}, nil
		}
		if progress.Complete {
			s.derived = nil
		} else {
			return StepResult{State: "working", Code: "publication_pending", Embedded: len(embeddings)}, nil
		}
	} else {
		committed, commitErr := s.config.Store.CommitJob(ctx, result)
		if commitErr != nil {
			return StepResult{}, boundaryError(commitErr)
		}
		if !committed {
			return StepResult{Code: "superseded"}, nil
		}
	}
	return StepResult{State: "working", Embedded: len(embeddings)}, nil
}

func validClaim(job archive.Job, account string) bool {
	if (job.Kind != archive.JobTextEmbedding && job.Kind != archive.JobDerivedEmbedding) || job.ID == "" || job.Lease.JobID != job.ID || job.Lease.LeaseID == "" || job.Attempts < 1 || len(job.Sources) == 0 || len(job.Sources) > MaxSources {
		return false
	}
	seen := map[SourceRef]bool{}
	for _, ref := range job.Sources {
		if ref.Account != account || !validSource(ref) || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	return true
}
func validateDerivedInput(input archive.DerivedBatchInput, account string) error {
	job, origin := input.Job, input.Origin
	if !validClaim(job, account) || job.Kind != archive.JobDerivedEmbedding || len(job.Documents) != 1 || job.Documents[0] != origin.ID || origin.Bytes < 1 || origin.Bytes > 64<<10 || len(origin.Sources) != len(job.Sources) {
		return ErrInvalidInput
	}
	members := map[SourceRef]bool{}
	for _, ref := range job.Sources {
		members[ref] = true
	}
	for _, ref := range origin.Sources {
		if !members[ref] {
			return ErrInvalidInput
		}
		delete(members, ref)
	}
	if len(members) != 0 {
		return ErrInvalidInput
	}
	if !input.Ready {
		if input.Cursor != "" || input.Text != "" || input.StartByte != 0 || input.EndByte != 0 {
			return ErrInvalidInput
		}
		return nil
	}
	if input.Cursor == "" || len(input.Cursor) > 256 || input.StartByte < 0 || input.EndByte <= input.StartByte || input.EndByte > origin.Bytes || len(input.Text) != input.EndByte-input.StartByte || len(input.Text) > inference.MaxTextBytes || !utf8.ValidString(input.Text) {
		return ErrInvalidInput
	}
	return nil
}
func derivedBatchDocuments(ctx context.Context, t inference.PassageTokenizer, input archive.DerivedBatchInput) ([]archive.Document, error) {
	text := input.Text
	var spans []inference.TextSpan
	for {
		var err error
		spans, err = splitPrefix(ctx, t, text, input.Job.Version.MaxPassageTokens)
		if err != nil {
			return nil, err
		}
		if len(spans)*len(input.Origin.Sources) <= MaxSources {
			break
		}
		next := len(text) / 2
		for next > 0 && !utf8.RuneStart(text[next]) {
			next--
		}
		if next == 0 {
			_, next = utf8.DecodeRuneInString(text)
		}
		if next >= len(text) {
			return nil, ErrLimit
		}
		text = text[:next]
	}
	documents := make([]archive.Document, len(spans))
	for i, span := range spans {
		id := archive.DocumentID(fmt.Sprintf("%s-%d-%d", input.Job.ID, input.StartByte+span.Start, input.StartByte+span.End))
		if len(id) > 256 {
			return nil, ErrInvalidInput
		}
		documents[i] = archive.Document{ID: id, Kind: input.Origin.Provenance.Kind, Text: span.Text, Sources: append([]archive.SourceRef(nil), input.Origin.Sources...), Provenance: input.Origin.Provenance, PassageTokens: span.Tokens}
	}
	return documents, nil
}
func (s *Service) encodeDocuments(ctx context.Context, job archive.Job, documents []archive.Document) ([]archive.Embedding, error) {
	var embeddings []archive.Embedding
	for start := 0; start < len(documents); start += inference.MaxBatchItems {
		end := start + inference.MaxBatchItems
		if end > len(documents) {
			end = len(documents)
		}
		texts := make([]string, end-start)
		for i, d := range documents[start:end] {
			count, err := s.config.Tokenizer.CountPassage(ctx, d.Text)
			if err != nil {
				return nil, err
			}
			if count < 1 || count > job.Version.MaxPassageTokens || count != d.PassageTokens {
				return nil, ErrInvalidInput
			}
			texts[i] = d.Text
		}
		vectors, err := s.config.Encoder.EncodePassages(ctx, texts)
		if err != nil {
			return nil, err
		}
		if err = validateVectors(vectors, len(texts), job.Version.Dimensions); err != nil {
			return nil, err
		}
		for i, vector := range vectors {
			embeddings = append(embeddings, archive.Embedding{DocumentID: documents[start+i].ID, Values: vector})
		}
	}
	return embeddings, nil
}

// withLease retains bounded independent renewals until the synchronous native
// calls return. Supervisor alone owns process/model/compute cancellation/reaping.
func (s *Service) withLease(ctx context.Context, until time.Time, renew func(context.Context) (time.Time, error), work func(context.Context) error) error {
	workCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	stop, renewed, done := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.config.LeaseTTL / 3)
		defer ticker.Stop()
		workDone := workCtx.Done()
		var cleanupTimer *time.Timer
		var cleanupDeadline <-chan time.Time
		defer func() {
			if cleanupTimer != nil {
				cleanupTimer.Stop()
			}
		}()
		for {
			select {
			case <-stop:
				return
			case <-workDone:
				workDone = nil
				cleanupTimer = time.NewTimer(inference.MaxRequestDuration)
				cleanupDeadline = cleanupTimer.C
			case <-cleanupDeadline:
				return
			case <-ticker.C:
				deadline := s.config.LeaseTTL / 3
				if deadline > time.Second {
					deadline = time.Second
				}
				renewCtx, finish := context.WithTimeout(context.Background(), deadline)
				_, err := renew(renewCtx)
				finish()
				if err != nil {
					select {
					case renewed <- err:
					default:
					}
					cancel()
				}
			}
		}
	}()
	err := work(workCtx)
	close(stop)
	<-done
	select {
	case renewalErr := <-renewed:
		return renewalErr
	default:
	}
	if err == nil {
		err = workCtx.Err()
	}
	return err
}

func validateJobInput(input archive.JobInput, account string) error {
	job := input.Job
	if (job.Kind != archive.JobTextEmbedding && job.Kind != archive.JobDerivedEmbedding) || job.ID == "" || job.Lease.JobID != job.ID || job.Lease.LeaseID == "" || job.Attempts < 1 || len(job.Sources) == 0 || len(job.Sources) > MaxSources || len(input.Documents) == 0 || len(input.Documents) > MaxDocuments || len(job.Documents) != len(input.Documents) {
		return ErrInvalidInput
	}
	claimed := map[SourceRef]bool{}
	for _, source := range job.Sources {
		if source.Account != account || !validSource(source) || claimed[source] {
			return ErrInvalidInput
		}
		claimed[source] = true
	}
	union := map[SourceRef]bool{}
	ids := map[archive.DocumentID]bool{}
	bytes, sources := 0, 0
	for i, document := range input.Documents {
		if document.ID == "" || document.ID != job.Documents[i] || ids[document.ID] || document.Text == "" || len(document.Text) > inference.MaxTextBytes || !utf8.ValidString(document.Text) || len(document.Sources) == 0 || len(document.Sources) > MaxSources || len(document.Spans) > MaxSources || document.PassageTokens < 0 || document.PassageTokens > MaxWindowTokens {
			return ErrInvalidInput
		}
		ids[document.ID] = true
		switch document.Kind {
		case archive.DocumentWindow, archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript:
		default:
			return ErrInvalidInput
		}
		if job.Kind == archive.JobTextEmbedding && (document.Kind != archive.DocumentWindow || document.PassageTokens == 0) || job.Kind == archive.JobDerivedEmbedding && document.Kind == archive.DocumentWindow {
			return ErrInvalidInput
		}
		if document.PassageTokens == 0 && (job.Kind != archive.JobDerivedEmbedding || len(input.Documents) != 1 || len(document.Spans) != 0) {
			return ErrInvalidInput
		}
		bytes += len(document.Text)
		sources += len(document.Sources)
		if bytes > MaxPublicationBytes || sources > MaxSources {
			return ErrLimit
		}
		members := map[SourceRef]bool{}
		for _, source := range document.Sources {
			if !claimed[source] || members[source] {
				return ErrInvalidInput
			}
			members[source] = true
			union[source] = true
		}
		for _, span := range document.Spans {
			if !members[span.Source] || span.StartByte < 0 || span.EndByte <= span.StartByte || (span.Field != "body" && span.Field != "caption") {
				return ErrInvalidInput
			}
		}
	}
	if len(union) != len(claimed) {
		return ErrInvalidInput
	}
	return nil
}
func validateVectors(vectors [][]float32, count, dimensions int) error {
	if len(vectors) != count {
		return inference.ErrProtocol
	}
	for _, vector := range vectors {
		if len(vector) != dimensions {
			return inference.ErrProtocol
		}
		norm := 0.
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return inference.ErrProtocol
			}
			norm += float64(value) * float64(value)
		}
		if math.Abs(norm-1) > 0.001 {
			return inference.ErrProtocol
		}
	}
	return nil
}
func embeddingFailure(err error) (string, bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", false
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", false
	case errors.Is(err, inference.ErrUnavailable), errors.Is(err, inference.ErrClosed), errors.Is(err, ErrNotConfigured):
		return "model_unavailable", false
	case errors.Is(err, errFragmentLimit), errors.Is(err, errUnsupportedText):
		return "unsupported", false
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidTokenizer), errors.Is(err, inference.ErrLimit), errors.Is(err, ErrLimit):
		return "invalid_input", true
	case errors.Is(err, inference.ErrProtocol):
		return "invalid_result", true
	default:
		return "worker_failed", false
	}
}
func (s *Service) retry(ctx context.Context, job archive.Job, code string, terminal bool) (StepResult, error) {
	if code != "model_unavailable" && code != "tokenizer_unavailable" && code != "unsupported" && job.Attempts >= s.config.MaxAttempts {
		terminal = true
	}
	after := retryDelay(job.ID, job.Attempts, s.config.RetryBase, s.config.RetryMax)
	if (code == "model_unavailable" || code == "tokenizer_unavailable") && after < 30*time.Second {
		after = 30 * time.Second
	}
	// Cancellation does not abandon the durable retry transition. This short,
	// fixed cleanup context admits no source reads and never prints private errors.
	retryCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		retryCtx, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
	}
	err := s.config.Store.RetryJob(retryCtx, archive.JobRetry{Lease: job.Lease, Code: code, After: after, Terminal: terminal})
	if superseded(err) {
		return StepResult{Code: "superseded"}, nil
	}
	if err != nil {
		return StepResult{}, boundaryError(err)
	}
	result := StepResult{State: "pending", Code: code}
	if terminal {
		result.State = "failed"
	}
	return result, nil
}
func retryDelay(id archive.JobID, attempt int, base, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt && delay < maximum; i++ {
		if delay > maximum/2 {
			delay = maximum
		} else {
			delay *= 2
		}
	}
	hash := sha256.Sum256([]byte(string(id)))
	jitter := int64(binary.BigEndian.Uint16(hash[:2])) + int64(attempt)*7919
	// Stable per-job/attempt jitter avoids a synchronized restart retry herd.
	delay = delay*3/4 + time.Duration(jitter%1001)*delay/2000
	if delay > maximum {
		delay = maximum
	}
	return delay
}
