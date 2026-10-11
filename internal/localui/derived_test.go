package localui

import (
	"encoding/json"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestDerivedContextPreservesOriginalLogicalTextAndSuppliedProvenance(t *testing.T) {
	body := "הודעה מקורית 123 https://example.invalid/ <img src=x onerror=alert(1)>"
	extracted := "תמלול בעברית 456 שלום"
	app := &fakeApplication{context: archive.ContextPage{Messages: []archive.MessageView{{
		ID: "message_1", Body: body, Kind: "voice",
		Derived: []archive.DerivedExcerpt{{DocumentID: "document_1", Text: extracted, Provenance: archive.DerivedProvenance{
			Kind: archive.DocumentTranscript, StartMS: 3000, EndMS: 7000, OffsetKind: "slice",
		}}},
	}}}}
	s := newTestServer(t, app)
	owner := login(t, s)
	rr := request(s, "POST", "/api/context", "{\"message_id\":\"message_1\"}", &owner, nil)
	if rr.Code != 200 {
		t.Fatal("derived context was rejected")
	}
	var page archive.ContextPage
	if json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Messages) != 1 {
		t.Fatal("context response invalid")
	}
	message := page.Messages[0]
	if message.Body != body || len(message.Derived) != 1 || message.Derived[0].Text != extracted {
		t.Fatal("logical source or derived text changed")
	}
	provenance := message.Derived[0].Provenance
	if provenance.OffsetKind != "slice" || provenance.StartMS != 3000 || provenance.EndMS != 7000 {
		t.Fatal("transcript timing was fabricated or lost")
	}
}
