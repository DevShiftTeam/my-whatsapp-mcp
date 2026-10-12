package archive

import (
	"strings"
	"unicode/utf8"
)

func validHistoricalTag(tag string) bool {
	if len(tag) != 16 {
		return false
	}
	for _, c := range tag {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func (v *HistoricalView) wrapHandle(raw, kind string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !validReadID(raw) || IsHistoricalHandle(raw) {
		return "", ErrSourceUnavailable
	}
	out := "h1." + v.meta.SnapshotID + "." + kind + "." + raw
	if len(out) > HistoricalHandleMaxBytes {
		return "", ErrLimitExceeded
	}
	return out, nil
}

func (v *HistoricalView) unwrapHandle(handle, kind string) (string, error) {
	if len(handle) > HistoricalHandleMaxBytes || !utf8.ValidString(handle) || strings.ContainsRune(handle, 0) {
		return "", ErrInvalidQuery
	}
	parts := strings.SplitN(handle, ".", 4)
	if len(parts) != 4 || parts[0] != "h1" || parts[1] != v.meta.SnapshotID || parts[2] != kind || !validReadID(parts[3]) || IsHistoricalHandle(parts[3]) {
		return "", ErrInvalidQuery
	}
	return parts[3], nil
}

func (v *HistoricalView) wrapCursor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !validString(raw, 2048) || IsHistoricalHandle(raw) {
		return "", ErrCursorStale
	}
	out := "h1." + v.meta.SnapshotID + ".p." + raw
	if len(out) > HistoricalCursorMaxBytes {
		return "", ErrLimitExceeded
	}
	return out, nil
}

func (v *HistoricalView) unwrapCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > HistoricalCursorMaxBytes || !utf8.ValidString(cursor) || strings.ContainsRune(cursor, 0) {
		return "", ErrCursorStale
	}
	parts := strings.SplitN(cursor, ".", 4)
	if len(parts) != 4 || parts[0] != "h1" || parts[1] != v.meta.SnapshotID || parts[2] != "p" || !validString(parts[3], 2048) || IsHistoricalHandle(parts[3]) {
		return "", ErrCursorStale
	}
	return parts[3], nil
}

func (v *HistoricalView) wrapSource(source *SourceVersion) error {
	id, err := v.wrapHandle(string(source.MessageID), "m")
	if err == nil {
		source.MessageID = MessageID(id)
	}
	return err
}

func (v *HistoricalView) wrapDerived(derived []DerivedExcerpt) ([]DerivedExcerpt, error) {
	out := append([]DerivedExcerpt(nil), derived...)
	for i := range out {
		id, err := v.wrapHandle(string(out[i].DocumentID), "d")
		if err != nil {
			return nil, err
		}
		out[i].DocumentID = DocumentID(id)
		if err = v.wrapSource(&out[i].Source); err != nil {
			return nil, err
		}
		id, err = v.wrapHandle(string(out[i].Provenance.MediaID), "a")
		if err != nil {
			return nil, err
		}
		out[i].Provenance.MediaID = MediaID(id)
	}
	return out, nil
}

func (v *HistoricalView) wrapSearch(page *SearchPage) error {
	for i := range page.Hits {
		hit := &page.Hits[i]
		id, err := v.wrapHandle(string(hit.MessageID), "m")
		if err != nil {
			return err
		}
		hit.MessageID = MessageID(id)
		id, err = v.wrapHandle(string(hit.ChatID), "c")
		if err != nil {
			return err
		}
		hit.ChatID = ChatID(id)
		id, err = v.wrapHandle(string(hit.DocumentID), "d")
		if err != nil {
			return err
		}
		hit.DocumentID = DocumentID(id)
		// Public copies may share backing arrays with internal provenance.
		hit.Sources = append([]SourceVersion(nil), hit.Sources...)
		for j := range hit.Sources {
			if err = v.wrapSource(&hit.Sources[j]); err != nil {
				return err
			}
		}
		hit.Derived, err = v.wrapDerived(hit.Derived)
		if err != nil {
			return err
		}
	}
	var err error
	page.NextCursor, err = v.wrapCursor(page.NextCursor)
	return err
}

func (v *HistoricalView) wrapContext(page *ContextPage) error {
	for i := range page.Messages {
		message := &page.Messages[i]
		id, err := v.wrapHandle(string(message.ID), "m")
		if err != nil {
			return err
		}
		message.ID = MessageID(id)
		id, err = v.wrapHandle(string(message.ChatID), "c")
		if err != nil {
			return err
		}
		message.ChatID = ChatID(id)
		message.SenderID, err = v.wrapHandle(message.SenderID, "i")
		if err != nil {
			return err
		}
		id, err = v.wrapHandle(string(message.ReplyTo), "m")
		if err != nil {
			return err
		}
		message.ReplyTo = MessageID(id)
		message.Derived, err = v.wrapDerived(message.Derived)
		if err != nil {
			return err
		}
		message.MediaAvailability = "unavailable"
	}
	return nil
}

func (v *HistoricalView) wrapChats(chats []ChatView) error {
	for i := range chats {
		id, err := v.wrapHandle(string(chats[i].ID), "c")
		if err != nil {
			return err
		}
		chats[i].ID, chats[i].Coverage = ChatID(id), "partial"
	}
	return nil
}

func (v *HistoricalView) wrapStats(page *StatsSnapshot) error {
	for i := range page.TopChats {
		id, err := v.wrapHandle(string(page.TopChats[i].ChatID), "c")
		if err != nil {
			return err
		}
		page.TopChats[i].ChatID = ChatID(id)
	}
	kind := "c"
	if page.Breakdown.Dimension == "people" {
		kind = "i"
	}
	for i := range page.Breakdown.Rows {
		id, err := v.wrapHandle(page.Breakdown.Rows[i].ID, kind)
		if err != nil {
			return err
		}
		page.Breakdown.Rows[i].ID = id
	}
	page.Media.RetainedVoice = 0 // historical handles never grant original access
	return nil
}
