package ingestion

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The SDK BuildRevoke/BuildMessageKey contract identifies another group member
// through MessageKey.Participant. The authenticated admin is the actor, not the
// original author. Only a full target key permits terminal deletion here.
func (r *Runtime) trustedAdminRevoke(ctx context.Context, e *events.Message, envelope archive.SourceKey) (archive.SourceEvent, *waE2E.Message, error) {
	fail := func() (archive.SourceEvent, *waE2E.Message, error) {
		r.mutationMetadataMissing("group_admin_control", e)
		return archive.SourceEvent{}, nil, archive.ErrMutationMetadataMissing
	}
	p := mutationProtocol(e)
	if e.Info.Edit != types.EditAttributeAdminRevoke || p == nil || p.GetType() != waE2E.ProtocolMessage_REVOKE || p.GetKey() == nil {
		return fail()
	}
	target := p.GetKey()
	if target.GetID() == "" || witnessIdentifier(target.GetID()) != target.GetID() || target.GetRemoteJID() == "" || target.GetParticipant() == "" || target.FromMe == nil {
		return fail()
	}
	chat, err := r.canonical(ctx, target.GetRemoteJID())
	if err != nil || chat != envelope.Chat {
		return fail()
	}
	author, err := r.historyUserClaim(ctx, target.GetParticipant())
	if err != nil {
		return fail()
	}
	// FromMe is relative to the control author, never to this archive's account.
	if target.GetFromMe() && author != envelope.Sender {
		return fail()
	}
	own, err := r.canonical(ctx, envelope.Account)
	if err != nil {
		return archive.SourceEvent{}, nil, err
	}
	key := archive.SourceKey{Account: envelope.Account, Chat: chat, Sender: author, Message: target.GetID(), FromMe: author == own}
	out := archive.SourceEvent{Key: key, Kind: archive.EventDelete, SourceRevision: e.Info.Timestamp.UnixMilli(), BodyKind: archive.MessageUnsupported}
	if e.Info.Timestamp.IsZero() {
		out.SourceRevision = 0
	}
	// An exact authenticated key can leave a tombstone even when no body exists.
	targets, err := r.store.MutationTargets(ctx, key.Account, key.Chat, key.Message)
	if err != nil {
		return out, nil, err
	}
	for _, m := range targets {
		if m.Key == key {
			out.OriginalSentAt = m.OriginalSentAt
			out.OriginalDateKnown = m.OriginalDateKnown
			out.ExpiresAt = m.Version.ExpiresAt
		}
	}
	return out, nil, nil
}
