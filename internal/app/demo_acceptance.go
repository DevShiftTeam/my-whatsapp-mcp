package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"strings"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

const demoOCRCanary = "SYNTHETIC_OCR_ACCEPTANCE"
const demoTranscriptCanary = "SYNTHETIC_TRANSCRIPT_ACCEPTANCE"
const demoDocumentCanary = "SYNTHETIC_DOCUMENT_ACCEPTANCE"

func demoAcceptanceDocument() []byte {
	return []byte("%PDF-1.7\n% SYNTHETIC_DOCUMENT_ACCEPTANCE\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n")
}

var errDemoAcceptance = errors.New("demo_acceptance_unavailable")

// This owner contains real cleanup capabilities, never public fixture counters.
// All writes are synchronous; Close follows their completion and retains any
// capability whose physical or durable cleanup has not been acknowledged.
type demoAcceptanceOwner struct {
	store     *archive.Store
	downloads []ingestion.DownloadAdmission
	originals []*media.Original
	inputs    []demoAcceptanceInput
}
type demoAcceptanceInput struct {
	lease    archive.JobLease
	original *media.Original
}

func (o *demoAcceptanceOwner) Close(ctx context.Context) error {
	for len(o.originals) != 0 {
		if err := o.originals[0].Close(); err != nil {
			return err
		}
		o.originals = o.originals[1:]
	}
	for len(o.inputs) != 0 {
		input := &o.inputs[0]
		if input.original != nil {
			if err := input.original.Close(); err != nil {
				return err
			}
			input.original = nil
		}
		if err := o.store.RetryExtraction(ctx, archive.JobRetry{Lease: input.lease, Code: "cancelled", After: time.Second}); err != nil {
			return err
		}
		o.inputs = o.inputs[1:]
	}
	for len(o.downloads) != 0 {
		if err := o.downloads[0].Close(); err != nil {
			return err
		}
		o.downloads = o.downloads[1:]
	}
	return nil
}

func demoAcceptanceVoice() []byte {
	data := make([]byte, 48)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], 40)
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 16000)
	binary.LittleEndian.PutUint32(data[28:32], 32000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:44], 4)
	return data
}

func demoAcceptanceEvent(id string, kind archive.MessageKind, size int64) archive.SourceEvent {
	mime := "audio/wav"
	if kind == archive.MessageImage {
		mime = "image/png"
	}
	if kind == archive.MessageDocument {
		mime = "application/pdf"
	}
	return archive.SourceEvent{Key: archive.SourceKey{Account: demoAccount, Chat: "demo-acceptance-media", Sender: "demo-author", Message: id},
		Kind: archive.EventOriginal, OriginalSentAt: time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), OriginalDateKnown: true,
		BodyKind: kind, Media: []archive.MediaReference{{Kind: string(kind), ProtocolReference: []byte("wa-indexer/synthetic-demo-only/v1/" + id), MimeType: mime, DeclaredBytes: size, Availability: "unavailable"}}}
}

func (o *demoAcceptanceOwner) admit(ctx context.Context, a *Application, generation archive.Generation, event archive.SourceEvent, data []byte, pending bool) (archive.MediaID, error) {
	result, err := a.store.ApplySourceEvent(ctx, event, generation)
	if err != nil || len(result.MediaIDs) != 1 {
		return "", errDemoAcceptance
	}
	if !pending {
		scope, err := a.scope(ctx)
		if err != nil {
			return "", err
		}
		original, err := a.resources.media.OpenOriginal(ctx, scope, result.MediaIDs[0])
		if original != nil {
			o.originals = append(o.originals, original)
			metadata := original.Metadata()
			expected := sha256.Sum256(data)
			if metadata.SHA256 != hex.EncodeToString(expected[:]) || metadata.Bytes != int64(len(data)) {
				return "", errDemoAcceptance
			}
			if err := original.Close(); err != nil {
				return "", err
			}
			o.originals = o.originals[:len(o.originals)-1]
			return result.MediaIDs[0], nil
		}
		if err != nil && !errors.Is(err, media.ErrUnavailable) && !errors.Is(err, archive.ErrNoWork) && !errors.Is(err, archive.ErrSourceUnavailable) {
			return "", err
		}
	}
	request := ingestion.MediaDownloadRequest{Key: event.Key, Version: result.Version, MediaID: result.MediaIDs[0], Kind: event.BodyKind,
		DeclaredBytes: int64(len(data)), ProtocolSHA256: sha256.Sum256(data)}
	if pending {
		request.DeclaredBytes = 0
		request.CiphertextMaximum = 26214426
	}
	download, err := a.resources.media.MediaAdmitter(ctx, request)
	if download != nil {
		o.downloads = append(o.downloads, download)
	}
	if err != nil {
		return "", err
	}
	if pending {
		return result.MediaIDs[0], nil
	}
	if n, err := download.File().Write(data); err != nil || n != len(data) {
		return "", errDemoAcceptance
	}
	if err := download.Complete(ctx, ingestion.VerifiedMedia{Key: event.Key, Version: result.Version, Kind: event.BodyKind,
		SHA256: request.ProtocolSHA256, Bytes: int64(len(data)), Reader: bytes.NewReader(data)}); err != nil {
		return "", err
	}
	if err := download.Close(); err != nil {
		return "", err
	}
	o.downloads = o.downloads[:len(o.downloads)-1]
	return result.MediaIDs[0], nil
}

func (o *demoAcceptanceOwner) publish(ctx context.Context, a *Application, id archive.MediaID, data []byte, kind archive.JobKind, text string) error {
	profile := "demo-synthetic-ocr-v1"
	documentKind := archive.DocumentOCR
	if kind == archive.JobTranscription {
		profile = "demo-synthetic-transcript-v1"
		documentKind = archive.DocumentTranscript
	}
	fingerprint := sha256.Sum256([]byte("wa-indexer/synthetic-demo-only/v1/" + profile))
	version := archive.ExtractionVersion{ID: profile, Kind: kind, ProfileID: profile, ProfileIdentity: "synthetic-demo-v1/" + profile,
		RuntimeSHA256: hex.EncodeToString(fingerprint[:]), ExtractorVersion: "synthetic-demo-v1", PromptVersion: "none-v1", Language: "auto"}
	work, err := a.store.ClaimExtraction(ctx, archive.ExtractionClaim{Account: demoAccount, Version: version, TTL: time.Minute})
	if errors.Is(err, archive.ErrNoWork) || err == nil && work.ID == "" {
		scope, scopeErr := a.scope(ctx)
		if scopeErr != nil {
			return scopeErr
		}
		page, searchErr := a.resources.retrieval.Search(ctx, scope, archive.SearchRequest{Query: strings.Fields(text)[0], Limit: 10})
		if searchErr != nil {
			return searchErr
		}
		for _, hit := range page.Hits {
			for _, derived := range hit.Derived {
				if derived.Provenance.MediaID == id && derived.Provenance.ProfileID == profile && derived.Provenance.ExtractorVersion == version.ExtractorVersion {
					return nil
				}
			}
		}
		return errDemoAcceptance
	}
	if err != nil {
		return err
	}
	o.inputs = append(o.inputs, demoAcceptanceInput{lease: work.Lease})
	index := len(o.inputs) - 1
	current, err := a.store.LoadExtractionInput(ctx, work.Lease)
	sha := sha256.Sum256(data)
	if err != nil || current.MediaID != id || current.ArtifactID == "" || current.OriginalSHA256 != hex.EncodeToString(sha[:]) || current.Source != work.Source {
		return errDemoAcceptance
	}
	scope, err := a.scope(ctx)
	if err != nil {
		return err
	}
	original, err := a.resources.media.OpenOriginal(ctx, scope, id)
	if original != nil {
		o.inputs[index].original = original
	}
	if err != nil {
		return err
	}
	metadata := original.Metadata()
	if metadata.ArtifactID != current.ArtifactID || metadata.SHA256 != current.OriginalSHA256 || metadata.Source != current.Source {
		return errDemoAcceptance
	}
	if err := original.Close(); err != nil {
		return err
	}
	o.inputs[index].original = nil
	document := archive.Document{ID: archive.DocumentID("demo-acceptance-" + string(work.ID)), Kind: documentKind, Text: text,
		Sources: []archive.SourceRef{current.Source}, Provenance: archive.DerivedProvenance{Kind: documentKind, MediaID: id,
			ProfileID: profile, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}}
	committed, err := a.store.CommitExtraction(ctx, archive.ExtractionResult{Lease: work.Lease, ContentHash: current.ContentHash,
		Outcome: archive.ExtractionText, Documents: []archive.Document{document}})
	if err != nil || !committed {
		return errDemoAcceptance
	}
	o.inputs = o.inputs[:index]
	return nil
}

func prepareDemoAcceptance(ctx context.Context, a *Application, generation archive.Generation) (*demoAcceptanceOwner, error) {
	if !a.demo || a.store == nil || a.resources == nil || a.resources.media == nil || a.resources.recoveryPending.Load() {
		return nil, errDemoAcceptance
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account != demoAccount {
		return nil, errDemoAcceptance
	}
	owner := &demoAcceptanceOwner{store: a.store}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		return owner, err
	}
	imageData, voiceData := encoded.Bytes(), demoAcceptanceVoice()
	for _, fixture := range []struct {
		id   string
		data []byte
		kind archive.MessageKind
		job  archive.JobKind
		text string
	}{
		{"acceptance-ocr", imageData, archive.MessageImage, archive.JobOCR, demoOCRCanary + " · טקסט סינתטי לבדיקת ממשק"},
		{"acceptance-transcript", voiceData, archive.MessageVoice, archive.JobTranscription, demoTranscriptCanary + " · תמלול סינתטי לבדיקת ממשק"},
	} {
		id, err := owner.admit(ctx, a, generation, demoAcceptanceEvent(fixture.id, fixture.kind, int64(len(fixture.data))), fixture.data, false)
		if err != nil {
			return owner, err
		}
		if err := owner.publish(ctx, a, id, fixture.data, fixture.job, fixture.text); err != nil {
			return owner, err
		}
	}
	document := demoAcceptanceDocument()
	event := demoAcceptanceEvent("acceptance-document", archive.MessageDocument, int64(len(document)))
	event.Caption = demoDocumentCanary
	event.Media[0].Filename = "synthetic-acceptance.pdf"
	if _, err := owner.admit(ctx, a, generation, event, document, false); err != nil {
		return owner, err
	}
	budget, err := a.store.GetOriginalBudget(ctx)
	if err != nil {
		return owner, err
	}
	// Explicit isolated acceptance preparation permits these bounded reservations
	// before lowering the authoritative limit; no reservation is fabricated.
	if budget.MaximumBytes != archive.DefaultOriginalBudgetBytes {
		budget, err = a.store.SetOriginalBudget(ctx, archive.OriginalBudgetUpdate{MaximumBytes: archive.DefaultOriginalBudgetBytes, ExpectedRevision: budget.Revision})
		if err != nil {
			return owner, err
		}
	}
	for _, id := range []string{"acceptance-budget-1", "acceptance-budget-2", "acceptance-budget-3"} {
		if _, err := owner.admit(ctx, a, generation, demoAcceptanceEvent(id, archive.MessageVoice, 0), voiceData, true); err != nil {
			return owner, err
		}
	}
	_, err = a.store.SetOriginalBudget(ctx, archive.OriginalBudgetUpdate{MaximumBytes: archive.MinimumOriginalBudgetBytes, ExpectedRevision: budget.Revision})
	return owner, err
}
