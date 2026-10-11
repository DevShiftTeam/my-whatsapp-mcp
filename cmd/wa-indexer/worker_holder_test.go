package main

import (
	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
	"testing"
)

func TestWorkerHolderRequiresExactHiddenArgument(t *testing.T) {
	if !isWorkerHolder([]string{workerlife.HolderModeArgument}) {
		t.Fatal("exact holder argument refused")
	}
	for _, arguments := range [][]string{nil, {workerlife.HolderModeArgument, "--data-dir", "/untrusted"}, {"serve", workerlife.HolderModeArgument}, {"--workerlife-holder=1"}} {
		if isWorkerHolder(arguments) {
			t.Fatal("ordinary/user arguments entered holder authority")
		}
	}
}
