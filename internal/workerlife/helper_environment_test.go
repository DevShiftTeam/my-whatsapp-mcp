package workerlife

import (
	"strings"
	"testing"
)

func TestHelperEnvironmentRemovesLoaderOverridesFromBothSources(t *testing.T) {
	ambient := []string{"DYLD_INSERT_LIBRARIES=/private/canary", "DYLD_FRAMEWORK_PATH=/private/canary", "LD_PRELOAD=/private/canary", "ROLE=holder", "GORACE=atexit_sleep_ms=0"}
	configured := []string{"DYLD_LIBRARY_PATH=/private/canary", "DYLD_PRINT_TO_FILE=/private/canary", "LD_LIBRARY_PATH=/private/canary", "LD_AUDIT=/private/canary", "WA_TEST_LITERAL=retained", "GOMAXPROCS=9"}
	actual := helperEnvironment(ambient, configured)
	retained := map[string]bool{}
	for _, value := range actual {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "DYLD_") || key == "LD_PRELOAD" || key == "LD_LIBRARY_PATH" || key == "LD_AUDIT" {
			t.Fatal("loader override survived", key)
		}
		retained[value] = true
	}
	if !retained["ROLE=holder"] || !retained["GORACE=atexit_sleep_ms=0"] || !retained["WA_TEST_LITERAL=retained"] || actual[len(actual)-1] != "GOMAXPROCS=1" {
		t.Fatal("role data or fixed holder thread setting lost", actual)
	}
	if ambient[0] != "DYLD_INSERT_LIBRARIES=/private/canary" || configured[0] != "DYLD_LIBRARY_PATH=/private/canary" {
		t.Fatal("borrowed environment mutated")
	}
}
