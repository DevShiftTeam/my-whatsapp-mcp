package ingestion

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestExactGroupAdminRevokeUsesOriginalAuthor(t *testing.T) {
	for _, author := range []string{"300@s.whatsapp.net", "100@s.whatsapp.net"} {
		r := testRuntime(fixtureStore())
		e := mutation("deleted-target", waE2E.ProtocolMessage_REVOKE)
		e.Info.Chat = types.NewJID("500", types.GroupServer)
		e.Info.IsGroup = true
		e.Info.Edit = types.EditAttributeAdminRevoke
		e.Message.ProtocolMessage.Key.RemoteJID = proto.String(e.Info.Chat.String())
		e.Message.ProtocolMessage.Key.Participant = proto.String(author)
		e.Message.ProtocolMessage.Key.FromMe = proto.Bool(false)
		out, body, err := r.trustedMutation(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil || out.Kind != archive.EventDelete || out.Key.Sender != author || out.Key.FromMe != (author == r.SourceAccount()) || r.Status().ReconciliationRequired {
			t.Fatal("admin actor substituted for original author")
		}
	}
}
func TestIncompleteGroupAdminControlStillFences(t *testing.T) {
	for _, missing := range []string{"participant", "chat", "protocol", "direction"} {
		r := testRuntime(fixtureStore())
		e := mutation("target", waE2E.ProtocolMessage_REVOKE)
		e.Info.Chat = types.NewJID("500", types.GroupServer)
		e.Info.IsGroup = true
		e.Info.Edit = types.EditAttributeAdminRevoke
		k := e.Message.ProtocolMessage.Key
		k.RemoteJID = proto.String(e.Info.Chat.String())
		k.Participant = proto.String("300@s.whatsapp.net")
		k.FromMe = proto.Bool(false)
		switch missing {
		case "participant":
			k.Participant = nil
		case "chat":
			k.RemoteJID = nil
		case "protocol":
			e.Message = &waE2E.Message{Conversation: proto.String("body")}
		case "direction":
			k.FromMe = nil
		}
		if _, _, err := r.trustedMutation(context.Background(), e); err == nil || !r.Status().ReconciliationRequired {
			t.Fatal("ambiguous admin control accepted")
		}
	}
}
