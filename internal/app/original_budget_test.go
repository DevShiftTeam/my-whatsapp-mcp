package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"testing"
)

func TestOriginalBudgetOwnerCASPersistsAndOmissionPreservesAuthority(t *testing.T) {
	root := privateTestRoot(t)
	a, err := New(context.Background(), Config{DataDir: root, Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.OriginalBudget(context.Background())
	if err != nil || first.MaximumBytes != 2<<30 || first.ChargedBytes != 0 || first.Pressure || first.Revision == 0 {
		t.Fatal("default authoritative budget unavailable")
	}
	changed, err := a.SetOriginalBudget(context.Background(), localui.OriginalBudgetRequest{MaximumBytes: 3 << 30, ExpectedRevision: first.Revision})
	if err != nil || changed.MaximumBytes != 3<<30 || changed.Revision <= first.Revision {
		t.Fatal(err)
	}
	if _, err = a.SetOriginalBudget(context.Background(), localui.OriginalBudgetRequest{MaximumBytes: 4 << 30, ExpectedRevision: first.Revision}); !errors.Is(err, localui.ErrOriginalBudgetConflict) {
		t.Fatal("stale budget setter overwrote authority")
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), Config{DataDir: root, Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	actual, err := reopened.OriginalBudget(context.Background())
	if err != nil || actual != changed {
		t.Fatal("omission reset central budget")
	}
}
func TestExplicitOriginalBudgetActionUsesCentralStoreBeforeResources(t *testing.T) {
	value := int64(64 << 20)
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true, OriginalBudgetBytes: &value})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	budget, err := a.OriginalBudget(context.Background())
	if err != nil || budget.MaximumBytes != value {
		t.Fatal("explicit action ignored")
	}
	value = 1
	if _, err := New(context.Background(), Config{DataDir: privateTestRoot(t), OriginalBudgetBytes: &value}); err == nil {
		t.Fatal("out-of-range authority accepted")
	}
}
