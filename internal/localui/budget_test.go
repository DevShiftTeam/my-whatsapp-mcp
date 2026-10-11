package localui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeBudgetApplication struct {
	*fakeApplication
	budget OriginalBudgetSnapshot
	input  OriginalBudgetRequest
}

func (a *fakeBudgetApplication) OriginalBudget(context.Context) (OriginalBudgetSnapshot, error) {
	return a.budget, a.called("budget_read")
}
func (a *fakeBudgetApplication) SetOriginalBudget(_ context.Context, input OriginalBudgetRequest) (OriginalBudgetSnapshot, error) {
	a.input = input
	err := a.called("budget_update")
	if err == nil {
		a.budget.MaximumBytes = input.MaximumBytes
		a.budget.Revision++
		a.budget.Pressure = a.budget.ChargedBytes >= input.MaximumBytes
	}
	return a.budget, err
}

func newBudgetTestServer(t *testing.T) (*Server, *fakeBudgetApplication) {
	t.Helper()
	a := &fakeBudgetApplication{fakeApplication: &fakeApplication{}, budget: OriginalBudgetSnapshot{MaximumBytes: 2 << 30, ChargedBytes: 3 << 30, Revision: 7, Pressure: true}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("budget owner construction failed")
	}
	return s, a
}

func TestOriginalBudgetRequiresOwnerCapabilityAndMutationOrigin(t *testing.T) {
	s, app := newBudgetTestServer(t)
	owner := login(t, s)
	for _, method := range []string{"GET", "PUT"} {
		body := `{"maximum_bytes":2147483648,"expected_revision":7}`
		if request(s, method, "/api/original-budget", body, nil, nil).Code != 401 {
			t.Fatal("budget accepted unauthenticated access")
		}
		if request(s, method, "/api/original-budget", body, &owner, map[string]string{"X-CSRF-Token": ""}).Code != 403 {
			t.Fatal("budget accepted cookie without capability")
		}
	}
	if request(s, "PUT", "/api/original-budget", `{"maximum_bytes":2147483648,"expected_revision":7}`, &owner, map[string]string{"Origin": ""}).Code != 403 {
		t.Fatal("budget update accepted missing origin")
	}
	if app.total() != 0 {
		t.Fatal("denied budget requests reached app")
	}
	if request(s, "GET", "/api/original-budget", "", &owner, nil).Code != 200 || app.count("budget_update") != 0 {
		t.Fatal("budget read mutated state")
	}
}

func TestOriginalBudgetAcceptsLowerChargedLimitAndRejectsInvalidNumbers(t *testing.T) {
	s, app := newBudgetTestServer(t)
	owner := login(t, s)
	for _, body := range []string{
		`{"maximum_bytes":67108863,"expected_revision":7}`, `{"maximum_bytes":1099511627777,"expected_revision":7}`, `{"maximum_bytes":2147483648.5,"expected_revision":7}`, `{"maximum_bytes":9223372036854775808,"expected_revision":7}`,
		`{"maximum_bytes":2147483648,"expected_revision":0}`, `{"maximum_bytes":2147483648,"expected_revision":7,"path":"/private"}`, `{"maximum_bytes":2147483648,"MAXIMUM_BYTES":67108864,"expected_revision":7}`,
	} {
		if request(s, "PUT", "/api/original-budget", body, &owner, nil).Code != 400 {
			t.Fatal("invalid original budget accepted")
		}
	}
	if app.total() != 0 {
		t.Fatal("invalid budget reached app")
	}
	rr := request(s, "PUT", "/api/original-budget", `{"maximum_bytes":67108864,"expected_revision":7}`, &owner, nil)
	if rr.Code != 200 || app.input.MaximumBytes != 64<<20 || app.input.ExpectedRevision != 7 {
		t.Fatal("valid lower-than-charged budget was rejected or changed")
	}
	var value OriginalBudgetSnapshot
	if json.Unmarshal(rr.Body.Bytes(), &value) != nil || value.MaximumBytes != 64<<20 || value.Revision != 8 || value.ChargedBytes != 3<<30 || !value.Pressure {
		t.Fatal("original charges or pressure fabricated")
	}
}

func TestOriginalBudgetConflictAndUnavailabilityAreFixed(t *testing.T) {
	s, app := newBudgetTestServer(t)
	owner := login(t, s)
	for _, check := range []struct {
		err    error
		status int
		code   string
	}{{ErrOriginalBudgetConflict, 409, "original_budget_changed"}, {errors.New("PRIVATE_PATH PRIVATE_TOKEN"), 503, "original_budget_unavailable"}} {
		app.err = check.err
		rr := request(s, "PUT", "/api/original-budget", `{"maximum_bytes":2147483648,"expected_revision":7}`, &owner, nil)
		if rr.Code != check.status || !strings.Contains(rr.Body.String(), check.code) || strings.Contains(rr.Body.String(), "PRIVATE_") {
			t.Fatal("budget error was not fixed")
		}
	}
	app.err = nil
	app.budget.Pressure = false
	if request(s, "GET", "/api/original-budget", "", &owner, nil).Code != 503 {
		t.Fatal("inconsistent budget invented healthy pressure")
	}
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	if request(plain, "GET", "/api/original-budget", "", &plainOwner, nil).Code != 503 {
		t.Fatal("optional budget fabricated state")
	}
}
