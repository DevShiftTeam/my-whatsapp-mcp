//go:build ts_omit_logtail

package remoteaccess

import (
	"testing"

	"tailscale.com/feature/buildfeatures"
)

func TestTailscaleProductionOmitsLogtail(t *testing.T) {
	if buildfeatures.HasLogTail {
		t.Fatal("production logtail enabled")
	}
}
