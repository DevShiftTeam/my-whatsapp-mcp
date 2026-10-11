package ingestion

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
)

type documentRedeliveryStore struct {
	*voiceRedeliveryStore
	selected []archive.MessageID
	items    []archive.DocumentRedeliverySource
}

func (s *documentRedeliveryStore) DocumentRedeliverySources(_ context.Context, scope archive.ReadScope, ids []archive.MessageID) ([]archive.DocumentRedeliverySource, error) {
	s.selectionCalls++
	s.selected = append([]archive.MessageID(nil), ids...)
	if scope.Principal != "owner" || scope.Account != "100@s.whatsapp.net" {
		return nil, archive.ErrUnauthorized
	}
	return append([]archive.DocumentRedeliverySource(nil), s.items...), nil
}

func documentRedeliveryFixture(t *testing.T) (*Runtime, *documentRedeliveryStore, documentRedeliveryRuntimePort, archive.ReadScope) {
	t.Helper()
	r, voiceStore, _, scope := voiceRedeliveryFixture(t)
	source := voiceStore.items[0]
	s := &documentRedeliveryStore{voiceRedeliveryStore: voiceStore, items: []archive.DocumentRedeliverySource{{Key: source.Key, Source: source.Source, OriginalSentAt: source.OriginalSentAt}}}
	r.store = s
	port, ok := any(r).(documentRedeliveryRuntimePort)
	if !ok {
		t.Fatal("runtime lacks targeted owner document redelivery")
	}
	return r, s, port, scope
}

func TestDocumentRedeliveryRequestsOnlySelectedOpaqueSource(t *testing.T) {
	r, s, port, scope := documentRedeliveryFixture(t)
	ids := []archive.MessageID{s.items[0].Source.Version.MessageID}
	sends := 0
	r.sendHistoryRequest = func(ctx context.Context, message *waE2E.Message) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2100*time.Millisecond {
			t.Fatal("document phone-copy attempt exceeded its bounded context")
		}
		keys := message.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(keys) != 1 || keys[0].GetMessageKey().GetID() != s.items[0].Key.Message || keys[0].GetMessageKey().GetRemoteJID() != s.items[0].Key.Chat || !keys[0].GetMessageKey().GetFromMe() {
			t.Fatal("document retry replaced the selected exact source key")
		}
		sends++
		return nil
	}
	requested, err := port.RequestDocumentRedelivery(context.Background(), scope, ids)
	if err != nil || requested != 1 || sends != 1 || !reflect.DeepEqual(s.selected, ids) {
		t.Fatal("selected document copy was not requested", err)
	}
	if len(s.events) != 0 || len(s.aliases) != 0 || len(s.mediaUpdates) != 0 {
		t.Fatal("peer acknowledgement manufactured a document import or download")
	}
	if count, err := r.RequestVoiceRedelivery(context.Background(), scope); !errors.Is(err, ErrMediaLimit) || count != 0 || sends != 1 {
		t.Fatal("document request did not share the bounded explicit phone-copy cooldown")
	}
}

func TestDocumentRedeliveryRejectsInvalidSelectionsBeforeReadingSources(t *testing.T) {
	for _, ids := range [][]archive.MessageID{nil, {""}, {"a", "a"}, {archive.MessageID(strings.Repeat("a", 129))}, {"a\x00b"}, {archive.MessageID(string([]byte{0xff}))}, {"a", "b", "c", "d", "e", "f", "g", "h", "i"}} {
		r, s, port, scope := documentRedeliveryFixture(t)
		sends := 0
		r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
		count, err := port.RequestDocumentRedelivery(context.Background(), scope, ids)
		if !errors.Is(err, ErrMediaInvalidAdmission) || count != 0 || sends != 0 || s.selectionCalls != 0 {
			t.Fatal("invalid opaque target selection reached archive or peer transport")
		}
	}
}

func TestDocumentRedeliveryRejectsSelectionSubstitutionBeforeAnyPeerRequest(t *testing.T) {
	for _, scenario := range []string{"different_id", "extra_source", "duplicate_source"} {
		t.Run(scenario, func(t *testing.T) {
			r, s, port, scope := documentRedeliveryFixture(t)
			ids := []archive.MessageID{s.items[0].Source.Version.MessageID}
			switch scenario {
			case "different_id":
				s.items[0].Source.Version.MessageID = "unselected-document"
			case "extra_source":
				other := s.items[0]
				other.Source.Version.MessageID = "unselected-document"
				other.Key.Message = "another-source"
				s.items = append(s.items, other)
			case "duplicate_source":
				ids = append(ids, "another-document")
				s.items = append(s.items, s.items[0])
			}
			sends := 0
			r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
			count, err := port.RequestDocumentRedelivery(context.Background(), scope, ids)
			if !errors.Is(err, ErrMediaInvalidAdmission) || count != 0 || sends != 0 {
				t.Fatal("archive selection substitution escaped exact-target admission")
			}
		})
	}
}

func TestDocumentRedeliveryRechecksCurrentAuthority(t *testing.T) {
	for _, scenario := range []string{"principal", "scope_account", "source_account", "deleted", "excluded", "revision", "hash", "stale_source", "stale_metadata", "date", "policy", "expiry", "disconnected", "paused", "unsafe", "admission", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			r, s, port, scope := documentRedeliveryFixture(t)
			item := &s.items[0]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "principal":
				scope.Principal = "mcp-client"
			case "scope_account":
				scope.Account = "other-account"
			case "source_account":
				item.Source.Account = "other-account"
			case "deleted":
				item.Source.Version.Deleted = true
			case "excluded":
				item.Source.Version.Excluded = true
			case "revision":
				item.Source.Version.Revision = 0
			case "hash":
				item.Source.ContentHash = strings.Repeat("z", 64)
			case "stale_source":
				ref := s.current[item.Key]
				ref.Version.Revision++
				s.current[item.Key] = ref
			case "stale_metadata":
				metadata := s.metadata[item.Key]
				metadata.Version.Revision++
				s.metadata[item.Key] = metadata
			case "date":
				item.OriginalSentAt = item.OriginalSentAt.Add(-time.Hour)
			case "policy":
				s.policy.Generation++
			case "expiry":
				item.Source.Version.ExpiresAt = time.Now().Add(-time.Second)
			case "disconnected":
				r.status.Connection = "disconnected"
			case "paused":
				r.status.Paused = true
			case "unsafe":
				r.status.ReconciliationRequired = true
			case "admission":
				r.admissions = 1
			case "canceled":
				cancel()
			}
			sends := 0
			r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sends++; return nil }
			count, _ := port.RequestDocumentRedelivery(ctx, scope, []archive.MessageID{item.Source.Version.MessageID})
			if count != 0 || sends != 0 || len(s.events) != 0 {
				t.Fatal("document phone copy escaped current source, owner, or lifecycle guards")
			}
		})
	}
}
