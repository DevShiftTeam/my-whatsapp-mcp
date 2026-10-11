package localui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeLocalAIApplication struct {
	*fakeApplication
	snapshot LocalAISnapshot
	updated  LocalAIUpdate
}

func (a *fakeLocalAIApplication) LocalAIState(context.Context) (LocalAISnapshot, error) {
	return a.snapshot, a.called("local_ai_state")
}

func (a *fakeLocalAIApplication) SetLocalAI(_ context.Context, input LocalAIUpdate) (LocalAISnapshot, error) {
	a.updated = input
	return a.snapshot, a.called("local_ai_select")
}

func newLocalAITestServer(t *testing.T) (*Server, *fakeLocalAIApplication) {
	t.Helper()
	a := &fakeLocalAIApplication{fakeApplication: &fakeApplication{}, snapshot: LocalAISnapshot{
		Revision: 1, Choice: LocalAIChoice{Mode: "off"}, State: "off", Code: "local_ai_off", Options: []LocalAIOption{},
	}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("local AI owner construction failed")
	}
	return s, a
}

const validLocalAIUpdate = `{"choice":{"mode":"preview","encoder_profile":"encoder.preview","ocr_profile":"ocr.fast","caption":true,"speech_language":"he"},"expected_revision":7}`

func TestStrictJSONVisitsNestedObjectsAndKeepsLogicalHebrewValues(t *testing.T) {
	for _, input := range []struct {
		body string
		want bool
	}{
		{`{"items":[{"label":"שלום English 42"},{"label":"אחר"}]}`, true},
		{`{"items":[{"label":"שלום","LABEL":"other"}]}`, false},
		{`{"items":[{"label":"שלום","\u006cabel":"other"}]}`, false},
		{`{"items":[{"\u017fpeech_language":"he"}]}`, false},
		{`{"choice":{"mode":"preview"}} {"choice":{"mode":"off"}}`, false},
		{`{"items":` + strings.Repeat("[", 33) + `0` + strings.Repeat("]", 33) + `}`, false},
	} {
		if uniqueFields([]byte(input.body)) != input.want {
			t.Fatal("nested strict JSON validation changed the wrong keys or values")
		}
	}
}

func TestLocalAIRequiresOwnerCapabilitiesAndExactMutationOrigin(t *testing.T) {
	s, app := newLocalAITestServer(t)
	owner := login(t, s)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/local-ai", ""}, {"POST", "/api/local-ai/select", validLocalAIUpdate},
	} {
		for _, headers := range []map[string]string{{"X-CSRF-Token": ""}, {"X-CSRF-Token": "wrong", "Origin": testOrigin}, {"Host": "localhost:8123"}} {
			if request(s, route.method, route.path, route.body, &owner, headers).Code != 403 {
				t.Fatal("local AI request accepted an invalid owner capability")
			}
		}
		if request(s, route.method, route.path, route.body, nil, nil).Code != 401 {
			t.Fatal("local AI endpoint accepted no owner")
		}
	}
	for _, origin := range []string{"", "http://127.0.0.1:8124", "https://example.invalid"} {
		if request(s, "POST", "/api/local-ai/select", validLocalAIUpdate, &owner, map[string]string{"Origin": origin}).Code != 403 {
			t.Fatal("local AI selection accepted an invalid origin")
		}
	}
	if request(s, "GET", "/api/local-ai/select", "", &owner, nil).Code != 405 || app.total() != 0 {
		t.Fatal("denied selection reached the app")
	}
}

func TestLocalAISelectionRejectsUnknownAliasesPathsAndInvalidChoices(t *testing.T) {
	s, app := newLocalAITestServer(t)
	owner := login(t, s)
	for _, body := range []string{
		`{"choice":{"mode":"preview","encoder_profile":"../../private"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"https://example.invalid/model"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"encoder.preview","path":"/private"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"encoder.preview","ENCODER_PROFILE":"other"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"encoder.preview","encoder_profile":"other"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"encoder.preview","encoder_\u0070rofile":"other"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","\u017fpeech_language":"he"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","speech_language":"he","\u017fpeech_language":"en"},"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"encoder.preview"},"expected_revision":7,"Expected_Revision":8}`,
		`{"choice":{"mode":"preview","caption":true},"expected_revision":0}`,
		`{"choice":{"mode":"preview","caption":true},"expected_revision":1.5}`,
		`{"choice":{"mode":"preview","caption":true},"expected_revision":18446744073709551616}`,
		`{"choice":{"mode":"preview","speech_language":"auto"},"expected_revision":7}`,
		`{"choice":{"mode":"preview"},"expected_revision":7}`,
		`{"choice":{"mode":"off","caption":true},"expected_revision":7}`,
		`{"choice":{"mode":"active","caption":true},"expected_revision":7}`,
		`{"choice":null,"expected_revision":7}`,
		`{"choice":{"mode":"preview","encoder_profile":"` + strings.Repeat("a", 101) + `"},"expected_revision":7}`,
	} {
		if request(s, "POST", "/api/local-ai/select", body, &owner, nil).Code != 400 {
			t.Fatal("invalid local AI selection accepted")
		}
	}
	body := validLocalAIUpdate + strings.Repeat(" ", maxBodyBytes-len(validLocalAIUpdate)+1)
	if request(s, "POST", "/api/local-ai/select", body, &owner, nil).Code != 413 || app.total() != 0 {
		t.Fatal("oversized or invalid selection reached the app")
	}
}

func TestLocalAISelectionForwardsExactDesiredChoiceAndCAS(t *testing.T) {
	s, app := newLocalAITestServer(t)
	owner := login(t, s)
	choice := LocalAIChoice{Mode: "preview", EncoderProfile: "encoder.preview", OCRProfile: "ocr.fast", Caption: true, SpeechLanguage: "he"}
	app.snapshot = LocalAISnapshot{Revision: 8, Choice: choice, State: "applying", Code: "local_ai_applying", Options: []LocalAIOption{{Kind: "encoder", ProfileID: "encoder.preview", ModelProfileID: "weights.pinned", State: "preview"}}}
	rr := request(s, "POST", "/api/local-ai/select", validLocalAIUpdate, &owner, nil)
	if rr.Code != 200 || app.updated.Choice != choice || app.updated.ExpectedRevision != 7 {
		t.Fatal("desired selection or CAS was not forwarded exactly")
	}
	var got LocalAISnapshot
	if json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Revision != 8 || got.Choice != choice || got.State != "applying" {
		t.Fatal("selection fabricated activation before application")
	}
	app.err = ErrLocalAIConflict
	if request(s, "POST", "/api/local-ai/select", validLocalAIUpdate, &owner, nil).Code != 409 {
		t.Fatal("CAS conflict did not request a reload")
	}
	app.err = nil
	app.snapshot = LocalAISnapshot{Revision: 9, Choice: LocalAIChoice{Mode: "off"}, State: "cleanup_pending", Code: "local_ai_cleanup_pending", Options: []LocalAIOption{}}
	rr = request(s, "POST", "/api/local-ai/select", `{"choice":{"mode":"off","caption":false},"expected_revision":8}`, &owner, nil)
	if rr.Code != 200 || app.updated.Choice != (LocalAIChoice{Mode: "off"}) || app.updated.ExpectedRevision != 8 || !strings.Contains(rr.Body.String(), `"state":"cleanup_pending"`) {
		t.Fatal("off selection hid pending cleanup or retained enabled components")
	}
}

func TestLocalAIStateRetainsUnavailableDesiredChoiceWithoutInventingEligibility(t *testing.T) {
	s, app := newLocalAITestServer(t)
	owner := login(t, s)
	choice := LocalAIChoice{Mode: "preview", EncoderProfile: "encoder.preview"}
	app.snapshot = LocalAISnapshot{Revision: 11, Choice: choice, State: "unavailable", Code: "local_ai_model_missing", Options: []LocalAIOption{{Kind: "encoder", ProfileID: "encoder.preview", ModelProfileID: "weights.pinned", State: "unavailable", Code: "local_ai_profile_unqualified"}}}
	rr := request(s, "GET", "/api/local-ai", "", &owner, nil)
	var got LocalAISnapshot
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Choice != choice || got.State != "unavailable" || len(got.Options) != 1 || got.Options[0].State != "unavailable" {
		t.Fatal("unavailable desired choice was enabled, erased or relabeled")
	}
	if app.count("local_ai_select") != 0 {
		t.Fatal("reading activation state performed a selection")
	}
	app.snapshot.State = "PRIVATE_NATIVE_STATE"
	app.snapshot.Code = "PRIVATE_RAW_DIAGNOSTIC"
	app.snapshot.Options[0].State = "PRIVATE_ELIGIBLE"
	app.snapshot.Options[0].Code = "PRIVATE_OPTION_ERROR"
	rr = request(s, "GET", "/api/local-ai", "", &owner, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "PRIVATE_") || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.State != "unavailable" || got.Options[0].State != "unavailable" {
		t.Fatal("raw native state crossed the owner projection or invented eligibility")
	}
	if app.snapshot.State != "PRIVATE_NATIVE_STATE" || app.snapshot.Options[0].State != "PRIVATE_ELIGIBLE" {
		t.Fatal("projection changed the app snapshot")
	}
}

func TestLocalAIMalformedSnapshotAndUnsupportedCapabilityFailClosed(t *testing.T) {
	s, app := newLocalAITestServer(t)
	owner := login(t, s)
	for _, snapshot := range []LocalAISnapshot{
		{Revision: 0, Choice: LocalAIChoice{Mode: "off"}},
		{Revision: 1, Choice: LocalAIChoice{Mode: "preview", EncoderProfile: "/PRIVATE_PATH"}},
		{Revision: 1, Choice: LocalAIChoice{Mode: "off"}, Options: make([]LocalAIOption, 65)},
	} {
		app.snapshot = snapshot
		rr := request(s, "GET", "/api/local-ai", "", &owner, nil)
		if rr.Code != 503 || strings.Contains(rr.Body.String(), "PRIVATE_") {
			t.Fatal("malformed desired state fabricated a usable choice")
		}
	}
	app.err = errors.New("PRIVATE_APP_FAILURE")
	rr := request(s, "GET", "/api/local-ai", "", &owner, nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "PRIVATE_") {
		t.Fatal("raw activation failure escaped")
	}
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	if request(plain, "GET", "/api/local-ai", "", &plainOwner, nil).Code != 503 || request(plain, "POST", "/api/local-ai/select", validLocalAIUpdate, &plainOwner, nil).Code != 503 {
		t.Fatal("unsupported app fabricated local AI controls")
	}
}
