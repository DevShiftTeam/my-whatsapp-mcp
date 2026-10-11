package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type fakeDocumentRetryApplication struct {
	*fakeApplication
	value DocumentRetrySnapshot
	ids   []archive.MessageID
}

func (a *fakeDocumentRetryApplication) RetryDocumentDownloads(_ context.Context, ids []archive.MessageID) (DocumentRetrySnapshot, error) {
	a.ids = append([]archive.MessageID(nil), ids...)
	return a.value, a.called("document_retry")
}

func TestDocumentRetryRouteRequiresOwnerAndOptionalCapability(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	owner := login(t, s)
	if request(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-message"]}`, nil, nil).Code != 401 {
		t.Fatal("document retry route did not require the owner session")
	}
	if request(s, "GET", "/api/documents/retry", "", &owner, nil).Code != 405 {
		t.Fatal("document retry accepted a read request")
	}
	if request(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-message"]}`, &owner, nil).Code != 503 {
		t.Fatal("document retry did not expose its optional owner capability")
	}
}

func TestDocumentRetryStrictBatchAndOwnerOrigin(t *testing.T) {
	app := &fakeDocumentRetryApplication{fakeApplication: &fakeApplication{}, value: DocumentRetrySnapshot{Queued: 1, Skipped: 1, Requested: 2}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	for _, headers := range []map[string]string{{"X-CSRF-Token": ""}, {"Origin": ""}, {"Origin": "https://other.invalid"}} {
		if request(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-one"]}`, &owner, headers).Code != 403 {
			t.Fatal("document retry accepted missing owner origin or capability")
		}
	}
	for _, body := range []string{`{}`, `{"message_ids":[]}`, `{"message_ids":["opaque-one","opaque-one"]}`, `{"message_ids":["https://private-canary.invalid/file"]}`, `{"message_ids":["opaque-one"],"account":"private-canary"}`, `{"message_ids":["opaque-one"],"protocol_reference":"private-canary"}`, `{"message_ids":["1","2","3","4","5","6","7","8","9"]}`, `null`, `[]`, ``} {
		if request(s, "POST", "/api/documents/retry", body, &owner, nil).Code != 400 {
			t.Fatal("document retry accepted malformed or unbounded source input")
		}
	}
	if app.total() != 0 {
		t.Fatal("denied retry reached the application")
	}
	rr := request(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-one","opaque-two"]}`, &owner, nil)
	var result DocumentRetrySnapshot
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil || result != app.value || len(app.ids) != 2 || app.ids[0] != "opaque-one" || app.ids[1] != "opaque-two" {
		t.Fatal("document retry lost exact opaque messages or bounded aggregate result")
	}
	if strings.Contains(rr.Body.String(), "opaque") {
		t.Fatal("retry response exposed source identifiers")
	}
}

func TestDocumentRetryClosedErrorsAndCounts(t *testing.T) {
	app := &fakeDocumentRetryApplication{fakeApplication: &fakeApplication{err: errors.New("PRIVATE_REFERENCE")}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	call := func() int {
		rr := request(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-one"]}`, &owner, nil)
		if strings.Contains(rr.Body.String(), "PRIVATE") {
			t.Fatal("retry leaked backend source data")
		}
		return rr.Code
	}
	if call() != 503 {
		t.Fatal("retry did not close a backend failure")
	}
	app.err = nil
	for _, value := range []DocumentRetrySnapshot{{Queued: -1}, {Skipped: -1}, {Requested: -1}, {Queued: 2}, {Skipped: 2}, {Queued: 1, Skipped: 1}, {Requested: 2}} {
		app.value = value
		if call() != 503 {
			t.Fatal("retry returned impossible batch counts")
		}
	}
}

func TestDocumentRetryRemoteCannotUseMCPBearerAsOwner(t *testing.T) {
	app := &fakeDocumentRetryApplication{fakeApplication: &fakeApplication{}, value: DocumentRetrySnapshot{Requested: 1}}
	s, err := NewRemote(remoteConfig(t), app)
	if err != nil {
		t.Fatal(err)
	}
	if remoteRequest(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-one"]}`, nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer mcp-token") }).Code != 401 {
		t.Fatal("MCP token bypassed remote owner password")
	}
	owner := remoteLogin(t, s)
	if remoteRequest(s, "POST", "/api/documents/retry", `{"message_ids":["opaque-one"]}`, &owner, nil).Code != 200 || app.count("document_retry") != 1 {
		t.Fatal("remote owner could not request exact document recovery")
	}
}
