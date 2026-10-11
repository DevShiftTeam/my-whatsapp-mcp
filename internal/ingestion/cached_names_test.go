package ingestion

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type existingNameStore struct {
	*recordingStore
	nameMu                    sync.Mutex
	targets                   []archive.ExistingNameTarget
	labels                    map[string]string
	canonical                 map[string]string
	queries, updates, maximum int
	queryErr                  error
}

func (s *existingNameStore) Canonical(_ context.Context, _ string, source string) (string, error) {
	if canonical, ok := s.canonical[source]; ok {
		return canonical, nil
	}
	return source, nil
}

func (s *existingNameStore) ExistingNameTargets(ctx context.Context, account, after string, limit int) ([]archive.ExistingNameTarget, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	s.queries++
	if account != "100@s.whatsapp.net" || limit < 1 || limit > 64 {
		return nil, "", archive.ErrInvalidEvent
	}
	if s.queryErr != nil {
		return nil, "", s.queryErr
	}
	var page []archive.ExistingNameTarget
	for _, target := range s.targets {
		if target.Key <= after {
			continue
		}
		if len(page) == limit {
			return page, page[len(page)-1].Key, nil
		}
		page = append(page, target)
	}
	return page, "", nil
}

func (s *existingNameStore) RefreshExistingSourceNames(ctx context.Context, names []archive.SourceNames) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if len(names) > 64 {
		return archive.ErrInvalidEvent
	}
	s.updates++
	if len(names) > s.maximum {
		s.maximum = len(names)
	}
	for _, name := range names {
		if name.Account != "100@s.whatsapp.net" {
			return archive.ErrInvalidEvent
		}
		if name.ChatName != "" {
			key := "c:" + name.Chat
			if _, exists := s.labels[key]; !exists {
				return archive.ErrInvalidEvent
			}
			s.labels[key] = name.ChatName
		}
		if name.SenderName != "" {
			key := "i:" + name.Sender
			if _, exists := s.labels[key]; !exists {
				return archive.ErrInvalidEvent
			}
			s.labels[key] = name.SenderName
		}
	}
	return nil
}

func nameRefreshFixture(t *testing.T) (*Runtime, *existingNameStore, *cachedNameContacts) {
	t.Helper()
	base := fixtureStore()
	r := testRuntime(base)
	s := &existingNameStore{recordingStore: base, labels: map[string]string{}, canonical: map[string]string{}}
	contacts := &cachedNameContacts{names: map[types.JID]types.ContactInfo{}}
	r.store, r.client.Store.Contacts = s, contacts
	t.Cleanup(func() { _ = r.Close() })
	return r, s, contacts
}

func addExistingNameTarget(s *existingNameStore, key, source, name string, identity bool) {
	target := archive.ExistingNameTarget{Key: key, Name: name}
	if identity {
		target.Sender = source
		s.labels["i:"+source] = name
	} else {
		target.Chat = source
		s.labels["c:"+source] = name
	}
	s.targets = append(s.targets, target)
}

func TestCachedNamesRefreshExistingRowsOnConnect(t *testing.T) {
	r, s, contacts := nameRefreshFixture(t)
	pn, lid := types.NewJID("200", types.DefaultUserServer), types.NewJID("900", types.HiddenUserServer)
	group := types.NewJID("123-456", types.GroupServer)
	addExistingNameTarget(s, "c:0001", pn.String(), "Old public name", false)
	addExistingNameTarget(s, "c:0002", group.String(), "Group subject", false)
	addExistingNameTarget(s, "i:0001", pn.String(), "Old sender name", true)
	s.canonical[lid.String()] = pn.String()
	r.client.Store.LIDs = &cachedNameLIDs{toPN: map[types.JID]types.JID{lid: pn}, toLID: map[types.JID]types.JID{pn: lid}}
	contacts.names[lid] = types.ContactInfo{Found: true, FullName: "ליאור בודוך"}
	contacts.names[group] = types.ContactInfo{Found: true, FullName: "Not a group name"}
	unseen := types.NewJID("777", types.DefaultUserServer)
	contacts.names[unseen] = types.ContactInfo{Found: true, FullName: "Unseen contact"}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		s.nameMu.Lock()
		defer s.nameMu.Unlock()
		return s.labels["c:"+pn.String()] == "ליאור בודוך" && s.labels["i:"+pn.String()] == "ליאור בודוך"
	})
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.nameMu.Lock()
	if len(s.labels) != 3 || s.labels["c:"+group.String()] != "Group subject" || s.maximum > 64 {
		t.Fatal("cached names created a contact or overwrote a group subject")
	}
	s.nameMu.Unlock()
	contacts.mu.Lock()
	defer contacts.mu.Unlock()
	if contacts.allLookups != 0 {
		t.Fatal("refresh enumerated the private phonebook")
	}
	for _, lookedUp := range contacts.lookups {
		if lookedUp == unseen || lookedUp == group {
			t.Fatal("refresh looked up an unarchived contact or group as a contact")
		}
	}
	if len(s.names) != 0 || len(s.aliases) != 0 || len(s.unsafe) != 0 {
		t.Fatal("optional labels created source rows, identities, or source failure")
	}
}

func TestCachedNamesRefreshUsesBoundedPages(t *testing.T) {
	r, s, contacts := nameRefreshFixture(t)
	for i := 0; i < 65; i++ {
		jid := types.NewJID(fmt.Sprintf("%d", 200+i), types.DefaultUserServer)
		addExistingNameTarget(s, fmt.Sprintf("c:%04d", i), jid.String(), "Old name", false)
		contacts.names[jid] = types.ContactInfo{Found: true, FullName: fmt.Sprintf("Saved contact %d", i)}
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		s.nameMu.Lock()
		defer s.nameMu.Unlock()
		return s.labels["c:264@s.whatsapp.net"] == "Saved contact 64"
	})
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if s.queries != 2 || s.updates != 2 || s.maximum != 64 || len(s.labels) != 65 {
		t.Fatal("name refresh did not finish in bounded keyset pages")
	}
}

func TestCachedNamesContactEventRefreshesWithoutSourceAdmission(t *testing.T) {
	r, s, contacts := nameRefreshFixture(t)
	jid := types.NewJID("200", types.DefaultUserServer)
	addExistingNameTarget(s, "c:0001", jid.String(), "Old name", false)
	contacts.names[jid] = types.ContactInfo{Found: true, FullName: "Saved name"}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		s.nameMu.Lock()
		defer s.nameMu.Unlock()
		return s.labels["c:"+jid.String()] == "Saved name"
	})
	contacts.mu.Lock()
	contacts.names[jid] = types.ContactInfo{Found: true, FullName: "Updated name"}
	contacts.mu.Unlock()
	r.deliver(&events.Contact{JID: jid})
	waitOptional(t, func() bool {
		s.nameMu.Lock()
		defer s.nameMu.Unlock()
		return s.labels["c:"+jid.String()] == "Updated name"
	})
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 0 || len(s.unsafe) != 0 || len(s.aliases) != 0 || r.Status().Received != 0 || r.Status().Controls != 0 {
		t.Fatal("optional contact event was treated as source content or authority")
	}
}

func TestCachedNamesLookupFailureDefersWithoutUnsafe(t *testing.T) {
	r, s, contacts := nameRefreshFixture(t)
	jid := types.NewJID("200", types.DefaultUserServer)
	addExistingNameTarget(s, "c:0001", jid.String(), "Old name", false)
	contacts.names[jid] = types.ContactInfo{Found: true, FullName: "Saved name"}
	s.queryErr = errors.New("PRIVATE CACHE ERROR")
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		for _, code := range r.Status().Codes {
			if code == "names_refresh_deferred" {
				return true
			}
		}
		return false
	})
	s.nameMu.Lock()
	s.queryErr = nil
	s.nameMu.Unlock()
	r.deliver(&events.Contact{JID: jid})
	waitOptional(t, func() bool {
		s.nameMu.Lock()
		updated := s.labels["c:"+jid.String()] == "Saved name"
		s.nameMu.Unlock()
		for _, code := range r.Status().Codes {
			if code == "names_refresh_deferred" {
				return false
			}
		}
		return updated
	})
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.unsafe) != 0 || r.Status().ReconciliationRequired {
		t.Fatal("optional name failure quarantined the archive")
	}
	for _, code := range r.Status().Codes {
		if code == "names_refresh_deferred" || code == "PRIVATE CACHE ERROR" {
			t.Fatal("name refresh did not clear its finite retry diagnostic")
		}
	}
}

func TestCachedNamesStopCancelsAndJoinsLookup(t *testing.T) {
	r, s, contacts := nameRefreshFixture(t)
	jid := types.NewJID("200", types.DefaultUserServer)
	addExistingNameTarget(s, "c:0001", jid.String(), "Old name", false)
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var once sync.Once
	contacts.lookup = func(ctx context.Context, _ types.JID) (types.ContactInfo, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		close(cancelled)
		return types.ContactInfo{}, ctx.Err()
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("optional cache lookup did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("stop returned before the owned name lookup cancelled")
	}
	if !r.Quiesced() || len(s.unsafe) != 0 || s.updates != 0 {
		t.Fatal("cancelled labels escaped their joined optional lifetime")
	}
}

func TestCachedNamesWakePreservesQueuedBackfill(t *testing.T) {
	r, _, _ := nameRefreshFixture(t)
	n := &optionalNetwork{commands: make(chan optionalCommand, 1), accepting: true}
	r.optionalMu.Lock()
	r.optional = n
	r.optionalMu.Unlock()
	defer func() { r.optionalMu.Lock(); r.optional = nil; r.optionalMu.Unlock() }()
	selection := optionalCommand{request: BackfillRequest{PolicyGeneration: 1, AllSelected: true, Limit: 32}}
	n.commands <- selection
	for i := 0; i < 100; i++ {
		r.wakeSourceNames()
	}
	if len(n.commands) != 1 || !n.namesPending {
		t.Fatal("name wake did not stay coalesced beside the queued selection")
	}
	queued := <-n.commands
	if queued.wake || !queued.request.AllSelected || queued.request.PolicyGeneration != 1 {
		t.Fatal("name wake replaced a queued backfill selection")
	}
}
