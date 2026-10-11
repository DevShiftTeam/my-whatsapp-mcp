package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func attachmentStdioTool(name string) *mcp.Tool {
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true}
	if name == "send_message" || name == "send_media" || name == "send_voice_note" {
		flag := true
		annotations = &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &flag, OpenWorldHint: &flag}
	}
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: annotations}
}

func TestStdioAttachmentCatalogRequiresCompleteCapabilitySets(t *testing.T) {
	core := []string{"search_messages", "get_context", "list_chats", "get_index_status"}
	optional := []string{"search_contacts", "search_groups", "search_attachments", "get_attachment", "send_message", "send_media", "send_voice_note", "get_send_status"}
	// Optional bits encode two contact tools, two attachment tools and four send
	// tools. These are the six complete, compatible daemon capability sets.
	complete := map[int]bool{
		0b00000000: true, // Four base read tools.
		0b00000011: true, // Six read tools with contact and group search.
		0b11110000: true, // Legacy eight-tool catalog with sending.
		0b11110011: true, // Ten tools with contact search and sending.
		0b00001111: true, // Eight read tools with attachment access.
		0b11111111: true, // Twelve tools with attachment access and sending.
	}
	for mask := 0; mask < 1<<len(optional); mask++ {
		t.Run(fmt.Sprintf("optional_mask_%08b", mask), func(t *testing.T) {
			tools := make([]*mcp.Tool, 0, len(core)+len(optional))
			for _, name := range core {
				tools = append(tools, attachmentStdioTool(name))
			}
			for bit, name := range optional {
				if mask&(1<<bit) != 0 {
					tools = append(tools, attachmentStdioTool(name))
				}
			}
			for range 2 {
				if validAdapterToolCatalog(tools) != complete[mask] {
					t.Fatal("stdio accepted a partial catalog or rejected a complete compatible catalog")
				}
				slices.Reverse(tools)
			}
		})
	}
}

func TestStdioAttachmentCatalogRejectsUntrustedDeclarations(t *testing.T) {
	names := []string{"search_messages", "get_context", "list_chats", "get_index_status", "search_contacts", "search_groups", "search_attachments", "get_attachment"}
	for _, test := range []struct {
		name   string
		change func([]*mcp.Tool)
	}{
		{"nil_tool", func(tools []*mcp.Tool) { tools[0] = nil }},
		{"duplicate_tool", func(tools []*mcp.Tool) { tools[3] = attachmentStdioTool("search_messages") }},
		{"unknown_read_tool", func(tools []*mcp.Tool) { tools[3] = attachmentStdioTool("read_everything") }},
		{"missing_core_tool", func(tools []*mcp.Tool) { tools[0] = attachmentStdioTool("get_send_status") }},
		{"search_attachment_write_hint", func(tools []*mcp.Tool) { tools[6].Annotations.ReadOnlyHint = false }},
		{"get_attachment_write_hint", func(tools []*mcp.Tool) { tools[7].Annotations.ReadOnlyHint = false }},
		{"search_attachment_missing_annotations", func(tools []*mcp.Tool) { tools[6].Annotations = nil }},
		{"get_attachment_missing_annotations", func(tools []*mcp.Tool) { tools[7].Annotations = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := make([]*mcp.Tool, 0, len(names))
			for _, name := range names {
				tools = append(tools, attachmentStdioTool(name))
			}
			test.change(tools)
			if validAdapterToolCatalog(tools) {
				t.Fatal("stdio trusted an unknown, duplicated, incomplete or writable attachment declaration")
			}
		})
	}
}

func TestStdioAdapterForwardsAttachmentTools(t *testing.T) {
	for _, withOutbound := range []bool{false, true} {
		name := "eight_read_tools"
		if withOutbound {
			name = "twelve_tools_with_outbound"
		}
		t.Run(name, func(t *testing.T) {
			credentials := credentialsFixture()
			credentials.grant.Scope.Principal = archive.NativeClientPrincipalPrefix + "v1/" + strings.Repeat("0", 32)
			config := stdioNativeConfig(credentials)
			names := []string{"search_messages", "get_context", "list_chats", "get_index_status", "search_contacts", "search_groups", "search_attachments", "get_attachment"}
			if withOutbound {
				names = append(names, "send_message", "send_media", "send_voice_note", "get_send_status")
			}
			pdf := []byte("%PDF-1.4\nsynthetic attachment fixture\n%%EOF\n")
			payloads := map[string]map[string]any{
				"search_attachments": {
					"results": []any{map[string]any{"message_id": "opaque-message-example", "file_name": "invoice-example.pdf", "mime_type": "application/pdf"}},
				},
				"get_attachment": {
					"message_id": "opaque-message-example", "file_name": "invoice-example.pdf", "mime_type": "application/pdf",
					"byte_length": len(pdf), "inline_base64": base64.StdEncoding.EncodeToString(pdf),
				},
			}
			type forwardedCall struct {
				name string
				args json.RawMessage
			}
			calls := make(chan forwardedCall, 2)
			server := mcp.NewServer(&mcp.Implementation{Name: "synthetic-attachments", Version: "1"}, nil)
			for _, name := range names {
				server.AddTool(attachmentStdioTool(name), func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					payload := payloads[name]
					if payload == nil {
						return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "synthetic unused tool"}}}, nil
					}
					calls <- forwardedCall{name: name, args: append(json.RawMessage(nil), request.Params.Arguments...)}
					data, err := json.Marshal(payload)
					if err != nil {
						return nil, err
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: payload}, nil
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
						t.Error("attachment adapter did not join after cancellation")
					}
				}
			}()
			go func() { done <- runStdioTransport(ctx, daemon.adapterConfig(fakeToken()), serverTransport) }()
			session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-attachment-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal("stdio rejected the complete attachment catalog")
			}
			defer session.Close()
			listed, err := session.ListTools(ctx, nil)
			if err != nil || len(listed.Tools) != len(names) {
				t.Fatal("stdio changed the complete attachment catalog")
			}
			for _, tool := range listed.Tools {
				if (tool.Name == "search_attachments" || tool.Name == "get_attachment") && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
					t.Fatal("attachment forwarding lost the read-only annotation")
				}
			}
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{"search_attachments", map[string]any{"query": "invoice", "file_name": "invoice", "mime_type": "application/pdf", "chat_ids": []string{"opaque-chat-example"}, "limit": 5}},
				{"get_attachment", map[string]any{"message_id": "opaque-message-example", "offset": 0, "length": 4096}},
			} {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
				if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
					t.Fatal("stdio failed to forward a read-only attachment call")
				}
				text, ok := result.Content[0].(*mcp.TextContent)
				wantJSON, wantErr := json.Marshal(payloads[call.name])
				gotJSON, gotErr := json.Marshal(result.StructuredContent)
				if !ok || wantErr != nil || gotErr != nil || text.Text != string(wantJSON) || !bytes.Equal(gotJSON, wantJSON) {
					t.Fatal("stdio changed attachment text, file metadata or inline bytes")
				}
				select {
				case forwarded := <-calls:
					var args map[string]any
					wantedArgs, wantedErr := json.Marshal(call.args)
					if json.Unmarshal(forwarded.args, &args) != nil {
						t.Fatal("forwarded attachment arguments are not JSON")
					}
					actualArgs, actualErr := json.Marshal(args)
					if forwarded.name != call.name || wantedErr != nil || actualErr != nil || !bytes.Equal(actualArgs, wantedArgs) {
						t.Fatal("stdio changed attachment arguments")
					}
				case <-ctx.Done():
					t.Fatal("attachment call never reached its authenticated daemon")
				}
			}
			_ = session.Close()
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("attachment stdio adapter did not join")
			}
		})
	}
}
