package app

import (
	"context"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
	"time"
)

// Synthetic identities never come from a real linked-device session.
const demoAccount = "wa-indexer-synthetic-demo-v1"

func demoEvents() []archive.SourceEvent {
	sent := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	key := func(id string) archive.SourceKey {
		return archive.SourceKey{Account: demoAccount, Chat: "demo-team", Sender: "demo-author", Message: id}
	}
	event := func(id, body string, kind archive.MessageKind, minute int) archive.SourceEvent {
		return archive.SourceEvent{Key: key(id), Kind: archive.EventOriginal, OriginalSentAt: sent.Add(time.Duration(minute) * time.Minute), OriginalDateKnown: true, Body: body, BodyKind: kind}
	}
	meeting := event("meeting", "פגישה ביום שלישי בשעה 10:30 — Project Atlas · שָׁלוֹם · 1,250 · https://example.invalid/demo", archive.MessageText, 0)
	reply := event("reply", "Confirmed. נתראה שם!", archive.MessageText, 1)
	reply.ReplyTo = &meeting.Key
	edited := event("edit", "OLD_EDIT_CANARY", archive.MessageText, 2)
	invalidation := edited
	invalidation.Kind = archive.EventEditInvalidation
	invalidation.Body = ""
	invalidation.SourceRevision = 2
	replacement := edited
	replacement.Kind = archive.EventEditContent
	replacement.Body = "Project Atlas updated: נפגש במשרד החדש"
	replacement.SourceRevision = 2
	deleted := event("delete", "DELETED_CANARY", archive.MessageText, 3)
	tombstone := deleted
	tombstone.Kind = archive.EventDelete
	tombstone.Body = ""
	tombstone.SourceRevision = 3
	image := event("image", "", archive.MessageImage, 4)
	image.Caption = "תמונה מהטיול בחיפה"
	image.Media = []archive.MediaReference{{Kind: "image", MimeType: "image/jpeg", Availability: "unavailable"}}
	voice := event("voice", "", archive.MessageVoice, 5)
	voice.Media = []archive.MediaReference{{Kind: "voice", MimeType: "audio/ogg", Availability: "unavailable"}}
	second := event("other", "Travel planning for ירושלים and Haifa", archive.MessageText, 6)
	second.Key.Chat = "demo-travel"
	old := event("old", "FILTERED_DATE_CANARY", archive.MessageText, 0)
	old.OriginalSentAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	return []archive.SourceEvent{meeting, reply, edited, invalidation, replacement, deleted, tombstone, image, voice, second, old}
}
func (a *Application) seedDemo(ctx context.Context, generation archive.Generation) (result error) {
	observation, err := a.store.BeginObservation(ctx, demoAccount)
	if err != nil {
		return err
	}
	// The owner explicitly admitted this small, finite synthetic replay. A
	// browser navigation must not interrupt an accepted edit/delete control and
	// turn a replayable demo into an unsafe live-observation recovery problem.
	replay, replayCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer replayCancel()
	ctx = replay
	defer func() {
		if result == nil {
			return
		}
		// This finite synthetic input is replayable. Drain admitted work and
		// clean only this owned marker; EndObservation still refuses unsafe data.
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if a.store.Drain(drain) == nil {
			_ = a.store.EndObservation(drain, observation)
		}
	}()
	for _, event := range demoEvents() {
		chatName := "Demo · צוות Atlas"
		if event.Key.Chat == "demo-travel" {
			chatName = "Demo · Travel planning"
		}
		if err = a.store.SetSourceNames(ctx, archive.SourceNames{Account: demoAccount, Chat: event.Key.Chat, Sender: event.Key.Sender, ChatName: chatName, SenderName: "Demo · נועה"}); err != nil {
			return err
		}
		if _, err = a.store.ApplySourceEvent(ctx, event, generation); err != nil {
			return err
		}
	}
	if err = a.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: demoAccount, Connection: "disconnected", CoverageUnknown: true}); err != nil {
		return err
	}
	if err = a.store.Drain(ctx); err != nil {
		return err
	}
	if err = a.store.EndObservation(ctx, observation); err != nil {
		return err
	}
	if a.resources != nil && a.resources.demoAcceptanceEnabled && a.resources.demoAcceptance == nil {
		owner, fixtureErr := prepareDemoAcceptance(ctx, a, generation)
		// A partial fixture owns real reservations and exact cleanup leases.
		// Retain it even on preparation failure until Close/Delete acknowledges it.
		a.resources.demoAcceptance = owner
		if fixtureErr != nil {
			return fixtureErr
		}
	}
	a.stateMu.Lock()
	a.status = ingestion.Status{Connection: "disconnected"}
	a.stateMu.Unlock()
	return nil
}
