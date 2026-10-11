package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type chatSearchReader struct {
	fakeReader
	requests []archive.ChatListRequest
}

func (f *chatSearchReader) ListChatsGuarded(_ context.Context, scope archive.ReadScope, request archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	f.reads++
	f.lastScope = scope
	f.requests = append(f.requests, request)
	page := archive.ChatPage{Chats: []archive.ChatView{{ID: "opaque-chat-1", Name: "ליה Example", MessageCount: 2}}, NextCursor: "opaque-next-page"}
	if request.Cursor != "" {
		page.Chats = []archive.ChatView{{ID: "opaque-chat-2", Name: "Lia Example", MessageCount: 1}}
		page.NextCursor = ""
	}
	return page, archive.ResponseGuard{}, nil
}

func chatToolRPC(t *testing.T, name string, args any) string {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	return string(request)
}

func TestChatNameSearchToolsForwardTypeScopeAndPagination(t *testing.T) {
	for _, test := range []struct{ tool, chatType string }{
		{"search_contacts", "direct"},
		{"search_groups", "group"},
		{"list_chats", "group"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			reader := &chatSearchReader{}
			credentials := credentialsFixture()
			server, _ := httpFixture(t, reader, credentials, nil)
			args := map[string]any{"query": "liaexample", "limit": 1}
			if test.tool == "list_chats" {
				args["chat_type"] = test.chatType
			}
			for _, cursor := range []string{"", "opaque-next-page"} {
				if cursor != "" {
					args["cursor"] = cursor
				}
				status, body := rpc(t, server, chatToolRPC(t, test.tool, args), nil)
				var response struct {
					Result struct {
						Page chatPageDTO `json:"structuredContent"`
					} `json:"result"`
				}
				if status != http.StatusOK || json.Unmarshal([]byte(body), &response) != nil || !response.Result.Page.Untrusted || len(response.Result.Page.Chats) != 1 {
					t.Fatal("name search did not return the existing guarded chat-page shape")
				}
				page := response.Result.Page
				if cursor == "" && (page.Chats[0].ChatID != "opaque-chat-1" || page.NextCursor != "opaque-next-page") || cursor != "" && (page.Chats[0].ChatID != "opaque-chat-2" || page.NextCursor != "") {
					t.Fatal("name search dropped the opaque pagination contract")
				}
				request := reader.requests[len(reader.requests)-1]
				if request.Query != "liaexample" || request.ChatType != test.chatType || request.Limit != 1 || request.Cursor != cursor || reader.lastScope.Principal != credentials.grant.Scope.Principal || reader.lastScope.Account != credentials.grant.Scope.Account {
					t.Fatal("name search changed the query, chat type, pagination or credential scope")
				}
				if strings.Contains(body, "@s.whatsapp.net") || strings.Contains(body, "@g.us") || strings.Contains(body, fakeToken()) {
					t.Fatal("name search exposed a provider identity or credential")
				}
			}
			if reader.reads != 2 || reader.checks != 4 {
				t.Fatal("name search bypassed its tool or final HTTP response guard")
			}
		})
	}
}

func TestChatNameSearchRejectsInvalidArgumentsBeforeArchiveRead(t *testing.T) {
	invalid := []map[string]any{
		{},
		{"query": ""},
		{"query": " \t\n"},
		{"query": "'---_%*"},
		{"query": "\u05b0\u05b7"},
		{"query": "lia\x00example"},
		{"query": strings.Repeat("ל", 129)},
		{"query": "lia", "limit": -1},
		{"query": "lia", "limit": 51},
		{"query": "lia", "cursor": strings.Repeat("x", 2049)},
		{"query": "lia", "chat_type": "group"},
		{"query": "lia", "account": "private-account-canary"},
	}
	for _, tool := range []string{"search_contacts", "search_groups"} {
		for _, args := range invalid {
			reader := &chatSearchReader{}
			server, _ := httpFixture(t, reader, credentialsFixture(), nil)
			status, body := rpc(t, server, chatToolRPC(t, tool, args), nil)
			if status != http.StatusOK || !strings.Contains(body, `"isError":true`) || reader.reads != 0 || strings.Contains(body, "private-account-canary") {
				t.Fatalf("%s accepted invalid name-search arguments", tool)
			}
		}
	}
	for _, args := range []map[string]any{
		{"query": "\t"}, {"query": "name", "chat_type": "broadcast"}, {"query": strings.Repeat("x", 257)},
	} {
		reader := &chatSearchReader{}
		server, _ := httpFixture(t, reader, credentialsFixture(), nil)
		_, body := rpc(t, server, chatToolRPC(t, "list_chats", args), nil)
		if !strings.Contains(body, `"isError":true`) || reader.reads != 0 {
			t.Fatal("list_chats accepted invalid name or chat type")
		}
	}
}

func TestChatNameSearchFinalGuardWithholdsNames(t *testing.T) {
	for _, tool := range []string{"search_contacts", "search_groups"} {
		reader := &chatSearchReader{fakeReader: fakeReader{failCheckAt: 2}}
		server, _ := httpFixture(t, reader, credentialsFixture(), nil)
		status, body := rpc(t, server, chatToolRPC(t, tool, map[string]any{"query": "lia"}), nil)
		if status != http.StatusConflict || strings.Contains(body, "Example") || strings.Contains(body, "opaque-chat-1") || strings.Contains(body, "next_cursor") || reader.reads != 1 || reader.checks != 2 {
			t.Fatal("name-search metadata escaped the final source guard")
		}
	}
}

func TestChatNameSearchToolMetadataExplainsArchivedScope(t *testing.T) {
	server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), nil)
	session, ctx := sdkSession(t, server, fakeToken())
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range tools.Tools {
		if tool.Name != "search_contacts" && tool.Name != "search_groups" {
			continue
		}
		found++
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || !strings.Contains(tool.Description, "archived") || !strings.Contains(tool.Description, "client grant") || !strings.Contains(tool.Description, "next_cursor") || !strings.Contains(tool.Description, "separate owner-approved permission") {
			t.Fatal("name-search discovery omitted scope, pagination or send-permission limits")
		}
		schemaJSON, err := json.Marshal(tool.InputSchema)
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err != nil || json.Unmarshal(schemaJSON, &schema) != nil || len(schema.Required) != 1 || schema.Required[0] != "query" || len(schema.Properties) != 3 {
			t.Fatal("name-search schema did not require a query or leaked namespace selectors")
		}
	}
	if found != 2 {
		t.Fatal("contact and group name-search tools are not both discoverable")
	}
}

func TestStdioAdapterForwardsContactAndGroupSearch(t *testing.T) {
	for _, withOutbound := range []bool{false, true} {
		name := "read_only"
		if withOutbound {
			name = "with_outbound"
		}
		t.Run(name, func(t *testing.T) {
			credentials := credentialsFixture()
			credentials.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
			config := stdioNativeConfig(credentials)
			if withOutbound {
				config.Outbound = &sendBackend{}
			}
			reader := &chatSearchReader{}
			handler, err := NewHandler(config, reader, credentials)
			if err != nil {
				t.Fatal(err)
			}
			daemon := newStdioUnixDaemon(t, handler)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			go func() { done <- runStdioTransport(ctx, daemon.adapterConfig(fakeToken()), serverTransport) }()
			session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-name-search", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			expected := 6
			if withOutbound {
				expected = 10
			}
			if err != nil || len(tools.Tools) != expected {
				t.Fatal("stdio failed to discover the complete name-search capability set")
			}
			for _, tool := range []struct{ name, chatType string }{{"search_contacts", "direct"}, {"search_groups", "group"}} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: map[string]any{"query": "liaexample"}})
				if err != nil || result == nil || result.IsError || len(result.Content) != 1 || len(reader.requests) == 0 {
					t.Fatal("stdio failed to forward a contact or group name-search call")
				}
				request := reader.requests[len(reader.requests)-1]
				if request.Query != "liaexample" || request.ChatType != tool.chatType || request.Limit != 20 {
					t.Fatal("stdio changed the query, server-fixed type or default page size")
				}
			}
			_ = session.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("name-search stdio adapter did not join")
			}
		})
	}
}

func TestStdioAdapterRequiresCompleteOptionalToolSets(t *testing.T) {
	core := []string{"search_messages", "get_context", "list_chats", "get_index_status"}
	for _, test := range []struct {
		name    string
		extra   []string
		allowed bool
	}{
		{"legacy_four", nil, true},
		{"six_read_tools", []string{"search_contacts", "search_groups"}, true},
		{"legacy_eight", []string{"send_message", "send_media", "send_voice_note", "get_send_status"}, true},
		{"ten_tools", []string{"search_contacts", "search_groups", "send_message", "send_media", "send_voice_note", "get_send_status"}, true},
		{"missing_group_search", []string{"search_contacts", "get_send_status"}, false},
		{"missing_contact_search", []string{"search_groups", "get_send_status"}, false},
		{"partial_outbound", []string{"search_contacts", "search_groups", "send_message", "get_send_status"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			credentials := credentialsFixture()
			credentials.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
			config := stdioNativeConfig(credentials)
			server := mcp.NewServer(&mcp.Implementation{Name: "synthetic-tool-set", Version: "1"}, nil)
			for _, name := range append(append([]string(nil), core...), test.extra...) {
				annotations := &mcp.ToolAnnotations{ReadOnlyHint: true}
				if name == "send_message" || name == "send_media" || name == "send_voice_note" {
					flag := true
					annotations = &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &flag, OpenWorldHint: &flag}
				}
				server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: annotations}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "synthetic"}}}, nil
				})
			}
			sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})
			daemon := newStdioUnixDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := authenticate(r, config, credentials, nil); err != nil {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				sdk.ServeHTTP(w, r)
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			done := make(chan error, 1)
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("tool-set adapter did not join after cancellation")
					}
				}
			}()
			go func() { done <- runStdioTransport(ctx, daemon.adapterConfig(fakeToken()), serverTransport) }()
			if test.allowed {
				session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-tool-set-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
				if err != nil {
					t.Fatal("compatible daemon capability set was rejected")
				}
				tools, err := session.ListTools(ctx, nil)
				_ = session.Close()
				if err != nil || len(tools.Tools) != len(core)+len(test.extra) {
					t.Fatal("stdio changed the compatible daemon capability set")
				}
			}
			select {
			case err := <-done:
				joined = true
				if test.allowed && err != nil || !test.allowed && !errors.Is(err, ErrUnavailable) {
					t.Fatal("stdio did not enforce complete optional capability sets")
				}
			case <-ctx.Done():
				t.Fatal("tool-set discovery did not finish")
			}
		})
	}
}
