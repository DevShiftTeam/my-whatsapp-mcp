package app

import (
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"testing"
	"time"
)

func TestLiveRebuildRuntimeConfigurationKeepsOrdinaryDefaults(t *testing.T) {
	paths := appdata.Paths{Root: "/private/synthetic", WorkingDir: "/private/synthetic/working"}
	cfg, err := liveRebuildRuntimeConfig(paths, nil, 32)
	if err != nil || cfg != nil {
		t.Fatal("ordinary archive opted into rebuild")
	}
	boundary := time.Now().Add(-time.Minute)
	provenance := &archive.RebuildProvenance{Generation: "synthetic-generation", Account: "synthetic-account", Boundary: boundary}
	cfg, err = liveRebuildRuntimeConfig(paths, provenance, 32)
	if err != nil || cfg == nil || cfg.ObservationID != provenance.Generation || cfg.Account != provenance.Account || !cfg.Boundary.Equal(boundary) || cfg.Requests != 32 {
		t.Fatal("runtime lost persisted source boundary")
	}
}

func TestLiveRebuildRuntimeConfigurationFailsClosedOnInvalidProvenance(t *testing.T) {
	for _, value := range []*archive.RebuildProvenance{
		{}, {Generation: "generation", Account: "account"},
		{Generation: "generation", Account: "account", Boundary: time.Now().Add(time.Hour)},
	} {
		if _, err := liveRebuildRuntimeConfig(appdata.Paths{}, value, 32); err == nil {
			t.Fatal("invalid provenance accepted")
		}
	}
}
