//go:build !sqlite_fts5

package archive

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestOpenWithoutFTS5RejectsClearly(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{})
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, ErrFTS5Unavailable) {
		t.Fatalf("missing compiled FTS5 capability returned %v", err)
	}
}
