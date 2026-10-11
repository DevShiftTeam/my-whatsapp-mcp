package localui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
)

type fakeModelApplication struct {
	*fakeApplication
	profiles  []modelstore.Profile
	state     ModelStateSnapshot
	operation ModelOperation
	started   ModelOperationRequest
	cancelled string
}

func (a *fakeModelApplication) Models(context.Context) ([]modelstore.Profile, error) {
	return a.profiles, a.called("models")
}
func (a *fakeModelApplication) ModelState(context.Context) (ModelStateSnapshot, error) {
	return a.state, a.called("model_state")
}
func (a *fakeModelApplication) StartModelOperation(_ context.Context, input ModelOperationRequest) (ModelOperation, error) {
	a.started = input
	return a.operation, a.called("model_start")
}
func (a *fakeModelApplication) CancelModelOperation(_ context.Context, id string) error {
	a.cancelled = id
	return a.called("model_cancel")
}

func newModelTestServer(t *testing.T) (*Server, *fakeModelApplication) {
	t.Helper()
	a := &fakeModelApplication{fakeApplication: &fakeApplication{}, profiles: []modelstore.Profile{{
		ID: "candidate.profile", Revision: "pinned", Selection: "candidate", GateStatus: "NOT_RUN", Platform: "any", License: "MIT",
		Manifest: "PRIVATE_MANIFEST", Notices: "PRIVATE_NOTICES", RuntimeRequirements: []string{"PRIVATE_RUNTIME"},
		Components: []modelstore.Component{{ID: "model", Filename: "PRIVATE_FILENAME", URL: "https://private.example/PRIVATE_URL", SHA256: "PRIVATE_HASH", Bytes: 100}},
	}}, operation: ModelOperation{ID: "op_1", ProfileID: "candidate.profile", Kind: "install", State: "queued"}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("model owner construction failed")
	}
	return s, a
}

func TestModelManagementUsesOwnerAuthorityForEveryReadAndWrite(t *testing.T) {
	s, app := newModelTestServer(t)
	owner := login(t, s)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/models", ""}, {"GET", "/api/models/state", ""},
		{"POST", "/api/models/operations", `{"profile_id":"candidate.profile","kind":"install"}`},
		{"POST", "/api/models/operations/cancel", `{"operation_id":"op_1"}`},
	} {
		if request(s, route.method, route.path, route.body, nil, nil).Code != 401 {
			t.Fatal("unauthenticated model endpoint accepted")
		}
		if request(s, route.method, route.path, route.body, &owner, map[string]string{"X-CSRF-Token": ""}).Code != 403 {
			t.Fatal("model endpoint accepted cookie without origin capability")
		}
		if route.method != "GET" && request(s, route.method, route.path, route.body, &owner, map[string]string{"Origin": ""}).Code != 403 {
			t.Fatal("model mutation accepted missing origin")
		}
	}
	if app.total() != 0 {
		t.Fatal("denied model requests reached the app")
	}
}

func TestModelCatalogProjectionOmitsArtifactURLsAndLocalImplementationFields(t *testing.T) {
	s, _ := newModelTestServer(t)
	owner := login(t, s)
	rr := request(s, "GET", "/api/models", "", &owner, nil)
	if rr.Code != 200 {
		t.Fatal("catalog request failed")
	}
	if strings.Contains(rr.Body.String(), "PRIVATE_") || strings.Contains(rr.Body.String(), "https://") {
		t.Fatal("model catalog exposed private or source fields")
	}
	var catalog struct {
		Profiles []ModelProfile `json:"profiles"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &catalog) != nil || len(catalog.Profiles) != 1 {
		t.Fatal("catalog projection invalid")
	}
	profile := catalog.Profiles[0]
	if profile.ID != "candidate.profile" || profile.Selection != "candidate" || profile.GateStatus != "NOT_RUN" || profile.Bytes != 100 || !profile.DownloadAvailable {
		t.Fatal("catalog qualification metadata lost")
	}
}

func TestModelOperationsAcceptPinnedIdentifiersAndRejectPathsURLsOrUnknownFields(t *testing.T) {
	s, app := newModelTestServer(t)
	owner := login(t, s)
	for _, body := range []string{
		`{"profile_id":"../../private","kind":"install"}`, `{"profile_id":"https://example.invalid/model","kind":"install"}`,
		`{"profile_id":"candidate.profile","kind":"activate"}`, `{"profile_id":"candidate.profile","kind":"install","url":"https://example.invalid/"}`,
		`{"profile_id":"candidate.profile","PROFILE_ID":"other","kind":"install"}`, `{"profile_id":"candidate.profile","kind":"install","experimental":true}`,
	} {
		if request(s, "POST", "/api/models/operations", body, &owner, nil).Code != 400 {
			t.Fatal("invalid model operation accepted")
		}
	}
	if app.total() != 0 {
		t.Fatal("invalid operation reached the app")
	}
	if request(s, "POST", "/api/models/operations", `{"profile_id":"candidate.profile","kind":"install"}`, &owner, nil).Code != 200 || app.started.ProfileID != "candidate.profile" || app.started.Kind != "install" {
		t.Fatal("valid model admission failed")
	}
	if request(s, "POST", "/api/models/operations/cancel", `{"operation_id":"op_1"}`, &owner, nil).Code != 200 || app.cancelled != "op_1" {
		t.Fatal("model cancellation did not reach the app")
	}
}

func TestModelErrorsAreFixedAndUnsupportedAppsExposeNoFakeState(t *testing.T) {
	s, app := newModelTestServer(t)
	owner := login(t, s)
	app.err = modelstore.ErrHashMismatch
	rr := request(s, "GET", "/api/models/state", "", &owner, nil)
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), `"code":"model_hash_mismatch"`) {
		t.Fatal("model verification failure was not actionable")
	}
	app.err = ErrModelOperationBusy
	if request(s, "POST", "/api/models/operations", `{"profile_id":"candidate.profile","kind":"verify"}`, &owner, nil).Code != 409 {
		t.Fatal("busy admission was not rejected")
	}
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	for _, path := range []string{"/api/models", "/api/models/state"} {
		if request(plain, "GET", path, "", &plainOwner, nil).Code != http.StatusServiceUnavailable {
			t.Fatal("unsupported app fabricated model readiness")
		}
	}
}

func TestModelProgressProjectionRejectsRawErrorsPathsAndInventedReadiness(t *testing.T) {
	s, app := newModelTestServer(t)
	app.state = ModelStateSnapshot{Items: []ModelState{{ProfileID: "candidate.profile", InstallationState: "PRIVATE_READY", Code: "PRIVATE_STATE_ERROR",
		Operation: &ModelOperation{ID: "/PRIVATE_PATH", ProfileID: "candidate.profile", Kind: "PRIVATE_KIND", State: "PRIVATE_STATE", Phase: "PRIVATE_PHASE",
			Code: "PRIVATE_ADAPTER_ERROR", CompletedBytes: -1, TotalBytes: -1, AdditionalBytes: -1}}}}
	owner := login(t, s)
	rr := request(s, "GET", "/api/models/state", "", &owner, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "PRIVATE_") {
		t.Fatal("model progress exposed raw adapter fields")
	}
	var state ModelStateSnapshot
	if json.Unmarshal(rr.Body.Bytes(), &state) != nil || len(state.Items) != 1 {
		t.Fatal("model progress invalid")
	}
	item := state.Items[0]
	if item.InstallationState != "unknown" || item.Operation.State != "interrupted" || item.Operation.Code != "model_operation_failed" || item.Operation.CompletedBytes != 0 {
		t.Fatal("invalid progress fabricated readiness")
	}
	if app.state.Items[0].InstallationState != "PRIVATE_READY" {
		t.Fatal("HTTP projection mutated the app-owned snapshot")
	}
}
