package inference

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeWorkerEnvironmentRejectsLoaderOverrides(t *testing.T) {
	for _, key := range []string{"DYLD_INSERT_LIBRARIES", "DYLD_FRAMEWORK_PATH", "LD_PRELOAD"} {
		t.Setenv(key, "")
	}
	t.Setenv("WA_ENCODER_AMBIENT_CANARY", "ambient-retained")
	lease := &testLease{}
	s, err := newTestSupervisor(t, Config{
		Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"},
		Environment: []string{
			"WA_ENCODER_HELPER=environment", "WA_ENCODER_CONFIGURED_CANARY=configured-retained",
			"DYLD_LIBRARY_PATH=", "DYLD_PRINT_TO_FILE=", "LD_LIBRARY_PATH=", "LD_AUDIT=",
			"GOMAXPROCS=9", "TOKENIZERS_PARALLELISM=true", "OMP_NUM_THREADS=9",
			"OPENBLAS_NUM_THREADS=9", "MKL_NUM_THREADS=9", "VECLIB_MAXIMUM_THREADS=9",
		},
		Profile: ReferenceProfile(), AllowExperimental: true,
		Acquire:     func(context.Context, Profile) (ComponentLease, error) { return lease, nil },
		IdleTimeout: time.Second, RequestTimeout: 2 * time.Second,
		TerminationGrace: 20 * time.Millisecond, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	vector, err := s.EncodeQuery(context.Background(), "synthetic environment fixture")
	if err != nil || len(vector) != 384 || vector[0] != 1 {
		t.Fatalf("native environment fixture failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().PID != 0 || !lease.isClosed() {
		t.Fatal("native environment fixture retained process or component ownership")
	}
}

// Loader canaries are empty so a failing regression cannot load any library.
// Presence still proves whether the actual child environment was filtered.
func supervisorFixtureEnvironmentValid() bool {
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "DYLD_") || key == "LD_PRELOAD" || key == "LD_LIBRARY_PATH" || key == "LD_AUDIT" {
			return false
		}
	}
	for key, expected := range map[string]string{
		"WA_ENCODER_HELPER": "environment", "WA_ENCODER_AMBIENT_CANARY": "ambient-retained",
		"WA_ENCODER_CONFIGURED_CANARY": "configured-retained", "GOMAXPROCS": "2",
		"TOKENIZERS_PARALLELISM": "false", "OMP_NUM_THREADS": "2", "OPENBLAS_NUM_THREADS": "2",
		"MKL_NUM_THREADS": "2", "VECLIB_MAXIMUM_THREADS": "2",
	} {
		if actual, present := os.LookupEnv(key); !present || actual != expected {
			return false
		}
	}
	return true
}
