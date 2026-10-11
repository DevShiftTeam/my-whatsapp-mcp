package archive

import (
	"context"
	"database/sql"
)

// documentPromotionEligible is the only exception to the same-protocol source
// conflict rule. Migration 014 records an unchanged, empty legacy unsupported
// original. A fresh authenticated document must reconstruct that exact old
// hash, including its original reply target, before its local revision changes.
func documentPromotionEligible(ctx context.Context, tx *sql.Tx, current sourceRow, event SourceEvent, dateMatches bool) (bool, error) {
	if !dateMatches || event.Kind != EventOriginal || event.BodyKind != MessageDocument || event.Body != "" || len(event.Media) != 1 || event.Media[0].Kind != "document" || event.Media[0].ViewOnce || current.state != "active" || current.kind != string(EventOriginal) || current.viewOnce || current.barrier != "" {
		return false, nil
	}
	legacy := event
	legacy.Body, legacy.Caption = "", ""
	legacy.BodyKind = MessageUnsupported
	legacy.Media = nil
	if hashEvent(legacy) != current.hash {
		return false, nil
	}
	emptyIdentity, err := sourceMediaIdentity(nil)
	if err != nil {
		return false, err
	}
	if current.mediaIdentity != "" && current.mediaIdentity != emptyIdentity {
		return false, nil
	}
	reply := SourceKey{}
	if event.ReplyTo != nil {
		if event.ReplyTo.Account != event.Key.Account {
			return false, nil
		}
		reply, err = canonicalKey(ctx, tx, *event.ReplyTo)
		if err != nil {
			return false, err
		}
	}
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN extension_document_promotions p ON p.message_id=m.id AND p.account=m.account
 WHERE m.id=? AND m.account=? AND m.state='active' AND m.source_kind='original' AND m.body_kind='unsupported'
 AND m.body='' AND m.caption='' AND m.normalized_body='' AND m.normalized_caption='' AND m.edited=0 AND m.view_once=0 AND m.revision_barrier=''
 AND m.original_date_known=1 AND m.local_revision=? AND m.protocol_revision=? AND m.content_hash=? AND m.extension_media_identity=?
 AND p.local_revision=m.local_revision AND p.protocol_revision=m.protocol_revision AND p.content_hash=m.content_hash AND p.media_identity=m.extension_media_identity
 AND m.reply_account=? AND m.reply_chat=? AND m.reply_sender=? AND m.reply_message=? AND m.reply_from_me=?
 AND NOT EXISTS(SELECT 1 FROM extension_media_associations a WHERE a.message_id=m.id))`, current.id, event.Key.Account, current.local, current.protocol, current.hash, current.mediaIdentity, reply.Account, reply.Chat, reply.Sender, reply.Message, reply.FromMe).Scan(&eligible)
	return eligible, err
}
