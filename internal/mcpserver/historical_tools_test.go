package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Distinct guards make a historical response impossible to bless with the live
// checker. These readers contain only synthetic response text, never source data.
type sourceTestReader struct {
	mu            sync.Mutex
	generation    archive.Generation
	body          string
	reads, checks int
	failCheckAt   int
	onCheck       func(int)
}

func (r *sourceTestReader) read() archive.ResponseGuard {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return archive.ResponseGuard{PolicyGeneration: r.generation}
}
func (r *sourceTestReader) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads, r.checks
}
func (r *sourceTestReader) SearchGuarded(_ context.Context, _ archive.ReadScope, _ archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	return archive.SearchPage{Hits: []archive.SearchHit{{MessageID: "opaque-message", ChatID: "opaque-chat", Snippet: r.body}}}, r.read(), nil
}
func (r *sourceTestReader) ContextGuarded(_ context.Context, _ archive.ReadScope, req archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	return archive.ContextPage{Messages: []archive.MessageView{{ID: req.MessageID, ChatID: "opaque-chat", Body: r.body}}}, r.read(), nil
}
func (r *sourceTestReader) ListChatsGuarded(_ context.Context, _ archive.ReadScope, _ archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	return archive.ChatPage{Chats: []archive.ChatView{{ID: "opaque-chat", Name: r.body, MessageCount: 1}}}, r.read(), nil
}
func (r *sourceTestReader) StatusGuarded(_ context.Context, _ archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	return archive.StatusSnapshot{Messages: 1, Chats: []archive.ChatView{{ID: "opaque-chat", Name: r.body}}, Rebuild: &archive.RebuildStatus{Mode: "live_synthetic"}, Connection: "connected", Coverage: "complete", Keyword: archive.Progress{Pending: 4}, Semantic: archive.Progress{State: "running", Pending: 2}, Media: archive.Progress{State: "running", Pending: 3}}, r.read(), nil
}
func (r *sourceTestReader) Search(ctx context.Context, s archive.ReadScope, q archive.SearchRequest) (archive.SearchPage, error) {
	p, _, e := r.SearchGuarded(ctx, s, q)
	return p, e
}
func (r *sourceTestReader) Context(ctx context.Context, s archive.ReadScope, q archive.ContextRequest) (archive.ContextPage, error) {
	p, _, e := r.ContextGuarded(ctx, s, q)
	return p, e
}
func (r *sourceTestReader) ListChats(ctx context.Context, s archive.ReadScope, q archive.ChatListRequest) (archive.ChatPage, error) {
	p, _, e := r.ListChatsGuarded(ctx, s, q)
	return p, e
}
func (r *sourceTestReader) Status(ctx context.Context, s archive.ReadScope) (archive.StatusSnapshot, error) {
	p, _, e := r.StatusGuarded(ctx, s)
	return p, e
}
func (r *sourceTestReader) CheckResponse(_ context.Context, _ archive.ReadScope, c archive.ResponseCheck) (archive.ReadSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	if r.onCheck != nil {
		r.onCheck(r.checks)
	}
	if c.Credential == nil || c.Guard.PolicyGeneration != r.generation || r.failCheckAt > 0 && r.checks >= r.failCheckAt {
		return archive.ReadSnapshot{}, archive.ErrSourceUnavailable
	}
	return archive.ReadSnapshot{}, nil
}

type sourceTestHistoricalFactory struct {
	reader *sourceTestReader
	deny   bool
}

func (f *sourceTestHistoricalFactory) Metadata() archive.HistoricalViewMetadata {
	return archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}
}
func (f *sourceTestHistoricalFactory) ForCredential(_ context.Context, g archive.CredentialGrant) (archive.HistoricalRead, error) {
	if f.deny {
		return nil, archive.ErrUnauthorized
	}
	return &sourceTestHistoricalRead{sourceTestReader: f.reader, grant: g, meta: f.Metadata()}, nil
}
func (f *sourceTestHistoricalFactory) ForOwner(context.Context, archive.ReadScope) (archive.HistoricalRead, error) {
	return nil, archive.ErrUnauthorized
}

type sourceTestHistoricalRead struct {
	*sourceTestReader
	grant archive.CredentialGrant
	meta  archive.HistoricalViewMetadata
}

func (r *sourceTestHistoricalRead) Metadata() archive.HistoricalViewMetadata { return r.meta }
func (r *sourceTestHistoricalRead) StatsGuarded(context.Context, archive.ReadScope, archive.StatsRequest) (archive.StatsSnapshot, archive.ResponseGuard, error) {
	return archive.StatsSnapshot{}, archive.ResponseGuard{}, archive.ErrInvalidQuery
}
func (r *sourceTestHistoricalRead) CheckResponse(ctx context.Context, s archive.ReadScope, c archive.ResponseCheck) (archive.ReadSnapshot, error) {
	if c.Credential == nil || !reflect.DeepEqual(*c.Credential, r.grant) || !reflect.DeepEqual(s, r.grant.Scope) {
		return archive.ReadSnapshot{}, archive.ErrUnauthorized
	}
	return r.sourceTestReader.CheckResponse(ctx, s, c)
}

func configureTestHistorical(t *testing.T, c *Config, f archive.HistoricalReadFactory) {
	t.Helper()
	c.HistoricalReads = f
}

func TestHistoricalSourceSelectedReaderOwnsBothResponseFences(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
			historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC", failCheckAt: failAt}
			factory := &sourceTestHistoricalFactory{reader: historical}
			server, _ := httpFixture(t, live, credentialsFixture(), func(c *Config) { configureTestHistorical(t, c, factory) })
			status, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "synthetic", "source": "historical"}), nil)
			liveReads, liveChecks := live.counts()
			historyReads, historyChecks := historical.counts()
			if failAt == 0 {
				if status != http.StatusOK || strings.Contains(body, `"isError":true`) || !strings.Contains(body, "HISTORY_SYNTHETIC") || strings.Contains(body, "LIVE_SYNTHETIC") {
					t.Fatalf("wrong selected response: status=%d body=%s", status, body)
				}
			} else if status != http.StatusConflict || strings.Contains(body, "HISTORY_SYNTHETIC") {
				t.Fatalf("historical guard failed to discard buffered content: %d %s", status, body)
			}
			if liveReads != 0 || liveChecks != 0 || historyReads != 1 || historyChecks != 2 {
				t.Fatalf("selected source fences crossed stores: live %d/%d historical %d/%d", liveReads, liveChecks, historyReads, historyChecks)
			}
		})
	}
}

func TestHistoricalSourceEveryReadCarriesCaptureMetadata(t *testing.T) {
	for _, name := range []string{"search_messages", "get_context", "list_chats", "get_index_status", "search_contacts", "search_groups"} {
		t.Run(name, func(t *testing.T) {
			factory := &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}}
			server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentialsFixture(), func(c *Config) { configureTestHistorical(t, c, factory) })
			args := map[string]any{"source": "historical"}
			switch name {
			case "search_messages", "search_contacts", "search_groups":
				args["query"] = "synthetic"
			case "get_context":
				args["message_id"] = "h1.0123456789abcdef.m.synthetic"
			}
			status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
			var response struct {
				Result struct {
					StructuredContent struct {
						ArchiveView       archive.HistoricalViewMetadata `json:"archive_view"`
						CurrentPhoneState string                         `json:"current_phone_state"`
						AvailableSources  []string                       `json:"available_sources"`
						Connection        string                         `json:"connection"`
						Coverage          string                         `json:"coverage"`
						Paused            bool                           `json:"paused"`
						Keyword           progressDTO                    `json:"keyword"`
						Semantic          progressDTO                    `json:"semantic"`
						Media             progressDTO                    `json:"media"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			if status != 200 || json.Unmarshal([]byte(body), &response) != nil || response.Result.StructuredContent.ArchiveView != factory.Metadata() || response.Result.StructuredContent.CurrentPhoneState != "unknown" {
				t.Fatalf("historical result omitted truthful metadata: %s", body)
			}
			if len(response.Result.StructuredContent.AvailableSources) != 0 || strings.Contains(body, `"live_rebuild"`) {
				t.Fatal("historical status included live progress or source counts")
			}
			if name == "get_index_status" {
				status := response.Result.StructuredContent
				if status.Connection != "historical_snapshot" || status.Coverage != "partial" || !status.Paused || status.Keyword.Pending != 0 || status.Semantic.State != "unavailable" || status.Media.State != "unavailable" || status.Semantic.Pending != 0 || status.Media.Pending != 0 {
					t.Fatal("historical status advertised active live work or phone coverage")
				}
			}
		})
	}
}

func TestHistoricalSourceDefaultRemainsLiveAndDenialsNeverFallback(t *testing.T) {
	for _, source := range []string{"", "live", "historical", "other"} {
		t.Run(source, func(t *testing.T) {
			live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
			historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}
			factory := &sourceTestHistoricalFactory{reader: historical, deny: true}
			server, _ := httpFixture(t, live, credentialsFixture(), func(c *Config) { configureTestHistorical(t, c, factory) })
			args := map[string]any{"query": "synthetic"}
			if source != "" {
				args["source"] = source
			}
			_, body := rpc(t, server, chatToolRPC(t, "search_messages", args), nil)
			reads, _ := live.counts()
			if source == "" || source == "live" {
				if !strings.Contains(body, "LIVE_SYNTHETIC") || strings.Contains(body, `"archive_view"`) || reads != 1 {
					t.Fatal("default source changed")
				}
			} else if !strings.Contains(body, `"isError":true`) || reads != 0 || strings.Contains(body, "LIVE_SYNTHETIC") {
				t.Fatal("denied source fell back to live")
			}
		})
	}
	live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
	server, _ := httpFixture(t, live, credentialsFixture(), nil)
	_, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "synthetic", "source": "historical"}), nil)
	reads, _ := live.counts()
	if !strings.Contains(body, `"isError":true`) || reads != 0 {
		t.Fatal("disabled historical source was admitted")
	}
}

func TestHistoricalSourceSchemasExtendOnlySixReadsAndPreserveTwelveTools(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentialsFixture(), func(c *Config) {
				c.Attachments = &fakeAttachments{}
				c.Outbound = &sendBackend{}
				if enabled {
					configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73}})
				}
			})
			session, ctx := sdkSession(t, server, fakeToken())
			listed, err := session.ListTools(ctx, nil)
			if err != nil || len(listed.Tools) != 12 {
				t.Fatalf("catalog changed: %v %#v", err, listed)
			}
			for _, tool := range listed.Tools {
				read := contains([]string{"search_messages", "get_context", "list_chats", "get_index_status", "search_contacts", "search_groups"}, tool.Name)
				data, _ := json.Marshal(tool.InputSchema)
				var schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				}
				if json.Unmarshal(data, &schema) != nil {
					t.Fatal("invalid tool schema")
				}
				_, hasSource := schema.Properties["source"]
				if hasSource != (enabled && read) {
					t.Fatalf("source exposed on wrong tool: %s %s", tool.Name, data)
				}
				if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != (read || tool.Name == "get_send_status" || tool.Name == "get_attachment" || tool.Name == "search_attachments") {
					t.Fatalf("annotations changed: %s", tool.Name)
				}
			}
		})
	}
}

func TestHistoricalSourceLiveRejectsHistoricalHandlesAndOriginalBounds(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"get_context", map[string]any{"message_id": "h1.0123456789abcdef.m.synthetic"}},
		{"get_context", map[string]any{"message_id": strings.Repeat("m", 129)}},
		{"search_messages", map[string]any{"query": "synthetic", "chat_ids": []string{"h1.0123456789abcdef.c.synthetic"}}},
		{"search_messages", map[string]any{"query": "synthetic", "cursor": "h1.0123456789abcdef.p.synthetic"}},
		{"list_chats", map[string]any{"cursor": "h1.0123456789abcdef.p.synthetic"}},
		{"search_groups", map[string]any{"query": "synthetic", "cursor": strings.Repeat("p", 2049)}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
			server, _ := httpFixture(t, live, credentialsFixture(), func(c *Config) {
				configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73}})
			})
			test.args["source"] = "live"
			_, body := rpc(t, server, chatToolRPC(t, test.name, test.args), nil)
			reads, _ := live.counts()
			if !strings.Contains(body, `"isError":true`) || reads != 0 {
				t.Fatalf("history handle or expanded bound reached live reader: %s", body)
			}
		})
	}
}

func TestHistoricalSourceConcurrentRequestsKeepSelectedReader(t *testing.T) {
	live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
	historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}
	server, _ := httpFixture(t, live, credentialsFixture(), func(c *Config) {
		c.MaxConcurrent = 16
		configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: historical})
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			source, want, other := "live", "LIVE_SYNTHETIC", "HISTORY_SYNTHETIC"
			if i%2 == 0 {
				source, want, other = "historical", "HISTORY_SYNTHETIC", "LIVE_SYNTHETIC"
			}
			status, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "synthetic", "source": source}), nil)
			if status != 200 || !strings.Contains(body, want) || strings.Contains(body, other) {
				t.Errorf("mixed source request crossed readers: %d %s", status, body)
			}
		}(i)
	}
	wg.Wait()
	lr, lc := live.counts()
	hr, hc := historical.counts()
	if lr != 6 || lc != 12 || hr != 6 || hc != 12 {
		t.Fatalf("mixed source checks crossed requests: %d/%d %d/%d", lr, lc, hr, hc)
	}
}

func TestHistoricalSourceFinalAdmissionStillRechecksNativeAndManagedRegistries(t *testing.T) {
	for _, mode := range []string{"native", "managed"} {
		t.Run(mode, func(t *testing.T) {
			credentials := nativeCredentialsFixture()
			native := nativeRegistryFixture(credentials.grant)
			if mode == "managed" {
				credentials = remoteCredentialsFixture()
			}
			remote := remoteRegistryFixture(credentials.grant)
			historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC", onCheck: func(n int) {
				if n == 1 {
					if mode == "native" {
						native.remove(nativeBinding(credentials.grant))
					} else {
						remote.remove(remoteBinding(credentials.grant))
					}
				}
			}}
			server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentials, func(c *Config) {
				configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: historical})
				if mode == "native" {
					c.Mode = LocalAdapter
					c.NativeClients = native
				} else {
					c.RemoteClients = remote
				}
			})
			status, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "synthetic", "source": "historical"}), func(r *http.Request) {
				if mode == "native" {
					r.Host = "localhost"
					r.Header.Del("Authorization")
					r.Header.Set(localAdapterHeader, fakeToken())
				}
			})
			if status != 401 || strings.Contains(body, "HISTORY_SYNTHETIC") {
				t.Fatal("unpublished current client received historical content")
			}
		})
	}
}

func TestHistoricalSourceEnvelopeBoundsAreEnabledOnlyForSelectedHistoricalReads(t *testing.T) {
	cases := []struct {
		name, key string
		size      int
		reject    bool
	}{
		{"get_context", "message_id", 140, false}, {"get_context", "message_id", 257, true},
		{"search_messages", "cursor", 2100, false}, {"search_messages", "cursor", 2305, true},
		{"list_chats", "cursor", 2100, false}, {"search_contacts", "cursor", 2305, true},
	}
	for _, test := range cases {
		t.Run(test.name+test.key, func(t *testing.T) {
			historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}
			server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentialsFixture(), func(c *Config) { configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: historical}) })
			args := map[string]any{"source": "historical", test.key: strings.Repeat("x", test.size)}
			if test.name == "search_messages" || test.name == "search_contacts" {
				args["query"] = "synthetic"
			}
			_, body := rpc(t, server, chatToolRPC(t, test.name, args), nil)
			reads, _ := historical.counts()
			if test.reject {
				if !strings.Contains(body, `"isError":true`) || reads != 0 {
					t.Fatal("unbounded historical envelope reached reader")
				}
			} else if strings.Contains(body, `"isError":true`) || reads != 1 {
				t.Fatalf("bounded historical envelope rejected: %s", body)
			}
		})
	}
}

func TestHistoricalSourceCannotSelectSendStatusOrAttachmentServices(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"send_message", map[string]any{"chat_id": "h1.0123456789abcdef.c.synthetic", "request_id": "synthetic", "text": "synthetic"}},
		{"send_message", map[string]any{"source": "historical", "chat_id": "opaque-chat", "request_id": "synthetic", "text": "synthetic"}},
		{"get_send_status", map[string]any{"source": "historical", "request_id": "synthetic"}},
		{"get_attachment", map[string]any{"attachment_id": "h1.0123456789abcdef.a.synthetic"}},
		{"get_attachment", map[string]any{"source": "historical", "attachment_id": "opaque-attachment"}},
		{"search_attachments", map[string]any{"source": "historical"}},
		{"search_attachments", map[string]any{"cursor": "h1.0123456789abcdef.p.synthetic"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			send := &sendBackend{}
			attachments := &fakeAttachments{}
			server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentialsFixture(), func(c *Config) {
				c.Outbound = send
				c.Attachments = attachments
				configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73}})
			})
			_, body := rpc(t, server, chatToolRPC(t, test.name, test.args), nil)
			if !strings.Contains(body, `"isError":true`) || send.callCount() != 0 || attachments.searches != 0 || attachments.prepares != 0 {
				t.Fatalf("historical source or handle reached live service: %s", body)
			}
		})
	}
}

func TestHistoricalSourceLiveStatusAdvertisesAvailabilityWithoutHistoricalCounts(t *testing.T) {
	live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
	historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}
	server, _ := httpFixture(t, live, credentialsFixture(), func(c *Config) { configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: historical}) })
	_, body := rpc(t, server, statusRPC, nil)
	if !strings.Contains(body, `"available_sources":["live","historical"]`) || strings.Contains(body, "HISTORY_SYNTHETIC") || strings.Contains(body, `"archive_view"`) {
		t.Fatalf("live status hid configured source or mixed counts: %s", body)
	}
	reads, checks := historical.counts()
	if reads != 0 || checks != 0 {
		t.Fatal("live status queried historical data")
	}
}

func TestHistoricalSourceJSONSendingRejectsHistoricalHandlesBeforeBackend(t *testing.T) {
	backend := &sendBackend{}
	server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentialsFixture(), func(c *Config) {
		c.Outbound = backend
		c.HistoricalReads = &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73}}
	})
	for _, route := range []string{"message", "media"} {
		args := map[string]any{"chat_id": "h1.0123456789abcdef.c.synthetic", "request_id": "synthetic"}
		switch route {
		case "message":
			args["text"] = "synthetic"
		case "media":
			args["kind"] = "document"
			args["filename"] = "synthetic.txt"
			args["mime_type"] = "text/plain"
			args["data_base64"] = "c3ludGhldGlj"
		}
		data, _ := json.Marshal(args)
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/send/"+route, strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+fakeToken())
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 || backend.callCount() != 0 {
			t.Fatal("historical handle reached live JSON sender")
		}
	}
}

func TestHistoricalSourceAvailabilityPreservesLiveSendPayloadAndScope(t *testing.T) {
	backend := &sendBackend{}
	credentials := credentialsFixture()
	server, _ := httpFixture(t, &sourceTestReader{generation: 1}, credentials, func(c *Config) {
		c.Outbound = backend
		c.HistoricalReads = &sourceTestHistoricalFactory{reader: &sourceTestReader{generation: 73}}
	})
	_, body := rpc(t, server, chatToolRPC(t, "send_message", map[string]any{"chat_id": "opaque-chat", "request_id": "synthetic-live-send", "text": "synthetic live message"}), nil)
	calls, grant, request := backend.snapshot()
	if strings.Contains(body, `"isError":true`) || calls != 1 || !reflect.DeepEqual(grant, credentials.grant) || request.ChatID != "opaque-chat" || request.RequestID != "synthetic-live-send" || request.Text != "synthetic live message" {
		t.Fatal("historical availability changed live send authority or payload")
	}
}
