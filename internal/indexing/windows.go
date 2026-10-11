package indexing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

type fieldPart struct {
	span SourceSpan
	text string
}
type sourceIdentity struct {
	account string
	id      archive.MessageID
}

// A valid tokenizer partition can exceed one atomic archive publication. Keep
// this distinct from malformed tokenizer output so staged work remains pending.
var errFragmentLimit = errors.Join(ErrLimit, errors.New("indexing_fragment_limit"))

const errUnsupportedText Error = "indexing_unsupported_text"

// BuildWindowBatch emits a bounded contiguous prefix from the writer's pinned
// position. The caller publishes only these completed documents and reloads;
// only the archive can acknowledge full source-field EOF.
func BuildWindowBatch(ctx context.Context, input WindowBatchInput, t inference.PassageTokenizer) ([]Document, error) {
	if ctx == nil || !input.Ready || input.Cursor == "" || input.Work.Generation == "" || len(input.Messages) > MaxInputMessages || input.Start.SourceOrdinal < 0 || input.Start.SourceOrdinal > len(input.Messages) || (input.Start.Field != "body" && input.Start.Field != "caption") || input.Start.StartByte < 0 {
		return nil, ErrInvalidInput
	}
	if nilPort(t) {
		return nil, ErrNotConfigured
	}
	seen := map[sourceIdentity]bool{}
	bytes := 0
	for i, m := range input.Messages {
		key := sourceIdentity{m.Source.Account, m.Source.Version.MessageID}
		if !validSource(m.Source) || m.Source.Account != input.Work.Account || m.ChatID != input.Work.ChatID || m.Source.Version.PolicyGeneration != input.Work.PolicyGeneration || m.SentAt.IsZero() || seen[key] || len(m.Body) > MaxSourceFieldBytes || len(m.Caption) > MaxSourceFieldBytes || !utf8.ValidString(m.Body) || !utf8.ValidString(m.Caption) {
			return nil, ErrInvalidInput
		}
		if i > 0 {
			previous := input.Messages[i-1]
			if m.SentAt.Before(previous.SentAt) || m.SentAt.Equal(previous.SentAt) && m.Source.Version.MessageID < previous.Source.Version.MessageID {
				return nil, ErrInvalidInput
			}
		}
		seen[key] = true
		bytes += len(m.Body) + len(m.Caption)
		if bytes > MaxPublicationBytes {
			return nil, ErrLimit
		}
	}
	if input.Start.SourceOrdinal == len(input.Messages) {
		if input.Start.StartByte != 0 {
			return nil, ErrInvalidInput
		}
		return nil, ctx.Err()
	}
	startMessage := input.Messages[input.Start.SourceOrdinal]
	startText := startMessage.Body
	if input.Start.Field == "caption" {
		startText = startMessage.Caption
	}
	if input.Start.StartByte > len(startText) || !utf8.ValidString(startText[:input.Start.StartByte]) {
		return nil, ErrInvalidInput
	}
	var documents []Document
	current := Document{Kind: archive.DocumentWindow, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}}
	members := map[sourceIdentity]bool{}
	outputBytes, sources, spans := 0, 0, 0
	flush := func() (bool, error) {
		if current.Text == "" {
			return false, nil
		}
		if len(documents) == MaxDocuments || outputBytes+len(current.Text) > MaxPublicationBytes || sources+len(current.Sources) > MaxSources || spans+len(current.Spans) > MaxSources {
			return true, nil
		}
		id, err := windowGenerationID(current, t.TokenizerIdentity(), input.Work.Generation)
		if err != nil {
			return false, err
		}
		current.ID = id
		documents = append(documents, current)
		outputBytes += len(current.Text)
		sources += len(current.Sources)
		spans += len(current.Spans)
		current = Document{Kind: archive.DocumentWindow, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}}
		members = map[sourceIdentity]bool{}
		return len(documents) == MaxDocuments, nil
	}
	var previous WindowMessage
	for ordinal := input.Start.SourceOrdinal; ordinal < len(input.Messages); ordinal++ {
		m := input.Messages[ordinal]
		if m.Body == "" && m.Caption == "" {
			continue
		}
		key := sourceIdentity{m.Source.Account, m.Source.Version.MessageID}
		if current.Text != "" && (m.SentAt.Sub(previous.SentAt) > MaxWindowGap || !members[key] && len(members) >= MaxWindowMessages) {
			full, err := flush()
			if err != nil {
				return nil, err
			}
			if full {
				return documents, nil
			}
		}
		for fieldOrdinal, field := range []struct{ name, text string }{{"body", m.Body}, {"caption", m.Caption}} {
			base := 0
			if ordinal == input.Start.SourceOrdinal {
				if input.Start.Field == "caption" && fieldOrdinal == 0 {
					continue
				}
				if field.name == input.Start.Field {
					base = input.Start.StartByte
				}
			}
			for base < len(field.text) {
				parts, err := splitPrefix(ctx, t, field.text[base:], MaxWindowTokens)
				if err != nil {
					return nil, err
				}
				for _, part := range parts {
					span := SourceSpan{Source: m.Source, Field: field.name, StartByte: base + part.Start, EndByte: base + part.End}
					merge := len(current.Spans) > 0 && current.Spans[len(current.Spans)-1].Source == span.Source && current.Spans[len(current.Spans)-1].Field == span.Field && current.Spans[len(current.Spans)-1].EndByte == span.StartByte
					separator := ""
					if current.Text != "" && !merge {
						separator = "\n"
					}
					candidate := current.Text + separator + part.Text
					count := MaxWindowTokens + 1
					if len(candidate) <= inference.MaxTextBytes {
						count, err = t.CountPassage(ctx, candidate)
						if err != nil {
							return nil, tokenizerError(err)
						}
						if count < 1 {
							return nil, ErrInvalidTokenizer
						}
					}
					if count > MaxWindowTokens || len(candidate) > inference.MaxTextBytes {
						full, err := flush()
						if err != nil {
							return nil, err
						}
						if full {
							return documents, nil
						}
						candidate, merge, count = part.Text, false, part.Tokens
					}
					current.Text, current.PassageTokens = candidate, count
					if merge {
						current.Spans[len(current.Spans)-1].EndByte = span.EndByte
					} else {
						current.Spans = append(current.Spans, span)
					}
					if !members[key] {
						members[key] = true
						current.Sources = append(current.Sources, m.Source)
					}
				}
				base += parts[len(parts)-1].End
			}
		}
		previous = m
	}
	_, err := flush()
	return documents, err
}

// A native split can hit its finite fragment envelope on valid dense text.
// Halve only that input prefix at an original UTF-8 boundary, then recount and
// retry. Every accepted byte remains available at the writer's next position.
func splitPrefix(ctx context.Context, t inference.PassageTokenizer, text string, maxTokens int) ([]inference.TextSpan, error) {
	end := len(text)
	if end > inference.MaxTextBytes {
		end = inference.MaxTextBytes
	}
	for end < len(text) && end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == 0 {
		return nil, ErrInvalidInput
	}
	for {
		spans, err := splitText(ctx, t, text[:end], maxTokens)
		if !errors.Is(err, errFragmentLimit) {
			return spans, err
		}
		next := end / 2
		for next > 0 && !utf8.RuneStart(text[next]) {
			next--
		}
		if next == 0 {
			_, next = utf8.DecodeRuneInString(text)
		}
		if next >= end {
			return nil, ErrLimit
		}
		end = next
	}
}

// BuildWindows preserves original field bytes and their exact UTF-8 spans.
// Tokens are measured on each complete passage, including prefix and specials.
// It never uses sums of per-message counts or pads a short conversation.
func BuildWindows(ctx context.Context, messages []WindowMessage, tokenizer inference.PassageTokenizer) ([]Document, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if nilPort(tokenizer) {
		return nil, ErrNotConfigured
	}
	if len(messages) > MaxInputMessages {
		return nil, ErrLimit
	}
	ordered := append([]WindowMessage(nil), messages...)
	seen := make(map[sourceIdentity]bool, len(ordered))
	inputBytes := 0
	for _, m := range ordered {
		key := sourceIdentity{m.Source.Account, m.Source.Version.MessageID}
		if !validSource(m.Source) || m.ChatID == "" || len(m.ChatID) > 256 || m.SentAt.IsZero() || !utf8.ValidString(m.Body) || !utf8.ValidString(m.Caption) || seen[key] {
			return nil, ErrInvalidInput
		}
		seen[key] = true
		inputBytes += len(m.Body) + len(m.Caption)
		if inputBytes > MaxPublicationBytes {
			return nil, ErrLimit
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.ChatID != b.ChatID {
			return a.ChatID < b.ChatID
		}
		if a.Source.Account != b.Source.Account {
			return a.Source.Account < b.Source.Account
		}
		if !a.SentAt.Equal(b.SentAt) {
			return a.SentAt.Before(b.SentAt)
		}
		if a.Source.Account != b.Source.Account {
			return a.Source.Account < b.Source.Account
		}
		return a.Source.Version.MessageID < b.Source.Version.MessageID
	})
	var docs []Document
	current := Document{Kind: archive.DocumentWindow, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}}
	currentSources := map[sourceIdentity]bool{}
	var previous WindowMessage
	outputBytes, sourceCount := 0, 0
	flush := func() error {
		if current.Text == "" {
			return nil
		}
		outputBytes += len(current.Text)
		sourceCount += len(current.Sources)
		if len(docs) >= MaxDocuments || outputBytes > MaxPublicationBytes || sourceCount > MaxSources {
			return ErrLimit
		}
		id, e := windowID(current, tokenizer.TokenizerIdentity())
		if e != nil {
			return e
		}
		current.ID = id
		docs = append(docs, current)
		current = Document{Kind: archive.DocumentWindow, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}}
		currentSources = map[sourceIdentity]bool{}
		return nil
	}
	for _, m := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m.Body == "" && m.Caption == "" {
			continue
		}
		key := sourceIdentity{m.Source.Account, m.Source.Version.MessageID}
		if current.Text != "" && (previous.ChatID != m.ChatID || previous.Source.Account != m.Source.Account || m.SentAt.Sub(previous.SentAt) > MaxWindowGap || !currentSources[key] && len(currentSources) >= MaxWindowMessages) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		for _, field := range []struct{ name, text string }{{"body", m.Body}, {"caption", m.Caption}} {
			parts, e := splitField(ctx, tokenizer, m.Source, field.name, field.text)
			if e != nil {
				return nil, e
			}
			for _, part := range parts {
				// Adjacent fragments of one original field are merged rather than
				// inserting a synthetic newline inside that field.
				merge := len(current.Spans) > 0 && current.Spans[len(current.Spans)-1].Source == part.span.Source && current.Spans[len(current.Spans)-1].Field == part.span.Field && current.Spans[len(current.Spans)-1].EndByte == part.span.StartByte
				candidate := current.Text
				separator := ""
				if candidate != "" && !merge {
					separator = "\n"
				}
				candidate += separator + part.text
				count := MaxWindowTokens + 1
				if len(candidate) <= inference.MaxTextBytes {
					count, e = tokenizer.CountPassage(ctx, candidate)
					if e != nil {
						return nil, tokenizerError(e)
					}
					if count < 1 {
						return nil, ErrInvalidTokenizer
					}
				}
				if count > MaxWindowTokens || len(candidate) > inference.MaxTextBytes {
					if err := flush(); err != nil {
						return nil, err
					}
					candidate = part.text
					merge = false
					count, e = tokenizer.CountPassage(ctx, candidate)
					if e != nil {
						return nil, tokenizerError(e)
					}
					if count < 1 || count > MaxWindowTokens {
						return nil, ErrInvalidTokenizer
					}
				}
				current.Text = candidate
				current.PassageTokens = count
				if merge {
					current.Spans[len(current.Spans)-1].EndByte = part.span.EndByte
				} else {
					current.Spans = append(current.Spans, part.span)
				}
				if !currentSources[key] {
					currentSources[key] = true
					current.Sources = append(current.Sources, m.Source)
				}
			}
		}
		previous = m
	}
	if e := flush(); e != nil {
		return nil, e
	}
	return docs, nil
}

func validSource(s SourceRef) bool {
	_, err := hex.DecodeString(s.ContentHash)
	return s.Account != "" && len(s.Account) <= 512 && utf8.ValidString(s.Account) && s.Version.MessageID != "" && len(s.Version.MessageID) <= 128 && utf8.ValidString(string(s.Version.MessageID)) && s.Version.Revision > 0 && s.Version.PolicyGeneration > 0 && !s.Version.Deleted && !s.Version.Excluded && len(s.ContentHash) == 64 && err == nil
}

// Bound every tokenizer IPC request even for a source much larger than one
// passage. Precut boundaries are original UTF-8 boundaries and retain all bytes.
func splitField(ctx context.Context, t inference.PassageTokenizer, s SourceRef, field, text string) ([]fieldPart, error) {
	spans, err := splitText(ctx, t, text, MaxWindowTokens)
	if err != nil {
		return nil, err
	}
	result := make([]fieldPart, len(spans))
	for i, span := range spans {
		result[i] = fieldPart{span: SourceSpan{Source: s, StartByte: span.Start, EndByte: span.End, Field: field}, text: span.Text}
	}
	return result, nil
}

// splitText partitions only original UTF-8 bytes. Each tokenizer call is bounded
// by the wire envelope; every returned fragment is recounted as a full passage.
func splitText(ctx context.Context, t inference.PassageTokenizer, text string, maxTokens int) ([]inference.TextSpan, error) {
	if maxTokens < 1 || maxTokens > MaxWindowTokens || !utf8.ValidString(text) {
		return nil, ErrInvalidInput
	}
	if strings.ContainsRune(text, 0) {
		return nil, errUnsupportedText
	}
	var result []inference.TextSpan
	for base := 0; base < len(text); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := base + inference.MaxTextBytes
		if end > len(text) {
			end = len(text)
		}
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		piece := text[base:end]
		count, err := t.CountPassage(ctx, piece)
		if err != nil {
			return nil, tokenizerError(err)
		}
		if count < 1 {
			return nil, ErrInvalidTokenizer
		}
		spans := []inference.TextSpan{{Start: 0, End: len(piece), Text: piece, Tokens: count}}
		if count > maxTokens {
			spans, err = t.SplitPassage(ctx, piece, maxTokens)
			if err != nil {
				if errors.Is(err, inference.ErrLimit) {
					return nil, errFragmentLimit
				}
				return nil, tokenizerError(err)
			}
		}
		offset := 0
		if len(spans) == 0 {
			return nil, ErrInvalidTokenizer
		}
		if len(spans) > MaxDocuments {
			return nil, errFragmentLimit
		}
		for _, span := range spans {
			if span.Start != offset || span.End <= span.Start || span.End > len(piece) || !utf8.ValidString(piece[:span.Start]) || !utf8.ValidString(piece[:span.End]) || span.Text != piece[span.Start:span.End] {
				return nil, ErrInvalidTokenizer
			}
			exact, e := t.CountPassage(ctx, span.Text)
			if e != nil {
				return nil, tokenizerError(e)
			}
			if exact < 1 || exact > maxTokens || span.Tokens != exact {
				return nil, ErrInvalidTokenizer
			}
			result = append(result, inference.TextSpan{Start: base + span.Start, End: base + span.End, Text: span.Text, Tokens: exact})
			if len(result) > MaxDocuments {
				return nil, errFragmentLimit
			}
			offset = span.End
		}
		if offset != len(piece) {
			return nil, ErrInvalidTokenizer
		}
		base = end
	}
	return result, nil
}

func tokenizerError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, inference.ErrUnavailable), errors.Is(err, inference.ErrClosed):
		return ErrNotConfigured
	case errors.Is(err, inference.ErrLimit):
		return ErrLimit
	default:
		return ErrInvalidTokenizer
	}
}
func windowID(d Document, identity inference.TokenizerIdentity) (archive.DocumentID, error) {
	value := struct {
		Schema, Chunking string
		Tokenizer        inference.TokenizerIdentity
		Sources          []SourceRef
		Spans            []SourceSpan
		Text             string
	}{"wa-indexer-window-id-v1", ChunkingVersion, identity, d.Sources, d.Spans, d.Text}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", ErrInvalidInput
	}
	sum := sha256.Sum256(raw)
	return archive.DocumentID("win_" + hex.EncodeToString(sum[:])), nil
}

func windowGenerationID(d Document, identity inference.TokenizerIdentity, g archive.IndexGenerationID) (archive.DocumentID, error) {
	value := struct {
		Schema, Chunking string
		Generation       archive.IndexGenerationID
		Tokenizer        inference.TokenizerIdentity
		Sources          []SourceRef
		Spans            []SourceSpan
		Text             string
	}{"wa-indexer-window-batch-id-v1", ChunkingVersion, g, identity, d.Sources, d.Spans, d.Text}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", ErrInvalidInput
	}
	sum := sha256.Sum256(raw)
	id := archive.DocumentID(string(g) + "-" + hex.EncodeToString(sum[:]))
	if len(id) > 256 {
		return "", ErrInvalidInput
	}
	return id, nil
}
