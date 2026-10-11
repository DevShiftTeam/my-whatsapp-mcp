//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationReopenChecksumAndNewerSchema(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t, s)
	r := apply(t, s, event("m", "persistent", 1, EventOriginal), p.Generation)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.SourceMetadata(ctx, event("m", "", 1, EventOriginal).Key)
	if err != nil || m.Version.MessageID != r.MessageID {
		t.Fatalf("reopen %#v %v", m, err)
	}
	if _, err = s.writeDB.Exec("UPDATE schema_migrations SET checksum='changed'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(ctx, dir, Options{})
	if !errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("checksum %v", err)
	}
}
func TestOpenRejectsUnsafeExistingFiles(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "archive.sqlite")); err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), dir, Options{})
	if !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("symlink %v", err)
	}
}

func TestMigrationRejectsNewerSchema(t *testing.T) {
	s := testArchive(t)
	dir := s.privateDir
	if _, err := s.writeDB.Exec("INSERT INTO schema_migrations(version,checksum) VALUES(11,'future')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), dir, Options{}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer schema %v", err)
	}
}
