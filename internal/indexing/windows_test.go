package indexing

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/inference"
)

type testTokenizer struct {
	broken bool
	cheap  bool
}
type failingTokenizer struct {
	testTokenizer
	err error
}

func (t failingTokenizer) CountPassage(context.Context, string) (int, error) { return 0, t.err }

type boundedRequestTokenizer struct {
	testTokenizer
	largest *int
}

func (t boundedRequestTokenizer) CountPassage(ctx context.Context, text string) (int, error) {
	if len(text) > *t.largest {
		*t.largest = len(text)
	}
	if len(text) > inference.MaxTextBytes {
		return 0, errors.New("oversized tokenizer IPC")
	}
	return t.testTokenizer.CountPassage(ctx, text)
}

func (testTokenizer) TokenizerIdentity() inference.TokenizerIdentity {
	return inference.TokenizerIdentity{ProfileID: "synthetic", TokenizerSHA256: strings.Repeat("a", 64), Implementation: "synthetic-v1", PrefixVersion: "passage-special-v1"}
}
func (t testTokenizer) CountPassage(ctx context.Context, text string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if t.cheap {
		return 3, nil
	}
	return 2 + utf8.RuneCountInString(text) + 20*strings.Count(text, "\n"), nil
}
func (t testTokenizer) SplitPassage(ctx context.Context, text string, max int) ([]inference.TextSpan, error) {
	if t.broken {
		return []inference.TextSpan{{Start: 1, End: len(text), Text: text[1:], Tokens: 3}}, nil
	}
	var out []inference.TextSpan
	start, end := 0, 0
	for end < len(text) {
		_, size := utf8.DecodeRuneInString(text[end:])
		next := end + size
		count, err := t.CountPassage(ctx, text[start:next])
		if err != nil {
			return nil, err
		}
		if count > max {
			if end == start {
				return nil, inference.ErrLimit
			}
			n, _ := t.CountPassage(ctx, text[start:end])
			out = append(out, inference.TextSpan{Start: start, End: end, Text: text[start:end], Tokens: n})
			start = end
			continue
		}
		end = next
	}
	if end > start {
		n, _ := t.CountPassage(ctx, text[start:end])
		out = append(out, inference.TextSpan{Start: start, End: end, Text: text[start:end], Tokens: n})
	}
	return out, nil
}
func windowMessage(id, chat, body string, at time.Time) archive.WindowMessage {
	return archive.WindowMessage{Source: archive.SourceRef{Account: "account", Version: archive.SourceVersion{MessageID: archive.MessageID(id), Revision: 1, PolicyGeneration: 1}, ContentHash: strings.Repeat("b", 64)}, ChatID: archive.ChatID(chat), SentAt: at, Body: body}
}

func batchInput(messages []archive.WindowMessage) archive.WindowBatchInput {
	return archive.WindowBatchInput{Ready: true, Cursor: "server-cursor", Work: archive.DirtyWork{ID: "work", Account: "account", ChatID: "one", PolicyGeneration: 1, Generation: "generation"}, Start: archive.WindowPosition{Field: "body"}, Messages: messages}
}

// Test-only writer simulation advances from accepted exact spans. Production
// never sends this position; it always reloads the archive-issued cursor/input.
func nextTestPosition(input archive.WindowBatchInput, documents []archive.Document) archive.WindowPosition {
	last := documents[len(documents)-1].Spans
	span := last[len(last)-1]
	ordinal := 0
	for input.Messages[ordinal].Source != span.Source {
		ordinal++
	}
	return archive.WindowPosition{SourceOrdinal: ordinal, Field: span.Field, StartByte: span.EndByte}
}

func TestWindowBatchesCoverEntire256KiBSourceAndCaption(t *testing.T) {
	message := windowMessage("large", "one", strings.Repeat("x", MaxSourceFieldBytes), time.Now().UTC())
	message.Caption = strings.Repeat("שלום🙂", 90)
	input := batchInput([]archive.WindowMessage{message})
	var all []archive.Document
	for batch := 0; batch < 8; batch++ {
		documents, err := BuildWindowBatch(context.Background(), input, testTokenizer{})
		if err != nil {
			t.Fatal(err)
		}
		if len(documents) == 0 {
			break
		}
		if len(documents) > MaxDocuments {
			t.Fatal("unbounded document batch")
		}
		for _, document := range documents {
			if !strings.HasPrefix(string(document.ID), "generation-") {
				t.Fatal("window ID is not generation namespaced")
			}
		}
		all = append(all, documents...)
		input.Start = nextTestPosition(input, documents)
		if batch == 7 {
			t.Fatal("large source did not reach EOF in bounded batches")
		}
	}
	if len(all) <= MaxDocuments {
		t.Fatal("fixture did not exercise multiple publications")
	}
	checkSourceCoverage(t, []archive.WindowMessage{message}, all)
}

func TestWindowBatchesGenerationIDsAndAuthoritativeStart(t *testing.T) {
	message := windowMessage("a", "one", "שלום🙂body", time.Now().UTC())
	message.Caption = " caption \n"
	input := batchInput([]archive.WindowMessage{message})
	input.Start = archive.WindowPosition{Field: "caption", StartByte: 1}
	a, err := BuildWindowBatch(context.Background(), input, testTokenizer{})
	if err != nil || len(a) != 1 || a[0].Text != message.Caption[1:] || a[0].Spans[0].Field != "caption" || a[0].Spans[0].StartByte != 1 {
		t.Fatalf("authoritative prefix: %#v %v", a, err)
	}
	b, err := BuildWindowBatch(context.Background(), input, testTokenizer{})
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("same batch is not deterministic")
	}
	input.Work.Generation = "replacement"
	c, err := BuildWindowBatch(context.Background(), input, testTokenizer{})
	if err != nil || c[0].ID == a[0].ID || !strings.HasPrefix(string(c[0].ID), "replacement-") {
		t.Fatal("window identity crossed generations")
	}
	input.Start = archive.WindowPosition{Field: "body", StartByte: 1}
	if _, err = BuildWindowBatch(context.Background(), input, testTokenizer{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("mid-rune start accepted")
	}
}

func TestWindowBatchAdaptsFragmentCapWithoutDroppingText(t *testing.T) {
	text := strings.Repeat("a", 300)
	input := batchInput([]archive.WindowMessage{windowMessage("dense", "one", text, time.Now().UTC())})
	tokenizer := fragmentTokenizer{split: func(text string) []inference.TextSpan {
		spans := make([]inference.TextSpan, len(text))
		for i := range text {
			spans[i] = inference.TextSpan{Start: i, End: i + 1, Text: text[i : i+1], Tokens: 3}
		}
		return spans
	}}
	documents, err := BuildWindowBatch(context.Background(), input, tokenizer)
	if err != nil {
		t.Fatal(err)
	}
	checkSourceCoverage(t, input.Messages, documents)
}
func checkSourceCoverage(t *testing.T, messages []archive.WindowMessage, docs []archive.Document) {
	t.Helper()
	type fieldKey struct {
		id    archive.MessageID
		field string
	}
	covered := map[fieldKey]string{}
	for _, d := range docs {
		var exact []string
		seen := map[archive.SourceRef]bool{}
		for _, span := range d.Spans {
			var field string
			found := false
			for _, m := range messages {
				if m.Source == span.Source {
					found = true
					if span.Field == "body" {
						field = m.Body
					} else if span.Field == "caption" {
						field = m.Caption
					}
					break
				}
			}
			if !found || span.StartByte < 0 || span.EndByte <= span.StartByte || span.EndByte > len(field) || !utf8.ValidString(field[:span.StartByte]) || !utf8.ValidString(field[:span.EndByte]) {
				t.Fatalf("invalid source span: %#v", span)
			}
			text := field[span.StartByte:span.EndByte]
			exact = append(exact, text)
			key := fieldKey{span.Source.Version.MessageID, span.Field}
			covered[key] += text
			seen[span.Source] = true
		}
		if strings.Join(exact, "\n") != d.Text {
			t.Fatalf("text invented or changed: %q", d.Text)
		}
		if len(seen) != len(d.Sources) {
			t.Fatal("source union mismatch")
		}
		for _, s := range d.Sources {
			if !seen[s] {
				t.Fatal("foreign source")
			}
		}
		count, _ := (testTokenizer{}).CountPassage(context.Background(), d.Text)
		if d.PassageTokens != count || count > 256 {
			t.Fatalf("incorrect complete passage count: %d %d", d.PassageTokens, count)
		}
	}
	for _, m := range messages {
		if covered[fieldKey{m.Source.Version.MessageID, "body"}] != m.Body || covered[fieldKey{m.Source.Version.MessageID, "caption"}] != m.Caption {
			t.Fatalf("lost source text %s", m.Source.Version.MessageID)
		}
	}
}
func TestWindowsStayInOneChat(t *testing.T) {
	at := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	messages := []archive.WindowMessage{windowMessage("c", "second", "שלום", at), windowMessage("b", "first", "world", at.Add(time.Minute)), windowMessage("a", "first", "hello", at)}
	docs, err := BuildWindows(context.Background(), messages, testTokenizer{})
	if err != nil || len(docs) != 2 {
		t.Fatalf("windows %d: %v", len(docs), err)
	}
	if docs[0].Text != "hello\nworld" || docs[1].Text != "שלום" {
		t.Fatalf("unexpected logical order: %#v", docs)
	}
	checkSourceCoverage(t, messages, docs)
	other, _ := BuildWindows(context.Background(), []archive.WindowMessage{messages[2], messages[0], messages[1]}, testTokenizer{})
	for i := range docs {
		if docs[i].ID != other[i].ID {
			t.Fatal("non-deterministic identity")
		}
	}
}
func TestWindowsKeepAccountsSeparateEvenForSameChatLabel(t *testing.T) {
	at := time.Now()
	a := windowMessage("a", "same-chat", "original a", at)
	b := windowMessage("b", "same-chat", "original b", at)
	b.Source.Account = "another-account"
	docs, err := BuildWindows(context.Background(), []WindowMessage{b, a}, testTokenizer{})
	if err != nil || len(docs) != 2 {
		t.Fatalf("cross-account window: %d %v", len(docs), err)
	}
}
func TestWindowsGapMessageAndWholePassageBounds(t *testing.T) {
	at := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	t.Run("five minute gap", func(t *testing.T) {
		m := []archive.WindowMessage{windowMessage("a", "one", "a", at), windowMessage("b", "one", "b", at.Add(5*time.Minute)), windowMessage("c", "one", "c", at.Add(10*time.Minute+time.Nanosecond))}
		d, e := BuildWindows(context.Background(), m, testTokenizer{})
		if e != nil || len(d) != 2 || len(d[0].Sources) != 2 {
			t.Fatalf("gap bounds: %#v %v", d, e)
		}
	})
	t.Run("empty text cannot bridge gap", func(t *testing.T) {
		m := []archive.WindowMessage{windowMessage("a", "one", "a", at), windowMessage("empty", "one", "", at.Add(5*time.Minute)), windowMessage("b", "one", "b", at.Add(10*time.Minute))}
		d, e := BuildWindows(context.Background(), m, testTokenizer{})
		if e != nil || len(d) != 2 {
			t.Fatalf("empty bridge: %d %v", len(d), e)
		}
	})
	t.Run("twenty messages no padding", func(t *testing.T) {
		var m []archive.WindowMessage
		for i := range 21 {
			m = append(m, windowMessage(string(rune('a'+i)), "one", "x", at.Add(time.Duration(i)*time.Second)))
		}
		d, e := BuildWindows(context.Background(), m, testTokenizer{cheap: true})
		if e != nil || len(d) != 2 || len(d[0].Sources) != 20 || len(d[1].Sources) != 1 {
			t.Fatalf("message bound: %#v %v", d, e)
		}
	})
	t.Run("joined count includes newline and prefix", func(t *testing.T) {
		m := []archive.WindowMessage{windowMessage("a", "one", strings.Repeat("a", 120), at), windowMessage("b", "one", strings.Repeat("b", 120), at)}
		d, e := BuildWindows(context.Background(), m, testTokenizer{})
		if e != nil || len(d) != 2 {
			t.Fatalf("whole joined passage must split: %d %v", len(d), e)
		}
		checkSourceCoverage(t, m, d)
	})
}
func TestLongMessageSplitsKeepSource(t *testing.T) {
	m := windowMessage("long", "one", strings.Repeat("עברית🙂", 100), time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC))
	m.Caption = "caption original"
	d, err := BuildWindows(context.Background(), []archive.WindowMessage{m}, testTokenizer{})
	if err != nil || len(d) < 2 {
		t.Fatalf("long split: %d %v", len(d), err)
	}
	checkSourceCoverage(t, []archive.WindowMessage{m}, d)
}
func TestWindowValidationAndHardLimits(t *testing.T) {
	m := windowMessage("a", "one", "original", time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC))
	for _, bad := range []archive.WindowMessage{func() archive.WindowMessage { x := m; x.Body = string([]byte{0xff}); return x }(), func() archive.WindowMessage { x := m; x.Source.Version.Deleted = true; return x }(), func() archive.WindowMessage { x := m; x.Source.ContentHash = "snippet-hash"; return x }()} {
		if _, e := BuildWindows(context.Background(), []archive.WindowMessage{bad}, testTokenizer{}); !errors.Is(e, ErrInvalidInput) {
			t.Fatalf("invalid input: %v", e)
		}
	}
	if _, e := BuildWindows(context.Background(), []archive.WindowMessage{m, m}, testTokenizer{}); !errors.Is(e, ErrInvalidInput) {
		t.Fatalf("duplicate: %v", e)
	}
	m.Body = strings.Repeat("a", MaxPublicationBytes+1)
	if _, e := BuildWindows(context.Background(), []archive.WindowMessage{m}, testTokenizer{cheap: true}); !errors.Is(e, ErrLimit) {
		t.Fatalf("byte bound: %v", e)
	}
	m.Body = strings.Repeat("a", 260*254)
	if _, e := BuildWindows(context.Background(), []archive.WindowMessage{m}, testTokenizer{}); !errors.Is(e, ErrLimit) {
		t.Fatalf("document bound: %v", e)
	}
	m.Body = strings.Repeat("א", 400)
	if _, e := BuildWindows(context.Background(), []archive.WindowMessage{m}, testTokenizer{broken: true}); !errors.Is(e, ErrInvalidTokenizer) {
		t.Fatalf("broken split: %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := BuildWindows(ctx, nil, testTokenizer{}); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
}
func TestTokenizerErrorsAreBodyFreeAndMissingPortsAreInert(t *testing.T) {
	m := windowMessage("a", "one", "private synthetic", time.Now())
	for _, test := range []struct {
		backend error
		want    error
	}{{errors.New("PRIVATE BODY /secret/path"), ErrInvalidTokenizer}, {inference.ErrUnavailable, ErrNotConfigured}, {inference.ErrLimit, ErrLimit}, {context.DeadlineExceeded, context.DeadlineExceeded}, {context.Canceled, context.Canceled}} {
		if _, e := BuildWindows(context.Background(), []WindowMessage{m}, failingTokenizer{err: test.backend}); !errors.Is(e, test.want) {
			t.Fatalf("tokenizer boundary: %v", e)
		}
	}
	var missing *testTokenizer
	if _, e := BuildWindows(context.Background(), []WindowMessage{m}, missing); !errors.Is(e, ErrNotConfigured) {
		t.Fatalf("typed nil tokenizer: %v", e)
	}
}
func TestLongOriginalNeverExceedsTokenizerIPC(t *testing.T) {
	m := windowMessage("a", "one", strings.Repeat("שלום🙂", 5000), time.Now())
	largest := 0
	docs, e := BuildWindows(context.Background(), []WindowMessage{m}, boundedRequestTokenizer{testTokenizer: testTokenizer{cheap: true}, largest: &largest})
	if e != nil {
		t.Fatal(e)
	}
	if largest > inference.MaxTextBytes || len(docs) < 2 {
		t.Fatalf("unbounded IPC %d", largest)
	}
	var original strings.Builder
	for _, d := range docs {
		if len(d.Text) > inference.MaxTextBytes || len(d.Sources) != 1 || d.Sources[0] != m.Source {
			t.Fatal("long original lost source/bounds")
		}
		for _, span := range d.Spans {
			original.WriteString(m.Body[span.StartByte:span.EndByte])
		}
	}
	if original.String() != m.Body {
		t.Fatal("long original truncated or changed")
	}
}
