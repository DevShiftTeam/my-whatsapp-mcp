package ingestion

import (
	"context"
	"sync"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type cachedNameContacts struct {
	wmstore.NoopStore
	mu         sync.Mutex
	names      map[types.JID]types.ContactInfo
	lookups    []types.JID
	allLookups int
	lookup     func(context.Context, types.JID) (types.ContactInfo, error)
}

func (s *cachedNameContacts) GetContact(ctx context.Context, jid types.JID) (types.ContactInfo, error) {
	if err := ctx.Err(); err != nil {
		return types.ContactInfo{}, err
	}
	s.mu.Lock()
	s.lookups = append(s.lookups, jid)
	name, lookup := s.names[jid], s.lookup
	s.mu.Unlock()
	if lookup != nil {
		return lookup(ctx, jid)
	}
	return name, nil
}

func (s *cachedNameContacts) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	s.mu.Lock()
	s.allLookups++
	s.mu.Unlock()
	return nil, archive.ErrInvalidEvent
}

type cachedNameLIDs struct {
	wmstore.NoopStore
	toPN, toLID map[types.JID]types.JID
}

func (s *cachedNameLIDs) GetPNForLID(ctx context.Context, jid types.JID) (types.JID, error) {
	if err := ctx.Err(); err != nil {
		return types.EmptyJID, err
	}
	return s.toPN[jid], nil
}

func (s *cachedNameLIDs) GetLIDForPN(ctx context.Context, jid types.JID) (types.JID, error) {
	if err := ctx.Err(); err != nil {
		return types.EmptyJID, err
	}
	return s.toLID[jid], nil
}

type cachedNameCanonicalStore struct {
	*recordingStore
	canonical map[string]string
}

func (s *cachedNameCanonicalStore) Canonical(_ context.Context, _ string, source string) (string, error) {
	if canonical, ok := s.canonical[source]; ok {
		return canonical, nil
	}
	return source, nil
}

func TestSourceNamesOutgoingUsesRecipientContact(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	owner, recipient := *r.client.Store.ID, types.NewJID("200", types.DefaultUserServer)
	r.client.Store.Contacts = &cachedNameContacts{names: map[types.JID]types.ContactInfo{
		owner:     {Found: true, FullName: "Archive owner"},
		recipient: {Found: true, FullName: "Lior Bodokh", PushName: "Recipient push name"},
	}}
	event := liveMessage("outgoing-name", "synthetic message")
	event.Info.IsFromMe, event.Info.Sender, event.Info.PushName = true, owner, "Owner push name"
	if err := r.message(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.names) != 1 || s.names[0].ChatName != "Lior Bodokh" || s.names[0].SenderName != "Archive owner" {
		t.Fatal("outgoing recipient and sender labels were not resolved independently")
	}
	if len(s.events) != 1 || s.events[0].Key.Chat != recipient.String() || s.events[0].Key.Sender != owner.String() || len(s.aliases) != 0 {
		t.Fatal("contact labels changed authenticated source identity")
	}
}

func TestSourceNamesOutgoingNeverUsesOwnerPushNameForChat(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	event := liveMessage("outgoing-no-contact", "synthetic message")
	event.Info.IsFromMe, event.Info.Sender, event.Info.PushName = true, *r.client.Store.ID, "Owner push name"
	if err := r.message(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.names) != 1 || s.names[0].ChatName != "" || s.names[0].SenderName != "Owner push name" {
		t.Fatal("owner push name was used as the outgoing recipient label")
	}
}

func TestSourceNamesSavedPNNameBeatsLIDPushName(t *testing.T) {
	for _, outgoing := range []bool{false, true} {
		t.Run(map[bool]string{false: "incoming", true: "outgoing"}[outgoing], func(t *testing.T) {
			base := fixtureStore()
			r := testRuntime(base)
			pn, lid := types.NewJID("200", types.DefaultUserServer), types.NewJID("900", types.HiddenUserServer)
			r.store = &cachedNameCanonicalStore{recordingStore: base, canonical: map[string]string{lid.String(): pn.String()}}
			r.client.Store.LIDs = &cachedNameLIDs{toPN: map[types.JID]types.JID{lid: pn}, toLID: map[types.JID]types.JID{pn: lid}}
			r.client.Store.Contacts = &cachedNameContacts{names: map[types.JID]types.ContactInfo{
				pn:  {Found: true, FullName: "ליאור בודוך"},
				lid: {Found: true, PushName: "Public push name"},
			}}
			event := liveMessage("mapped-name", "synthetic message")
			event.Info.Chat, event.Info.Sender, event.Info.PushName = lid, lid, "Event push name"
			key := archive.SourceKey{Account: r.SourceAccount(), Chat: pn.String(), Sender: pn.String()}
			if outgoing {
				event.Info.IsFromMe, event.Info.Sender = true, *r.client.Store.ID
				key.Sender = r.SourceAccount()
			}
			if err := r.sourceNames(context.Background(), key, event); err != nil {
				t.Fatal(err)
			}
			if len(base.names) != 1 || base.names[0].ChatName != "ליאור בודוך" || !outgoing && base.names[0].SenderName != "ליאור בודוך" {
				t.Fatal("verified PN saved name was hidden by a lower-priority LID push name")
			}
			if len(base.aliases) != 0 {
				t.Fatal("label fallback created an identity alias")
			}
		})
	}
}

func TestSourceNamesRejectsConflictingCachedMapping(t *testing.T) {
	base := fixtureStore()
	r := testRuntime(base)
	pn, lid := types.NewJID("200", types.DefaultUserServer), types.NewJID("900", types.HiddenUserServer)
	r.client.Store.LIDs = &cachedNameLIDs{toPN: map[types.JID]types.JID{lid: pn}, toLID: map[types.JID]types.JID{pn: lid}}
	r.client.Store.Contacts = &cachedNameContacts{names: map[types.JID]types.ContactInfo{pn: {Found: true, FullName: "Wrong cached identity"}}}
	event := liveMessage("conflicting-name", "synthetic message")
	event.Info.Chat, event.Info.Sender, event.Info.PushName = lid, lid, "Authenticated event name"
	key := archive.SourceKey{Account: r.SourceAccount(), Chat: lid.String(), Sender: lid.String()}
	if err := r.sourceNames(context.Background(), key, event); err != nil {
		t.Fatal(err)
	}
	if len(base.names) != 1 || base.names[0].ChatName != "Authenticated event name" || len(base.aliases) != 0 {
		t.Fatal("uncommitted PN/LID mapping supplied a label or changed identity")
	}
}

func TestSourceNamesGroupKeepsSenderSeparate(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	event := liveMessage("group-name", "synthetic message")
	event.Info.IsGroup, event.Info.Chat = true, types.NewJID("123-456", types.GroupServer)
	contacts := &cachedNameContacts{names: map[types.JID]types.ContactInfo{
		event.Info.Sender: {Found: true, FullName: "Group member"},
		event.Info.Chat:   {Found: true, FullName: "Not a contact label"},
	}}
	r.client.Store.Contacts = contacts
	if err := r.message(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(s.names) != 1 || s.names[0].ChatName != "" || s.names[0].SenderName != "Group member" {
		t.Fatal("group member label was used as the group title")
	}
	for _, lookup := range contacts.lookups {
		if lookup == event.Info.Chat {
			t.Fatal("group JID was looked up as a phonebook contact")
		}
	}
}
