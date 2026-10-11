//go:build sqlite_fts5

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

const voiceFixtureToken = "synthetic-voice-application-token-0123456789"
const voiceFixtureText = "VOICE_BATCH_CANARY תמלול הודעה בעברית"

type voiceHTTPFixture struct {
	server       *httptest.Server
	ready        atomic.Bool
	failNext     atomic.Bool
	rejectStatus atomic.Int64
	calls        atomic.Int64
	identity     string
}

func newVoiceHTTPFixture(t *testing.T, ready bool) *voiceHTTPFixture {
	t.Helper()
	runtime := map[string]any{"faster_whisper": "1.2.1", "ctranslate2": "4.8.2", "av": "18.0.0", "onnxruntime": "1.31.0", "tokenizers": "0.22.1", "compute_type": "int8", "cpu_threads": 4, "num_workers": 1, "language": "he", "vad_filter": true}
	recipe := map[string]any{"protocol_version": 1, "backend": "faster-whisper", "model": "ivrit-ai/whisper-large-v3-turbo-ct2", "revision": strings.Repeat("a", 40), "model_sha256": strings.Repeat("b", 64), "runtime": runtime,
		"model_files": []map[string]string{{"path": "config.json", "sha256": strings.Repeat("c", 64)}, {"path": "model.bin", "sha256": strings.Repeat("b", 64)}, {"path": "tokenizer.json", "sha256": strings.Repeat("d", 64)}}}
	encoded, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal("synthetic recipe encode failed")
	}
	sha := sha256.Sum256(encoded)
	f := &voiceHTTPFixture{identity: hex.EncodeToString(sha[:])}
	f.ready.Store(ready)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+voiceFixtureToken {
			t.Error("voice batch request was not authenticated")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/info" && r.Method == http.MethodGet {
			if !f.ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"recipe": recipe, "identity": f.identity})
			return
		}
		if r.URL.Path != "/v1/transcribe" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls.Add(1)
		data, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			t.Error("fixture upload failed")
			w.WriteHeader(500)
			return
		}
		sha := sha256.Sum256(data)
		if hex.EncodeToString(sha[:]) != r.Header.Get("X-Audio-SHA256") || r.Header.Get("X-Transcription-Identity") != f.identity {
			t.Error("verified audio upload lost identity")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.failNext.Swap(false) {
			f.ready.Store(false)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if status := f.rejectStatus.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"identity": f.identity, "audio_sha256": hex.EncodeToString(sha[:]), "language": "he", "text": voiceFixtureText, "start": 0, "end": 1, "silent": false})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func voiceTestApplication(t *testing.T, f *voiceHTTPFixture) (*Application, Config) {
	t.Helper()
	cfg := Config{DataDir: shortNativeRoot(t), Demo: true, Transcription: &transcription.Options{URL: f.server.URL, Token: voiceFixtureToken, Language: "he"}}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal("optional batch service blocked app construction")
	}
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if _, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.StartPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, cfg
}

func addVoiceTestOriginal(t *testing.T, a *Application, id string) {
	t.Helper()
	policy, err := a.store.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner := &demoAcceptanceOwner{store: a.store}
	t.Cleanup(func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	data := demoAcceptanceVoice()
	if _, err := owner.admit(context.Background(), a, policy.Generation, demoAcceptanceEvent(id, archive.MessageVoice, int64(len(data))), data, false); err != nil {
		t.Fatal("synthetic original could not be retained")
	}
}

func waitVoiceCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("voice pipeline did not reach the expected durable state")
}

func voiceSearch(t *testing.T, a *Application) archive.SearchPage {
	t.Helper()
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "VOICE_BATCH_CANARY", Limit: 10})
	if err != nil {
		t.Fatal("voice search failed")
	}
	return page
}

func TestVoiceBatchWorksWithoutNativeModelsAndSurvivesNativeOffAndRestart(t *testing.T) {
	f := newVoiceHTTPFixture(t, false)
	a, cfg := voiceTestApplication(t, f)
	if page, err := a.Search(context.Background(), archive.SearchRequest{Query: "Atlas", Limit: 10}); err != nil || len(page.Hits) == 0 {
		t.Fatal("unavailable voice service blocked text import or keyword search")
	}
	addVoiceTestOriginal(t, a, "independent-voice-one")
	f.ready.Store(true)
	waitVoiceCondition(t, func() bool { return len(voiceSearch(t, a).Hits) == 1 })
	state, err := a.LocalAIState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetLocalAI(context.Background(), localui.LocalAIUpdate{ExpectedRevision: state.Revision, Choice: localui.LocalAIChoice{Mode: "off"}}); err != nil {
		t.Fatal(err)
	}
	waitVoiceCondition(t, func() bool {
		state, err := a.LocalAIState(context.Background())
		return err == nil && state.State == "off"
	})
	addVoiceTestOriginal(t, a, "independent-voice-two")
	waitVoiceCondition(t, func() bool { return len(voiceSearch(t, a).Hits) == 2 })
	page := voiceSearch(t, a)
	for _, hit := range page.Hits {
		if len(hit.Derived) != 1 || hit.Derived[0].Text != voiceFixtureText || hit.Derived[0].Provenance.Kind != archive.DocumentTranscript {
			t.Fatal("batch transcript lost generated provenance")
		}
	}
	status, err := a.Status(context.Background())
	if err != nil || status.Semantic.State != "not_configured" || status.Media.Complete != 2 || status.Media.Code != "transcription_ready" {
		t.Fatalf("voice status: semantic=%s media=%s/%s pending=%d complete=%d failed=%d err=%v", status.Semantic.State, status.Media.State, status.Media.Code, status.Media.Pending, status.Media.Complete, status.Media.Failed, err)
	}
	calls := f.calls.Load()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal("saved voice archive did not reopen")
	}
	t.Cleanup(func() {
		if err := restarted.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if len(voiceSearch(t, restarted).Hits) != 2 {
		t.Fatal("committed transcripts did not survive restart")
	}
	waitVoiceCondition(t, func() bool {
		status, err := restarted.Status(context.Background())
		return err == nil && status.Media.Complete == 2 && status.Media.Code == "transcription_ready"
	})
	if f.calls.Load() != calls {
		t.Fatal("completed originals were transcribed again after restart")
	}
}

func TestVoiceFailureAfterReadyReleasesDurablePinsAndPreservesText(t *testing.T) {
	f := newVoiceHTTPFixture(t, true)
	f.failNext.Store(true)
	a, _ := voiceTestApplication(t, f)
	addVoiceTestOriginal(t, a, "transient-voice-failure")
	waitVoiceCondition(t, func() bool { return f.calls.Load() == 1 })
	waitVoiceCondition(t, func() bool {
		pinned, err := a.store.ListPinnedExtractions(context.Background(), archive.ExtractionRecoveryRequest{Account: demoAccount, Limit: 64})
		return err == nil && len(pinned.Items) == 0
	})
	status, err := a.Status(context.Background())
	if err != nil || status.Media.State != "unavailable" || status.Media.Code != "transcription_unavailable" || status.Media.Pending != 1 || status.Media.Failed != 0 {
		t.Fatal("voice outage was not a pending retry")
	}
	if page, err := a.Search(context.Background(), archive.SearchRequest{Query: "Atlas", Limit: 10}); err != nil || len(page.Hits) == 0 {
		t.Fatal("voice failure blocked text reads")
	}
	// A fresh extraction version can claim the same retained original. A stuck
	// physical pin would block this claim even though the HTTP request ended.
	a.resources.voice.mu.Lock()
	version := a.resources.voice.backend.Version()
	a.resources.voice.mu.Unlock()
	version.ID += "-synthetic-new-version"
	work, err := a.store.ClaimExtraction(context.Background(), archive.ExtractionClaim{Account: demoAccount, Version: version, TTL: time.Minute})
	if err != nil || work.ID == "" {
		t.Fatal("failed request left its original pinned")
	}
	if err := a.store.RetryExtraction(context.Background(), archive.JobRetry{Lease: work.Lease, Code: "cancelled", After: time.Second}); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceConfigurationCannotEnableDuplicateNativeTranscription(t *testing.T) {
	opts := &transcription.Options{URL: "http://127.0.0.1:6971", Token: voiceFixtureToken, Language: "he"}
	cfg := Config{Transcription: opts, Media: &MediaConfig{Experimental: true, ExtractionVersions: []archive.ExtractionVersion{{Kind: archive.JobTranscription}}}}
	if !errors.Is(validateTranscriptionConfig(cfg), errTranscriptionConfig) {
		t.Fatal("two voice backends could run concurrently")
	}
	copied := copyApplicationConfig(Config{Transcription: opts})
	opts.Token = "changed"
	if copied.Transcription.Token != voiceFixtureToken {
		t.Fatal("caller mutated immutable transcription configuration")
	}
}

func TestVoiceMCPReportsConfiguredScopedProgressAndGeneratedTranscript(t *testing.T) {
	f := newVoiceHTTPFixture(t, true)
	a, _ := voiceTestApplication(t, f)
	addVoiceTestOriginal(t, a, "scoped-mcp-voice")
	waitVoiceCondition(t, func() bool { return len(voiceSearch(t, a).Hits) == 1 })
	voiceChat := voiceSearch(t, a).Hits[0].ChatID
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	a.mcpConfig.Remote = &RemoteMCPConfig{Listen: listen, PublicURL: "https://mcp.example.com/mcp", Server: mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"mcp.example.com"}, Origins: []string{"https://mcp.example.com"}}}
	if err := a.startMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	grant, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "synthetic voice scope", Mode: "remote", Chats: []archive.ChatID{voiceChat}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: grant.ID, Revision: grant.Revision})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) string {
		r := httptest.NewRecorder()
		a.RemoteMCPHandler().ServeHTTP(r, borrowedRemoteMCPRequest(secret.Token, body))
		if r.Code != http.StatusOK {
			t.Fatal("scoped MCP request failed")
		}
		return r.Body.String()
	}
	call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"synthetic-voice-test","version":"1"}}}`)
	body := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"VOICE_BATCH_CANARY","limit":10}}}`)
	if !strings.Contains(body, voiceFixtureText) || !strings.Contains(body, "generated_voice_transcript") {
		t.Fatal("MCP omitted generated voice transcript")
	}
	body = call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_index_status","arguments":{}}}`)
	var envelope struct {
		Result struct {
			StructuredContent struct {
				Media struct {
					State    string `json:"state"`
					Complete int64  `json:"complete"`
					Code     string `json:"code"`
				} `json:"media"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil || envelope.Result.StructuredContent.Media.State == "not_configured" || envelope.Result.StructuredContent.Media.Complete != 1 {
		t.Fatal("MCP reported voice processing as unconfigured or unscoped")
	}
}
