package ingestion

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
	if name, err := r.cachedContactName(ctx, key.Account, event.Info.Sender.ToNonAD()); err == nil && name != "" {
		senderName = name
	}
	if !event.Info.IsGroup {
		// Outgoing authors are the owner. Resolve the recipient independently so
		// the owner's push name can never become the one-to-one chat label.
		if name, err := r.cachedContactName(ctx, key.Account, event.Info.Chat.ToNonAD()); err == nil && name != "" {
			chatName = name
		} else if !event.Info.IsFromMe {
			chatName = senderName
		}
	}
	if senderName == "" && chatName == "" {
		return nil
	}
	return r.store.SetSourceNames(ctx, archive.SourceNames{Account: key.Account, Chat: key.Chat, Sender: key.Sender, ChatName: chatName, SenderName: senderName})
}

func contactJID(jid types.JID) bool {
	return !jid.IsEmpty() && jid == jid.ToNonAD() && (jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer)
}

// Only the protected SDK cache supplies display labels. This never enumerates
// the phonebook, requests network metadata, establishes an alias, or returns a
// phone number/JID as a fallback name.
func (r *Runtime) cachedContactName(ctx context.Context, account string, jid types.JID) (string, error) {
	if !contactJID(jid) || r.client.Store.Contacts == nil {
		return "", nil
	}
	primary, err := r.client.Store.Contacts.GetContact(ctx, jid)
	if err != nil {
		return "", err
	}
	contacts := []types.ContactInfo{primary}
	other, err := r.cachedNameCounterpart(ctx, account, jid)
	if err != nil {
		return "", err
	}
	if !other.IsEmpty() {
		mapped, err := r.client.Store.Contacts.GetContact(ctx, other)
		if err != nil {
			return "", err
		}
		contacts = append(contacts, mapped)
	}
	// A saved PN full name should beat a LID push name, and vice versa.
	for field := 0; field < 4; field++ {
		for _, contact := range contacts {
			if !contact.Found {
				continue
			}
			names := [...]string{contact.FullName, contact.FirstName, contact.PushName, contact.BusinessName}
			if name := boundedName(names[field]); name != "" {
				return name, nil
			}
		}
	}
	return "", nil
}

// A counterpart is usable as label metadata only if the cached mapping agrees
// in both directions and the archive has already committed that same identity.
func (r *Runtime) cachedNameCounterpart(ctx context.Context, account string, jid types.JID) (types.JID, error) {
	if r.client.Store.LIDs == nil || account == "" {
		return types.EmptyJID, nil
	}
	var other, back types.JID
	var err error
	if jid.Server == types.HiddenUserServer {
		other, err = r.client.Store.LIDs.GetPNForLID(ctx, jid)
		if err == nil && contactJID(other) && other.Server == types.DefaultUserServer {
			back, err = r.client.Store.LIDs.GetLIDForPN(ctx, other)
		}
	} else {
		other, err = r.client.Store.LIDs.GetLIDForPN(ctx, jid)
		if err == nil && contactJID(other) && other.Server == types.HiddenUserServer {
			back, err = r.client.Store.LIDs.GetPNForLID(ctx, other)
		}
	}
	if err != nil {
		return types.EmptyJID, err
	}
	if other.IsEmpty() || back != jid || !contactJID(other) {
		return types.EmptyJID, nil
	}
	canonical, err := r.store.Canonical(ctx, account, jid.String())
	if err != nil {
		return types.EmptyJID, err
	}
	mapped, err := r.store.Canonical(ctx, account, other.String())
	if err != nil {
		return types.EmptyJID, err
	}
	if canonical == "" || canonical != mapped {
		return types.EmptyJID, nil
	}
	return other, nil
}
