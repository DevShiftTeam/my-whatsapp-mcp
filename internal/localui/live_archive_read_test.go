package localui

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
	"strings"
	"testing"
)

type liveOwnerReadApp struct {
	*statsTestApplication
	value                   any
	invalid                 bool
	reads, checks, releases int
}

func (a *liveOwnerReadApp) ReadLiveArchive(_ context.Context, _ ArchiveReadRequest) (ArchiveReadResult, error) {
	a.reads++
	return ArchiveReadResult{Value: a.value, Check: func(context.Context) error {
		a.checks++
		if a.invalid {
			return archive.ErrSourceRevisionConflict
		}
		return nil
	}, Release: func() { a.releases++ }}, nil
}

type liveGapMarshal struct{ hook func() }

func (v liveGapMarshal) MarshalJSON() ([]byte, error) {
	v.hook()
	return []byte(`{"Snapshot":{"UnknownHistoryGap":false},"canary":"OWNER_STALE_COVERAGE_CANARY"}`), nil
}
func TestUnknownHistoryOwnerLiveFinalDelivery(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/search", `{"query":"canary"}`},
		{http.MethodPost, "/api/context", `{"message_id":"opaque_message"}`},
		{http.MethodGet, "/api/chats", ""},
		{http.MethodGet, "/api/status", ""},
		{http.MethodPost, "/api/stats", `{}`},
	} {
		for _, gap := range []bool{false, true} {
			name := route.path + "/stable"
			if gap {
				name = route.path + "/gap_during_marshal"
			}
			t.Run(name, func(t *testing.T) {
				a := &liveOwnerReadApp{statsTestApplication: &statsTestApplication{fakeApplication: &fakeApplication{}}}
				a.value = liveGapMarshal{hook: func() { a.invalid = gap }}
				s, err := New(Config{Origin: testOrigin}, a)
				if err != nil {
					t.Fatal(err)
				}
				owner := login(t, s)
				rr := request(s, route.method, route.path, route.body, &owner, nil)
				if a.reads != 1 || a.releases != 1 {
					t.Fatalf("live response did not retain guarded source lifetime: reads=%d releases=%d", a.reads, a.releases)
				}
				if gap {
					if rr.Code < 400 || strings.Contains(rr.Body.String(), "OWNER_STALE_COVERAGE_CANARY") {
						t.Fatal("gap changed during serialization but stale metadata escaped")
					}
				} else if rr.Code != 200 || a.checks != 2 || !strings.Contains(rr.Body.String(), "OWNER_STALE_COVERAGE_CANARY") || strings.Contains(rr.Body.String(), "archive_view") {
					t.Fatalf("live guarded response changed source semantics: status=%d checks=%d", rr.Code, a.checks)
				}
			})
		}
	}
}
