package transcription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBusyReadinessKeepsFrozenRecipeAndRecovers(t *testing.T) {
	var busy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("readiness request was not authenticated")
		}
		w.Header().Set("Content-Type", "application/json")
		if busy.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(testInfo(t, testRecipe()))
	}))
	defer server.Close()
	backend, err := New(context.Background(), Options{URL: server.URL, Token: testToken, Language: "he"})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	frozen := backend.Version()
	busy.Store(true)
	if err := backend.Ready(context.Background()); err == nil || err.Error() != "transcription busy" || errors.Is(err, ErrUnavailable) {
		t.Fatalf("busy service reported an outage: %v", err)
	}
	busy.Store(false)
	if err := backend.Ready(context.Background()); err != nil || backend.Version() != frozen {
		t.Fatalf("busy probe changed the extraction recipe: %v", err)
	}
}
