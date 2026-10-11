package ingestion

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types/events"
	"strings"
	"unicode/utf8"
)

func boundedName(name string) string {
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return ""
	}
	if len(name) > 256 {
		name = name[:256]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return name
}
func (r *Runtime) sourceNames(ctx context.Context, key archive.SourceKey, event *events.Message) error {
	senderName := boundedName(event.Info.PushName)
	chatName := ""
	if !event.Info.IsGroup && !event.Info.IsFromMe {
		chatName = senderName
	}
	// Protected connector contacts supply labels only. They do not determine keys
	// or aliases and an absent contact never becomes a public phone/JID fallback.
	if r.client.Store.Contacts != nil {
		contact, err := r.client.Store.Contacts.GetContact(ctx, event.Info.Sender.ToNonAD())
		if err == nil {
			if name := boundedName(contact.FullName); name != "" {
				senderName = name
			} else if name := boundedName(contact.FirstName); name != "" {
				senderName = name
			}
			if !event.Info.IsGroup && !event.Info.IsFromMe {
				chatName = senderName
			}
		}
	}
	if senderName == "" && chatName == "" {
		return nil
	}
	return r.store.SetSourceNames(ctx, archive.SourceNames{Account: key.Account, Chat: key.Chat, Sender: key.Sender, ChatName: chatName, SenderName: senderName})
}
