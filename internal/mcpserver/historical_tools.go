package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// selectedArchiveRead is local to one tool invocation. The response guard
// belongs to reader; its source cannot be inferred from the live grant.
type selectedArchiveRead struct {
	reader   guardedReader
	metadata *archive.HistoricalViewMetadata
}
type selectedReadOutput struct {
	value  any
	reader guardedReader
}
type selectedReadRunner func(context.Context, archive.CredentialGrant, selectedArchiveRead, json.RawMessage) (any, archive.ResponseGuard, error)
type selectedReadAdder func(string, string, string, selectedReadRunner)

func historicalReadAdder(c Config, live guardedReader, add attachmentToolAdder) selectedReadAdder {
	return func(name, description, schema string, run selectedReadRunner) {
		if c.HistoricalReads != nil {
			schema = historicalSourceSchema(schema)
			description += " Optional source selects live (default) or the captured, read-only historical archive under current permissions. Historical IDs cannot be used for live sends or downloads."
		}
		add(name, description, schema, func(ctx context.Context, grant archive.CredentialGrant, raw json.RawMessage) (any, archive.ResponseGuard, error) {
			selected, args, err := selectArchiveRead(ctx, c, live, grant, raw)
			if err != nil {
				return nil, archive.ResponseGuard{}, err
			}
			out, guard, err := run(ctx, grant, selected, args)
			if err != nil {
				return nil, guard, err
			}
			if selected.metadata != nil {
				out, err = withHistoricalMetadata(out, *selected.metadata)
				if err != nil {
					return nil, guard, err
				}
			}
			return selectedReadOutput{value: out, reader: selected.reader}, guard, nil
		})
	}
}

func selectArchiveRead(ctx context.Context, c Config, live guardedReader, grant archive.CredentialGrant, raw json.RawMessage) (selectedArchiveRead, json.RawMessage, error) {
	args, err := uniqueReadArguments(raw)
	if err != nil {
		return selectedArchiveRead{}, nil, err
	}
	source := "live"
	if value, present := args["source"]; present {
		if c.HistoricalReads == nil {
			return selectedArchiveRead{}, nil, archive.ErrSourceUnavailable
		}
		// null and the empty string are not the omitted default.
		if !bytes.HasPrefix(bytes.TrimSpace(value), []byte(`"`)) || json.Unmarshal(value, &source) != nil || (source != "live" && source != "historical") {
			return selectedArchiveRead{}, nil, archive.ErrInvalidQuery
		}
		delete(args, "source")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return selectedArchiveRead{}, nil, archive.ErrInvalidQuery
	}
	selected := selectedArchiveRead{reader: live}
	if source == "historical" {
		reader, err := c.HistoricalReads.ForCredential(ctx, grant)
		if err != nil {
			return selectedArchiveRead{}, nil, err
		}
		if reader == nil {
			return selectedArchiveRead{}, nil, ErrUnavailable
		}
		metadata := reader.Metadata()
		selected = selectedArchiveRead{reader: reader, metadata: &metadata}
	}
	return selected, encoded, nil
}

// Selection rejects duplicate properties rather than letting JSON's last value
// silently choose authority. The ordinary tool decoder still rejects unknowns.
func uniqueReadArguments(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, archive.ErrInvalidQuery
	}
	args := make(map[string]json.RawMessage)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, archive.ErrInvalidQuery
		}
		key, ok := token.(string)
		if !ok {
			return nil, archive.ErrInvalidQuery
		}
		if _, exists := args[key]; exists {
			return nil, archive.ErrInvalidQuery
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, archive.ErrInvalidQuery
		}
		args[key] = value
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') {
		return nil, archive.ErrInvalidQuery
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return nil, archive.ErrInvalidQuery
	}
	return args, nil
}

func (r selectedArchiveRead) validID(id string) bool {
	maximum := 128
	if r.metadata != nil {
		maximum = archive.HistoricalHandleMaxBytes
	} else if archive.IsHistoricalHandle(id) {
		return false
	}
	return id != "" && len(id) <= maximum
}
func (r selectedArchiveRead) validCursor(cursor string) bool {
	maximum := 2048
	if r.metadata != nil {
		maximum = archive.HistoricalCursorMaxBytes
	} else if archive.IsHistoricalHandle(cursor) {
		return false
	}
	return len(cursor) <= maximum
}

// The outer historical envelope has explicit larger limits. Conditional live
// limits remain the original bounds even when the source selector is enabled.
func historicalSourceSchema(base string) string {
	var schema map[string]any
	if json.Unmarshal([]byte(base), &schema) != nil {
		panic("invalid static MCP read schema")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		properties = make(map[string]any)
		schema["properties"] = properties
	}
	properties["source"] = map[string]any{"type": "string", "enum": []string{"live", "historical"}}
	live, historical := make(map[string]any), make(map[string]any)
	for _, bound := range []struct {
		name             string
		historical, live int
	}{{"message_id", archive.HistoricalHandleMaxBytes, 128}, {"cursor", archive.HistoricalCursorMaxBytes, 2048}} {
		if property, ok := properties[bound.name].(map[string]any); ok {
			property["maxLength"] = bound.historical
			live[bound.name] = map[string]any{"maxLength": bound.live}
			historical[bound.name] = map[string]any{"maxLength": bound.historical}
		}
	}
	if property, ok := properties["chat_ids"].(map[string]any); ok {
		items := property["items"].(map[string]any)
		items["maxLength"] = archive.HistoricalHandleMaxBytes
		live["chat_ids"] = map[string]any{"items": map[string]any{"maxLength": 128}}
		historical["chat_ids"] = map[string]any{"items": map[string]any{"maxLength": archive.HistoricalHandleMaxBytes}}
	}
	if len(live) > 0 {
		schema["allOf"] = []any{map[string]any{
			"if":   map[string]any{"required": []string{"source"}, "properties": map[string]any{"source": map[string]any{"const": "historical"}}},
			"then": map[string]any{"properties": historical}, "else": map[string]any{"properties": live},
		}}
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("invalid static MCP read schema")
	}
	return string(encoded)
}

func withHistoricalMetadata(value any, metadata archive.HistoricalViewMetadata) (any, error) {
	label := func(current contentLabel) contentLabel {
		current.ArchiveView = &metadata
		current.ArchiveState = "historical_snapshot"
		current.CurrentPhoneState = "unknown"
		current.CoverageWarning = "Captured-time historical archive; read-only and partial. Current phone state is unknown. Current permissions apply; post-capture phone edits and deletions may not be reflected."
		return current
	}
	switch out := value.(type) {
	case searchDTO:
		out.contentLabel = label(out.contentLabel)
		return out, nil
	case contextDTO:
		out.contentLabel = label(out.contentLabel)
		return out, nil
	case chatPageDTO:
		out.contentLabel = label(out.contentLabel)
		return out, nil
	case statusDTO:
		out.contentLabel = label(out.contentLabel)
		out.Rebuild = nil
		out.AvailableSources = nil
		out.HistoricalSnapshot = &archive.HistoricalSnapshot{CapturedAt: metadata.CapturedAt, ReadOnly: true, PhoneState: "unknown"}
		out.Connection = "historical_snapshot"
		out.Paused = true
		out.Coverage = "partial"
		out.Keyword.Pending = 0
		out.Semantic = progressDTO{State: "unavailable", Code: "historical_snapshot_read_only"}
		out.Media = progressDTO{State: "unavailable", Code: "historical_snapshot_read_only"}
		return out, nil
	default:
		return nil, ErrUnavailable
	}
}

func historicalSourceInstructions(c Config) string {
	if c.HistoricalReads == nil {
		return ""
	}
	captured := c.HistoricalReads.Metadata().CapturedAt.Format(time.RFC3339)
	return strings.Join([]string{
		"Live is the default archive source. Sending, when enabled, uses the live source and current owner-approved permissions.",
		"The six message/chat/index read tools optionally accept source=historical for the captured archive from " + captured + " under current live permissions.",
		"Historical content is read-only, partial and captured-time; current phone state is unknown. Its source-bound IDs cannot be used for live sends or downloads. No sources are merged.",
	}, " ")
}
