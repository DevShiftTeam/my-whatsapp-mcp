package ingestion

import (
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"strings"
	"unicode"
	"unicode/utf8"
)

func undeclaredHistoryStub(web *waWeb.WebMessageInfo) bool {
	if web == nil || web.MessageStubType == nil || int32(web.GetMessageStubType()) <= 0 {
		return false
	}
	_, known := waWeb.WebMessageInfo_StubType_name[int32(web.GetMessageStubType())]
	return !known
}

// A present claim must already be a canonical wire JID. ParseJID and ToNonAD
// are intentionally permissive; normalization must not rescue malformed claims.
func partialHistoryJID(raw string, allowGroup bool) bool {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) || strings.Count(raw, "@") != 1 || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" || jid.String() != raw || jid.ToNonAD() != jid {
		return false
	}
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer || (allowGroup && jid.Server == types.GroupServer)
}
