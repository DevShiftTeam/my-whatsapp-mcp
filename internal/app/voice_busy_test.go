//go:build sqlite_fts5

package app

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestVoiceBusyProgressPreservesPendingAndCompletedCounts(t *testing.T) {
	owner := &voiceOwner{code: "transcription_busy"}
	progress := archive.Progress{State: "ready", Pending: 5, Complete: 7}
	owner.progress(&progress)
	if progress.State != "pending" || progress.Code != "transcription_busy" || progress.Pending != 5 || progress.Complete != 7 || progress.Failed != 0 {
		t.Fatalf("busy service changed progress or reported an outage: %#v", progress)
	}
}

func TestVoiceBusyUploadReleasesPinsAndPreservesAttemptBudget(t *testing.T) {
	fixture := newVoiceHTTPFixture(t, true)
	fixture.rejectStatus.Store(http.StatusTooManyRequests)
	app, _ := voiceTestApplication(t, fixture)
	addVoiceTestOriginal(t, app, "busy-upload-race")
	waitVoiceCondition(t, func() bool { return fixture.calls.Load() == 1 })
	fixture.busy.Store(true)
	fileURL := url.URL{Scheme: "file", Path: filepath.Join(app.paths.ArchiveDir, "archive.sqlite"), RawQuery: "mode=ro"}
	readDB, err := sql.Open("sqlite3", fileURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer readDB.Close()
	waitVoiceCondition(t, func() bool {
		var count, attempts, pinned int64
		var state, code sql.NullString
		err := readDB.QueryRow("SELECT count(*),COALESCE(SUM(attempts),0),COALESCE(SUM(input_pinned),0),MIN(state),MIN(last_code) FROM extension_extraction_jobs WHERE account=?", demoAccount).Scan(&count, &attempts, &pinned, &state, &code)
		return err == nil && count == 1 && attempts == 0 && pinned == 0 && state.String == "pending" && code.String == "model_unavailable"
	})
	waitVoiceCondition(t, func() bool {
		status, err := app.Status(context.Background())
		return err == nil && status.Media.Code == "transcription_busy" && status.Media.State == "pending" && status.Media.Failed == 0
	})
	if fixture.calls.Load() != 1 || len(voiceSearch(t, app).Hits) != 0 {
		t.Fatal("busy upload published a transcript or started another upload")
	}
}

func TestVoiceBusyServiceDefersClaimsAndResumes(t *testing.T) {
	fixture := newVoiceHTTPFixture(t, true)
	app, _ := voiceTestApplication(t, fixture)
	waitVoiceCondition(t, func() bool {
		status, err := app.Status(context.Background())
		return err == nil && status.Media.Code == "transcription_ready"
	})
	fixture.busy.Store(true)
	waitVoiceCondition(t, func() bool {
		status, err := app.Status(context.Background())
		return err == nil && (status.Media.Code == "transcription_busy" || status.Media.Code == "transcription_unavailable")
	})
	status, err := app.Status(context.Background())
	if err != nil || status.Media.State != "pending" || status.Media.Code != "transcription_busy" {
		t.Fatalf("busy readiness reported an outage: state=%s code=%s err=%v", status.Media.State, status.Media.Code, err)
	}
	addVoiceTestOriginal(t, app, "busy-service-pending-voice")
	time.Sleep(1200 * time.Millisecond)
	status, err = app.Status(context.Background())
	pinned, pinErr := app.store.ListPinnedExtractions(context.Background(), archive.ExtractionRecoveryRequest{Account: demoAccount, Limit: 64})
	if err != nil || pinErr != nil || status.Media.Pending != 1 || status.Media.Failed != 0 || fixture.calls.Load() != 0 || len(pinned.Items) != 0 {
		t.Fatalf("busy service consumed work: pending=%d failed=%d uploads=%d pins=%d err=%v pin_err=%v", status.Media.Pending, status.Media.Failed, fixture.calls.Load(), len(pinned.Items), err, pinErr)
	}
	if page, err := app.Search(context.Background(), archive.SearchRequest{Query: "Atlas", Limit: 10}); err != nil || len(page.Hits) == 0 {
		t.Fatal("busy transcription blocked source text search")
	}
	fixture.busy.Store(false)
	waitVoiceCondition(t, func() bool { return len(voiceSearch(t, app).Hits) == 1 })
	status, err = app.Status(context.Background())
	if err != nil || status.Media.Code != "transcription_ready" || status.Media.Complete != 1 || status.Media.Failed != 0 || fixture.calls.Load() != 1 {
		t.Fatalf("busy service did not resume: code=%s complete=%d failed=%d uploads=%d err=%v", status.Media.Code, status.Media.Complete, status.Media.Failed, fixture.calls.Load(), err)
	}
}
