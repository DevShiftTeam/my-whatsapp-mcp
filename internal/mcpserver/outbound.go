package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"strings"
)

type Outbound interface {
	Send(context.Context, archive.CredentialGrant, outbound.Request) (outbound.Receipt, error)
	SendStatus(context.Context, archive.CredentialGrant, string) (outbound.Receipt, error)
	CheckSendResponse(context.Context, archive.CredentialGrant, outbound.Receipt) error
}

const sendReceiptSchema = `{"type":"object","required":["request_id","chat_id","state","duplicate"],"properties":{"request_id":{"type":"string"},"chat_id":{"type":"string"},"message_id":{"type":"string"},"state":{"type":"string","enum":["accepted","failed","outcome_unknown"]},"accepted_at":{"type":"string","format":"date-time"},"duplicate":{"type":"boolean"}},"additionalProperties":false}`

var sendSchemas = map[string]string{
	"send_message":    `{"type":"object","additionalProperties":false,"required":["chat_id","request_id","text"],"properties":{"chat_id":{"type":"string","minLength":1,"maxLength":128},"request_id":{"type":"string","minLength":1,"maxLength":128},"text":{"type":"string","minLength":1,"maxLength":65536}}}`,
	"send_media":      `{"type":"object","additionalProperties":false,"required":["chat_id","request_id","kind","data_base64","mime_type"],"properties":{"chat_id":{"type":"string","minLength":1,"maxLength":128},"request_id":{"type":"string","minLength":1,"maxLength":128},"kind":{"type":"string","enum":["image","video","document","audio"]},"data_base64":{"type":"string","minLength":1,"maxLength":11184812},"mime_type":{"type":"string","maxLength":128},"filename":{"type":"string","maxLength":255},"caption":{"type":"string","maxLength":4096},"seconds":{"type":"integer","minimum":0,"maximum":3600}}}`,
	"send_voice_note": `{"type":"object","additionalProperties":false,"required":["chat_id","request_id","data_base64","seconds"],"properties":{"chat_id":{"type":"string","minLength":1,"maxLength":128},"request_id":{"type":"string","minLength":1,"maxLength":128},"data_base64":{"type":"string","minLength":1,"maxLength":11184812},"seconds":{"type":"integer","minimum":1,"maximum":3600}}}`,
	"get_send_status": `{"type":"object","additionalProperties":false,"required":["request_id"],"properties":{"request_id":{"type":"string","minLength":1,"maxLength":128}}}`,
}

func addOutboundTools(s *mcp.Server, c Config, credentials Credentials) {
	descriptions := map[string]string{
		"send_message":    "Send text to an owner-approved chat_id. Requires explicit send permission and a unique request_id; reuse that ID and identical content after a timeout. accepted means server acknowledgement, not recipient delivery.",
		"send_media":      "Send an image, MP4 video, document or audio attachment (up to 8 MiB decoded). Pass standard base64 bytes, never a path or URL. Documents require filename. Requires owner send permission and a unique request_id; do not resend outcome_unknown under a new ID.",
		"send_voice_note": "Send an existing Ogg/Opus recording as a WhatsApp voice note (PTT). Pass base64 bytes and duration in seconds (1-3600), up to 8 MiB. Requires owner send permission and a unique request_id. Does not record or synthesize audio.",
		"get_send_status": "Look up a send request for this credential. accepted is WhatsApp server acknowledgement; outcome_unknown requires checking WhatsApp before any new send. Never automatically resends.",
	}
	for _, name := range []string{"send_message", "send_media", "send_voice_note", "get_send_status"} {
		open := name != "get_send_status"
		destructive := open
		annotations := &mcp.ToolAnnotations{ReadOnlyHint: !open, DestructiveHint: &destructive, IdempotentHint: true, OpenWorldHint: &open}
		s.AddTool(&mcp.Tool{Name: name, Description: descriptions[name], InputSchema: json.RawMessage(sendSchemas[name]), OutputSchema: json.RawMessage(sendReceiptSchema), Annotations: annotations}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			state, ok := ctx.Value(requestKey{}).(*requestState)
			if !ok {
				return sendToolFailure(ErrUnauthorized), nil
			}
			deadline, ok := state.ctx.Deadline()
			if !ok {
				return sendToolFailure(ErrUnavailable), nil
			}
			ctx, cancel := context.WithDeadline(ctx, deadline)
			stop := context.AfterFunc(state.ctx, cancel)
			defer func() { stop(); cancel() }()
			grant, err := state.principal.resolve(ctx, credentials)
			if err != nil {
				return sendToolFailure(err), nil
			}
			if c.HistoricalSnapshot != nil && name != "get_send_status" {
				return sendToolFailure(archive.ErrHistoricalSnapshotReadOnly), nil
			}
			receipt, err := runSend(ctx, c.Outbound, grant, name, r.Params.Arguments)
			if err != nil {
				return sendToolFailure(err), nil
			}
			state.noteSend(grant, receipt)
			data, _ := json.Marshal(receipt)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: json.RawMessage(data)}, nil
		})
	}
}
func decodeSend(raw json.RawMessage, out any, allowed []string) error {
	if decodeArgs(raw, out) != nil {
		return outbound.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return outbound.ErrInvalid
	}
	seen := map[string]bool{}
	fields := map[string]bool{}
	for _, k := range allowed {
		fields[k] = true
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return outbound.ErrInvalid
		}
		k, ok := key.(string)
		if !ok || seen[k] || !fields[k] {
			return outbound.ErrInvalid
		}
		seen[k] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return outbound.ErrInvalid
		}
	}
	return nil
}
func runSend(ctx context.Context, b Outbound, g archive.CredentialGrant, name string, raw json.RawMessage) (outbound.Receipt, error) {
	if name == "get_send_status" {
		var r struct {
			RequestID string `json:"request_id"`
		}
		if decodeSend(raw, &r, []string{"request_id"}) != nil || !outbound.ValidRequestID(r.RequestID) {
			return outbound.Receipt{}, outbound.ErrInvalid
		}
		return b.SendStatus(ctx, g, r.RequestID)
	}
	var r outbound.Request
	fields := []string{"chat_id", "request_id"}
	switch name {
	case "send_message":
		fields = append(fields, "text")
	case "send_media":
		fields = append(fields, "kind", "data_base64", "mime_type", "filename", "caption", "seconds")
	case "send_voice_note":
		fields = append(fields, "data_base64", "seconds")
	default:
		return outbound.Receipt{}, outbound.ErrInvalid
	}
	if decodeSend(raw, &r, fields) != nil || archive.IsHistoricalHandle(string(r.ChatID)) {
		return outbound.Receipt{}, outbound.ErrInvalid
	}
	switch name {
	case "send_message":
		r.Kind = "text"
	case "send_voice_note":
		r.Kind = "voice"
		r.MIMEType = "audio/ogg; codecs=opus"
	case "send_media":
		if r.Kind != "image" && r.Kind != "video" && r.Kind != "document" && r.Kind != "audio" {
			return outbound.Receipt{}, outbound.ErrInvalid
		}
	}
	if _, err := outbound.Validate(r); err != nil {
		return outbound.Receipt{}, err
	}
	return b.Send(ctx, g, r)
}
func sendCode(err error) string {
	for _, e := range []error{archive.ErrHistoricalSnapshotReadOnly, outbound.ErrInvalid, outbound.ErrUnavailable, archive.ErrSendForbidden, archive.ErrSendConflict, archive.ErrSendNotFound, ErrUnauthorized, archive.ErrUnauthorized, archive.ErrPolicyChanged, archive.ErrReconciliationRequired} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "send_unavailable"
}
func sendToolFailure(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: sendCode(err)}}}
}
func sendAPIRoute(path string) bool {
	return path == "/api/send/message" || path == "/api/send/media" || path == "/api/send/voice" || path == "/api/send/status"
}
func serveSendAPI(w http.ResponseWriter, r *http.Request, c Config, credentials Credentials) {
	state, ok := r.Context().Value(requestKey{}).(*requestState)
	if !ok {
		fixedHTTP(w, 401, "mcp_unauthorized")
		return
	}
	grant, err := state.principal.resolve(r.Context(), credentials)
	if err != nil {
		fixedHTTP(w, 401, "mcp_unauthorized")
		return
	}
	if c.HistoricalSnapshot != nil && r.URL.Path != "/api/send/status" {
		fixedHTTP(w, 403, "historical_snapshot_read_only")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		fixedHTTP(w, 400, "send_invalid_request")
		return
	}
	name := map[string]string{"message": "send_message", "media": "send_media", "voice": "send_voice_note", "status": "get_send_status"}[strings.TrimPrefix(r.URL.Path, "/api/send/")]
	receipt, err := runSend(r.Context(), c.Outbound, grant, name, raw)
	if err != nil {
		status := 503
		if errors.Is(err, outbound.ErrInvalid) {
			status = 400
		}
		if errors.Is(err, archive.ErrSendForbidden) {
			status = 403
		}
		if errors.Is(err, archive.ErrSendConflict) {
			status = 409
		}
		if errors.Is(err, archive.ErrSendNotFound) {
			status = 404
		}
		fixedHTTP(w, status, sendCode(err))
		return
	}
	state.noteSend(grant, receipt)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(receipt)
}
