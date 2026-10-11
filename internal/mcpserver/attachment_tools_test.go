package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type fakeAttachments struct {
	searches, prepares, downloads int
	grant                         archive.CredentialGrant
	request                       archive.AttachmentSearchRequest
	mediaID                       archive.MediaID
	fetch, inline                 bool
}

func (f *fakeAttachments) Search(_ context.Context, grant archive.CredentialGrant, request archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error) {
	f.searches++
	f.grant, f.request = grant, request
	return archive.AttachmentPage{Attachments: []archive.AttachmentView{{AttachmentID: "opaque-attachment", MessageID: "opaque-document", ChatID: "opaque-chat", Filename: "הצעה.pdf", MIMEType: "application/pdf", Bytes: 31, DeclaredBytes: 31, Availability: "retained", Before: []archive.MessageView{{ID: "opaque-before", ChatID: "opaque-chat", Body: "Proposal for Friday"}}}}, NextCursor: "opaque-attachment-cursor", Snapshot: archive.ReadSnapshot{RecoveryCode: "owner_restored_snapshot", RecoveryEpoch: 8}}, archive.ResponseGuard{}, nil
}

func (f *fakeAttachments) Prepare(_ context.Context, grant archive.CredentialGrant, id archive.MediaID, fetch, inline bool) (AttachmentDelivery, archive.ResponseGuard, error) {
	f.prepares++
	f.grant, f.mediaID, f.fetch, f.inline = grant, id, fetch, inline
	return AttachmentDelivery{Attachment: archive.AttachmentView{AttachmentID: id, MessageID: "opaque-document", ChatID: "opaque-chat", Filename: "הצעה.pdf", MIMEType: "application/pdf", Bytes: 31, Availability: "retained"}, Snapshot: archive.ReadSnapshot{RecoveryCode: "owner_restored_snapshot", RecoveryEpoch: 8}, Status: "ready", DownloadURL: "https://archive.example/mcp/attachments/" + fakeToken(), ExpiresAt: time.Now().Add(time.Minute)}, archive.ResponseGuard{}, nil
}

func (f *fakeAttachments) ServeDownload(w http.ResponseWriter, _ *http.Request) {
	f.downloads++
	w.WriteHeader(http.StatusNotFound)
}

func TestAttachmentToolsPreserveScopeFiltersContextAndOriginalResponseFence(t *testing.T) {
	for _, name := range []string{"search_attachments", "get_attachment"} {
		t.Run(name, func(t *testing.T) {
			backend := &fakeAttachments{}
			reader := &fakeReader{}
			credentials := credentialsFixture()
			server, _ := httpFixture(t, reader, credentials, func(config *Config) { config.Attachments = backend })
			args := map[string]any{"query": "Friday", "filename": "הצעה", "file_type": "application/pdf", "chat_ids": []string{"opaque-chat"}, "before": 1, "after": 2, "limit": 2, "from": "2026-10-01T00:00:00Z", "until": "2026-10-12T00:00:00Z"}
			if name == "get_attachment" {
				args = map[string]any{"attachment_id": "opaque-attachment", "fetch_if_needed": true, "inline": false}
			}
			status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
			if status != http.StatusOK || strings.Contains(body, `"isError":true`) || !strings.Contains(body, "opaque-document") || !strings.Contains(body, `"recovery_epoch":8`) || !strings.Contains(body, `"untrusted":true`) || reader.checks != 2 {
				t.Fatal("attachment tool did not preserve the original tool and HTTP response fences")
			}
			if backend.grant.CredentialID != credentials.grant.CredentialID || backend.grant.Scope.Account != credentials.grant.Scope.Account || backend.grant.Scope.Principal != credentials.grant.Scope.Principal {
				t.Fatal("attachment tool changed the resolved credential scope")
			}
			if name == "search_attachments" {
				if backend.searches != 1 || backend.request.Query != "Friday" || backend.request.Filename != "הצעה" || backend.request.FileType != "application/pdf" || len(backend.request.ChatIDs) != 1 || backend.request.Before != 1 || backend.request.After != 2 || backend.request.Limit != 2 || !strings.Contains(body, "Proposal for Friday") || !strings.Contains(body, "opaque-attachment-cursor") {
					t.Fatal("attachment search dropped filters, context or pagination")
				}
			} else if backend.prepares != 1 || backend.mediaID != "opaque-attachment" || !backend.fetch || backend.inline || !strings.Contains(body, "download_url") {
				t.Fatal("get_attachment dropped its bounded retrieval choices")
			}
		})
	}
}

func TestAttachmentToolsRejectNamespaceAndDownloadInputsBeforeService(t *testing.T) {
	for _, name := range []string{"search_attachments", "get_attachment"} {
		invalid := []map[string]any{{"url": "https://private-canary.invalid/file"}, {"path": "/private-canary"}, {"account": "private-canary"}}
		if name == "search_attachments" {
			invalid = append(invalid, map[string]any{"query": strings.Repeat("x", 513)}, map[string]any{"filename": strings.Repeat("x", 256)}, map[string]any{"file_type": "text/plain; charset=utf-8"}, map[string]any{"file_type": "application/*"}, map[string]any{"before": 4}, map[string]any{"after": -1}, map[string]any{"limit": 21}, map[string]any{"cursor": strings.Repeat("x", 1025)}, map[string]any{"from": "2026-10-12T00:00:00Z", "until": "2026-10-01T00:00:00Z"})
		} else {
			invalid = append(invalid, map[string]any{}, map[string]any{"attachment_id": ""}, map[string]any{"attachment_id": strings.Repeat("x", 129)}, map[string]any{"attachment_id": "https://private-canary.invalid"})
		}
		for _, args := range invalid {
			backend := &fakeAttachments{}
			server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(config *Config) { config.Attachments = backend })
			_, body := rpc(t, server, chatToolRPC(t, name, args), nil)
			if !strings.Contains(body, `"isError":true`) || backend.searches != 0 || backend.prepares != 0 || strings.Contains(body, "private-canary") {
				t.Fatal("attachment tool accepted a caller namespace, path, URL or unbounded filter")
			}
		}
	}
}

func TestAttachmentToolFinalGuardWithholdsFilesAndNeighborText(t *testing.T) {
	for _, name := range []string{"search_attachments", "get_attachment"} {
		backend := &fakeAttachments{}
		reader := &fakeReader{failCheckAt: 2}
		server, _ := httpFixture(t, reader, credentialsFixture(), func(config *Config) { config.Attachments = backend })
		args := map[string]any{}
		if name == "get_attachment" {
			args["attachment_id"] = "opaque-attachment"
		}
		status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
		if status != http.StatusConflict || strings.Contains(body, "opaque-document") || strings.Contains(body, "Proposal") || strings.Contains(body, "download_url") {
			t.Fatal("attachment metadata or bytes escaped an invalidated original response guard")
		}
	}
}

func TestAttachmentToolsAdvertiseBoundedArchivedFileRetrieval(t *testing.T) {
	server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(config *Config) { config.Attachments = &fakeAttachments{} })
	session, ctx := sdkSession(t, server, fakeToken())
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 8 {
		t.Fatal("attachment read catalog is incomplete")
	}
	for _, tool := range listed.Tools {
		if tool.Name != "search_attachments" && tool.Name != "get_attachment" {
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !strings.Contains(tool.Description, "untrusted") || !strings.Contains(tool.Description, "permitted") {
			t.Fatal("attachment metadata omitted scope and untrusted-content limits")
		}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil || strings.Contains(string(encoded), `"url"`) || strings.Contains(string(encoded), `"path"`) || strings.Contains(string(encoded), `"account"`) {
			t.Fatal("attachment schema exposed a caller-selected download boundary")
		}
	}
}

func TestAttachmentDownloadDispatchKeepsHostOriginAndCanonicalPathChecks(t *testing.T) {
	backend := &fakeAttachments{}
	config := Config{Mode: Bearer, Hosts: []string{"archive.example"}, Origins: []string{"https://archive.example"}, Attachments: backend}
	handler, err := NewHandler(config, &fakeReader{}, credentialsFixture())
	if err != nil {
		t.Fatal(err)
	}
	path := "/mcp/attachments/" + fakeToken()
	for _, test := range []struct {
		path, method, host, origin string
		code                       int
		delegate                   bool
	}{
		{path, "GET", "archive.example", "", 404, true},
		{path, "POST", "archive.example", "", 404, false},
		{path, "GET", "wrong.example", "", 403, false},
		{path, "GET", "archive.example", "https://wrong.example", 403, false},
		{path + "?token=private-canary", "GET", "archive.example", "", 404, false},
		{path + "/extra", "GET", "archive.example", "", 404, false},
		{"/mcp/attachments/short", "GET", "archive.example", "", 404, false},
	} {
		before := backend.downloads
		request := httptest.NewRequest(test.method, "https://archive.example"+test.path, nil)
		request.Host = test.host
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, request)
		if rr.Code != test.code || (backend.downloads > before) != test.delegate || strings.Contains(rr.Body.String(), "private-canary") {
			t.Fatal("attachment route bypassed configured host, origin, method or canonical path checks")
		}
		if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("attachment dispatch omitted shared privacy headers")
		}
	}
}

func TestAttachmentToolsAreAbsentWithoutConfiguredService(t *testing.T) {
	server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), nil)
	for _, name := range []string{"search_attachments", "get_attachment"} {
		status, body := rpc(t, server, chatToolRPC(t, name, map[string]any{"attachment_id": "opaque-attachment"}), nil)
		if status != http.StatusOK || !strings.Contains(body, "error") || strings.Contains(body, "opaque-attachment") {
			t.Fatal("unconfigured attachment tool fabricated an attachment response")
		}
	}
}
