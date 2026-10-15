package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const (
	historySpoolCanaryURL     = "https://canary.invalid/history-spool/direct-path"
	historySpoolCanaryKey     = "canary-media-key-material-0123456789"
	historySpoolCanaryHandle  = "canary-enc-handle"
	historySpoolCanaryRequest = "canary-request-id"
	historySpoolCanaryMessage = "canary-original-message"
	historySpoolCanaryAccount = "canary-account@example.invalid"
	historySpoolCanaryBody    = "canary-compressed-body"
)

func historySpoolTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func historySpoolTestDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(historySpoolTestRoot(t), "history-spool")
}

func historySpoolTestOpen(t *testing.T, dir string) *historySpool {
	t.Helper()
	spool, err := openHistorySpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.close() })
	return spool
}

func historySpoolTestNotice(payload []byte) *waE2E.HistorySyncNotification {
	digest := sha256.Sum256(payload)
	return &waE2E.HistorySyncNotification{
		FileSHA256:                             digest[:],
		FileLength:                             proto.Uint64(uint64(len(payload))),
		MediaKey:                               []byte(historySpoolCanaryKey),
		FileEncSHA256:                          bytes.Repeat([]byte{0x22}, 32),
		DirectPath:                             proto.String(historySpoolCanaryURL),
		SyncType:                               waE2E.HistorySyncType_INITIAL_BOOTSTRAP.Enum(),
		ChunkOrder:                             proto.Uint32(3),
		OriginalMessageID:                      proto.String(historySpoolCanaryMessage),
		EncHandle:                              proto.String(historySpoolCanaryHandle),
		FullHistorySyncOnDemandRequestMetadata: &waE2E.FullHistorySyncOnDemandRequestMetadata{RequestID: proto.String(historySpoolCanaryRequest)},
	}
}

func historySpoolTestNoticeWithMetadata(t *testing.T, payload []byte, size int) *waE2E.HistorySyncNotification {
	t.Helper()
	notice := historySpoolTestNotice(payload)
	padding := 0
	for range 4 {
		notice.EncHandle = proto.String(strings.Repeat("h", padding))
		current := proto.Size(notice)
		if current == size {
			return notice
		}
		padding += size - current
		if padding < 0 {
			break
		}
	}
	t.Fatal("could not build metadata of the requested size")
	return nil
}

func historySpoolTestSave(t *testing.T, spool *historySpool, payload []byte) string {
	t.Helper()
	id, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func historySpoolTestEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func historySpoolTestWrite(t *testing.T, path string, mode os.FileMode, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func historySpoolTestDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func historySpoolTestFlip(data []byte, index int) []byte {
	out := bytes.Clone(data)
	out[index] ^= 0x01
	return out
}

func TestHistorySpoolRoundTripsAcrossReopen(t *testing.T) {
	dir := historySpoolTestDir(t)
	first := historySpoolTestOpen(t, dir)
	payload := []byte("synthetic compressed " + historySpoolCanaryBody)
	notice := historySpoolTestNotice(payload)
	id, err := first.save(t.Context(), notice, payload, historySpoolCanaryAccount)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	reopened := historySpoolTestOpen(t, dir)
	got, raw, account, err := reopened.read(id)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, notice) || !bytes.Equal(raw, payload) || account != historySpoolCanaryAccount {
		t.Fatal("reopened batch differs from the saved batch")
	}
}

func TestHistorySpoolMissingKeyWithCrashResidueNeverRegenerates(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	id := historySpoolTestSave(t, spool, []byte("synthetic orphaned complete capture"))
	if err := spool.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, id+".batch"), filepath.Join(dir, id+".tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openHistorySpool(dir); err == nil {
		_ = reopened.close()
		t.Fatal("new key orphaned retained crash residue")
	}
	if _, err := os.Lstat(filepath.Join(dir, "key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing key was silently replaced")
	}
}

func TestHistorySpoolStoresOnlyEncryptedOpaqueFiles(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := []byte("zlib-shaped synthetic " + historySpoolCanaryBody + strings.Repeat("-", 512))
	id := historySpoolTestSave(t, spool, payload)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatal("batch identifier is not an opaque 16-byte hex value")
	}
	want := []string{"key", id + ".batch"}
	slices.Sort(want)
	if got := historySpoolTestEntries(t, dir); !slices.Equal(got, want) {
		t.Fatal("successful save left unexpected entries")
	}
	for _, name := range want {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{historySpoolCanaryURL, historySpoolCanaryKey, historySpoolCanaryHandle, historySpoolCanaryRequest, historySpoolCanaryMessage, historySpoolCanaryAccount, historySpoolCanaryBody} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("%s stores plaintext metadata", name)
			}
		}
	}
}

func TestHistorySpoolStoresInlinePayloadOnce(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := bytes.Repeat([]byte("inline-body-"), 1<<17)
	notice := historySpoolTestNotice(payload)
	notice.FileSHA256 = nil
	notice.InitialHistBootstrapInlinePayload = payload
	id, err := spool.save(t.Context(), notice, payload, historySpoolCanaryAccount)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, id+".batch"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= int64(len(payload))+4096 {
		t.Fatal("inline payload was stored twice")
	}
	got, raw, _, err := spool.read(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetInitialHistBootstrapInlinePayload() != nil || !bytes.Equal(raw, payload) {
		t.Fatal("read returned an inline copy instead of the single compressed payload")
	}
}

func TestHistorySpoolRejectsIncompleteOrMismatchedInputs(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := []byte("complete synthetic payload")
	mismatched := historySpoolTestNotice(payload)
	mismatched.FileSHA256[0] ^= 1
	shortDigest := historySpoolTestNotice(payload)
	shortDigest.FileSHA256 = []byte{1, 2, 3}
	inlineMismatch := historySpoolTestNotice(payload)
	inlineMismatch.InitialHistBootstrapInlinePayload = []byte("different synthetic payload")
	cases := map[string]struct {
		notice  *waE2E.HistorySyncNotification
		payload []byte
		account string
	}{
		"nil notice":       {nil, payload, historySpoolCanaryAccount},
		"nil payload":      {historySpoolTestNotice(payload), nil, historySpoolCanaryAccount},
		"empty payload":    {historySpoolTestNotice(payload), []byte{}, historySpoolCanaryAccount},
		"digest mismatch":  {mismatched, payload, historySpoolCanaryAccount},
		"short digest":     {shortDigest, payload, historySpoolCanaryAccount},
		"inline mismatch":  {inlineMismatch, payload, historySpoolCanaryAccount},
		"empty account":    {historySpoolTestNotice(payload), payload, ""},
		"control account":  {historySpoolTestNotice(payload), payload, "a\nb"},
		"oversize account": {historySpoolTestNotice(payload), payload, strings.Repeat("a", historySpoolMaxAccount+1)},
	}
	for name, tc := range cases {
		if _, err := spool.save(t.Context(), tc.notice, tc.payload, tc.account); !errors.Is(err, errHistorySpoolUnavailable) {
			t.Fatalf("%s was accepted", name)
		}
	}
	if got := historySpoolTestEntries(t, dir); !slices.Equal(got, []string{"key"}) {
		t.Fatal("rejected input left files behind")
	}
}

func TestHistorySpoolEnforcesPayloadAndMetadataLimits(t *testing.T) {
	spool := historySpoolTestOpen(t, historySpoolTestDir(t))
	full := bytes.Repeat([]byte{'m'}, int(compressedMax))
	if _, err := spool.save(t.Context(), historySpoolTestNotice(full), full, historySpoolCanaryAccount); err != nil {
		t.Fatal("complete maximum-size payload was refused:", err)
	}
	over := bytes.Repeat([]byte{'m'}, int(compressedMax)+1)
	if _, err := spool.save(t.Context(), historySpoolTestNotice(over), over, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolLimit) {
		t.Fatal("oversize payload was not refused as a limit:", err)
	}
	small := []byte("metadata boundary payload")
	atLimit := historySpoolTestNoticeWithMetadata(t, small, historySpoolMaxMetadata)
	if _, err := spool.save(t.Context(), atLimit, small, historySpoolCanaryAccount); err != nil {
		t.Fatal("metadata at the limit was refused:", err)
	}
	overLimit := historySpoolTestNoticeWithMetadata(t, small, historySpoolMaxMetadata+1)
	if _, err := spool.save(t.Context(), overLimit, small, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolLimit) {
		t.Fatal("oversize metadata was not refused as a limit:", err)
	}
}

func TestHistorySpoolQuotaRefusesWithoutDeletingEntries(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := []byte("quota payload")
	spool.maxBatches = 2
	first := historySpoolTestSave(t, spool, payload)
	second := historySpoolTestSave(t, spool, payload)
	if _, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolLimit) {
		t.Fatal("count limit did not refuse the third batch:", err)
	}
	want := []string{"key", first + ".batch", second + ".batch"}
	slices.Sort(want)
	if got := historySpoolTestEntries(t, dir); !slices.Equal(got, want) {
		t.Fatal("refused save changed retained entries")
	}
	info, err := os.Stat(filepath.Join(dir, first+".batch"))
	if err != nil {
		t.Fatal(err)
	}
	spool.maxBatches = 128
	spool.maxBytes = 2 * info.Size()
	if _, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolLimit) {
		t.Fatal("byte limit did not refuse the next batch:", err)
	}
	spool.maxBytes = 3 * info.Size()
	third := historySpoolTestSave(t, spool, payload)
	ids, err := spool.pending()
	if err != nil {
		t.Fatal(err)
	}
	want = []string{first, second, third}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatal("pending batches changed after quota refusals")
	}
}

func TestHistorySpoolRemoveIsExplicitAndNonTraversing(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := []byte("remove payload")
	keep := historySpoolTestSave(t, spool, payload)
	drop := historySpoolTestSave(t, spool, payload)
	invalid := []string{"", "../" + keep, keep + "/..", "G" + drop[1:], drop[:31], drop + "x", "/" + drop, "key"}
	for _, id := range invalid {
		if err := spool.remove(id); !errors.Is(err, errHistorySpoolUnavailable) {
			t.Fatal("invalid batch identifier was accepted")
		}
	}
	if err := spool.remove(drop); err != nil {
		t.Fatal(err)
	}
	if err := spool.remove(drop); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("removing an absent batch was not explicit")
	}
	want := []string{"key", keep + ".batch"}
	slices.Sort(want)
	if got := historySpoolTestEntries(t, dir); !slices.Equal(got, want) {
		t.Fatal("remove affected other entries")
	}
	if _, _, _, err := spool.read(keep); err != nil {
		t.Fatal(err)
	}
}

func TestHistorySpoolRefusesLinkedOrWidenedBatchesAfterOpen(t *testing.T) {
	dir := historySpoolTestDir(t)
	outside := filepath.Dir(dir)
	spool := historySpoolTestOpen(t, dir)
	id := historySpoolTestSave(t, spool, []byte("linked payload"))
	batch := filepath.Join(dir, id+".batch")
	alias := filepath.Join(outside, "alias.batch")
	if err := os.Link(batch, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := spool.read(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("hard-linked batch was read")
	}
	if err := spool.remove(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("hard-linked batch was removed")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(batch, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := spool.read(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("widened batch was read")
	}
	if err := os.Chmod(batch, 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(outside, "moved.batch")
	if err := os.Rename(batch, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, batch); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := spool.read(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("symlinked batch was read")
	}
	if err := spool.remove(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("symlinked batch was removed")
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("symlink handling touched its target")
	}
}

func TestHistorySpoolTamperingFailsAuthentication(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	first := historySpoolTestSave(t, spool, []byte("first tamper payload"))
	second := historySpoolTestSave(t, spool, []byte("second tamper payload"))
	firstPath := filepath.Join(dir, first+".batch")
	original, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(filepath.Join(dir, second+".batch"))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string][]byte{
		"version":    historySpoolTestFlip(original, 0),
		"nonce":      historySpoolTestFlip(original, 1),
		"ciphertext": historySpoolTestFlip(original, len(original)/2),
		"tag":        historySpoolTestFlip(original, len(original)-1),
		"truncated":  original[:len(original)-1],
		"appended":   append(bytes.Clone(original), 0),
		"swapped":    other,
	}
	for name, data := range mutations {
		if err := os.WriteFile(firstPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := spool.read(first); !errors.Is(err, errHistorySpoolUnavailable) {
			t.Fatalf("%s mutation was accepted", name)
		}
	}
	if err := os.WriteFile(firstPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := spool.read(first); err != nil {
		t.Fatal("restored batch was refused")
	}
}

func TestHistorySpoolOpenRefusesUnsafeDirectories(t *testing.T) {
	base := historySpoolTestRoot(t)
	check := func(name, path string) {
		t.Helper()
		_, err := openHistorySpool(path)
		if !errors.Is(err, errHistorySpoolUnavailable) {
			t.Fatalf("%s was accepted", name)
		}
		if strings.Contains(err.Error(), base) {
			t.Fatalf("%s error exposed a private path", name)
		}
	}
	check("relative path", "history-spool")
	check("unclean path", filepath.Join(base, "history-spool")+"/")
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0770); err != nil {
		t.Fatal(err)
	}
	check("group-writable ancestor", filepath.Join(shared, "history-spool"))
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	check("symlinked ancestor", filepath.Join(link, "history-spool"))
	wide := filepath.Join(base, "wide")
	if err := os.Mkdir(wide, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0755); err != nil {
		t.Fatal(err)
	}
	check("widened spool directory", wide)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	spoolLink := filepath.Join(base, "spool-link")
	if err := os.Symlink(target, spoolLink); err != nil {
		t.Fatal(err)
	}
	check("symlinked spool directory", spoolLink)
}

func TestHistorySpoolOpenRefusesUnsafeEntries(t *testing.T) {
	cases := map[string]func(t *testing.T, dir, id string){
		"unknown name": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "notes.txt"), 0600, []byte("synthetic"))
		},
		"non-hex batch name": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "Q"+id[1:]+".batch"), 0600, []byte("synthetic"))
		},
		"directory entry": func(t *testing.T, dir, id string) {
			if err := os.Mkdir(filepath.Join(dir, "subdirectory"), 0700); err != nil {
				t.Fatal(err)
			}
		},
		"batch symlink": func(t *testing.T, dir, id string) {
			target := filepath.Join(filepath.Dir(dir), "synthetic-target")
			historySpoolTestWrite(t, target, 0600, []byte("synthetic"))
			if err := os.Symlink(target, filepath.Join(dir, "0123456789abcdef0123456789abcdef.batch")); err != nil {
				t.Fatal(err)
			}
		},
		"batch hard link": func(t *testing.T, dir, id string) {
			if err := os.Link(filepath.Join(dir, id+".batch"), filepath.Join(filepath.Dir(dir), "synthetic-alias")); err != nil {
				t.Fatal(err)
			}
		},
		"widened batch": func(t *testing.T, dir, id string) {
			if err := os.Chmod(filepath.Join(dir, id+".batch"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"oversize batch": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "0123456789abcdef0123456789abcdef.batch"), 0600, make([]byte, historySpoolMaxFile+1))
		},
		"widened residue": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "fedcba9876543210fedcba9876543210.tmp"), 0644, []byte("synthetic"))
		},
		"widened key": func(t *testing.T, dir, id string) {
			if err := os.Chmod(filepath.Join(dir, "key"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"short key": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "key"), 0600, make([]byte, 31))
		},
		"long key": func(t *testing.T, dir, id string) {
			historySpoolTestWrite(t, filepath.Join(dir, "key"), 0600, make([]byte, 33))
		},
		"key symlink": func(t *testing.T, dir, id string) {
			target := filepath.Join(filepath.Dir(dir), "synthetic-key")
			historySpoolTestWrite(t, target, 0600, make([]byte, 32))
			if err := os.Remove(filepath.Join(dir, "key")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "key")); err != nil {
				t.Fatal(err)
			}
		},
		"key hard link": func(t *testing.T, dir, id string) {
			if err := os.Link(filepath.Join(dir, "key"), filepath.Join(filepath.Dir(dir), "synthetic-key-alias")); err != nil {
				t.Fatal(err)
			}
		},
		"missing key beside batch": func(t *testing.T, dir, id string) {
			if err := os.Remove(filepath.Join(dir, "key")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := historySpoolTestDir(t)
			spool := historySpoolTestOpen(t, dir)
			id := historySpoolTestSave(t, spool, []byte("unsafe entry payload"))
			if err := spool.close(); err != nil {
				t.Fatal(err)
			}
			mutate(t, dir, id)
			if _, err := openHistorySpool(dir); !errors.Is(err, errHistorySpoolUnavailable) {
				t.Fatal("unsafe entry was accepted")
			}
		})
	}
}

func TestHistorySpoolKeyIsPrivateAndNeverReplaced(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	info, err := os.Lstat(filepath.Join(dir, "key"))
	if err != nil || info.Mode().Perm() != 0600 || info.Size() != 32 {
		t.Fatal("key is not a private 32-byte file")
	}
	historySpoolTestSave(t, spool, []byte("key payload"))
	if err := spool.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	if _, err := openHistorySpool(dir); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("retained batches opened without their key")
	}
	if _, err := os.Lstat(filepath.Join(dir, "key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was created beside retained batches")
	}
}

func TestHistorySpoolRetainsCrashResidueWithoutDeletingIt(t *testing.T) {
	dir := historySpoolTestDir(t)
	if err := historySpoolTestOpen(t, dir).close(); err != nil {
		t.Fatal(err)
	}
	residue := filepath.Join(dir, "0123456789abcdef0123456789abcdef.tmp")
	historySpoolTestWrite(t, residue, 0600, []byte("partial synthetic batch"))
	reopened := historySpoolTestOpen(t, dir)
	if data, err := os.ReadFile(residue); err != nil || string(data) != "partial synthetic batch" {
		t.Fatal("crash residue was changed or deleted")
	}
	if ids, err := reopened.pending(); err != nil || len(ids) != 0 {
		t.Fatal("crash residue was reported as a readable batch")
	}
	reopened.maxBatches = 1
	if _, err := reopened.save(t.Context(), historySpoolTestNotice([]byte("quota")), []byte("quota"), historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolLimit) {
		t.Fatal("crash residue was not counted toward quota")
	}
	if _, err := os.Stat(residue); err != nil {
		t.Fatal("refused save deleted crash residue")
	}

	fresh := historySpoolTestDir(t)
	if err := os.Mkdir(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(fresh, "fedcba9876543210fedcba9876543210.tmp")
	historySpoolTestWrite(t, orphan, 0600, []byte("interrupted key creation"))
	if reopened, err := openHistorySpool(fresh); err == nil {
		_ = reopened.close()
		t.Fatal("ambiguous key-creation residue permitted silent key replacement")
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("key creation deleted an orphaned residue")
	}
}

func TestHistorySpoolFailedWriteRetainsResidueAndNoBatch(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	spool.fault = func(stage string) error {
		if stage == "temp-synced" {
			return errors.New("injected")
		}
		return nil
	}
	payload := []byte("failed write payload")
	if _, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("failed write was reported as success")
	}
	var temps, batches int
	for _, name := range historySpoolTestEntries(t, dir) {
		switch {
		case strings.HasSuffix(name, ".tmp"):
			temps++
		case strings.HasSuffix(name, ".batch"):
			batches++
		}
	}
	if temps != 1 || batches != 0 {
		t.Fatal("failed write did not retain exactly its residue without a committed batch")
	}
	if ids, err := spool.pending(); err != nil || len(ids) != 0 {
		t.Fatal("uncommitted write became pending")
	}
}

func TestHistorySpoolDirectorySyncFailureKeepsCommittedBatchPending(t *testing.T) {
	spool := historySpoolTestOpen(t, historySpoolTestDir(t))
	spool.fault = func(stage string) error {
		if stage == "renamed" {
			return errors.New("injected")
		}
		return nil
	}
	payload := []byte("committed payload")
	id, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount)
	if !errors.Is(err, errHistorySpoolUnavailable) || id != "" {
		t.Fatal("unsynced rename was reported as acknowledged")
	}
	spool.fault = nil
	ids, err := spool.pending()
	if err != nil || len(ids) != 1 {
		t.Fatal("committed batch was not retained as pending")
	}
	_, raw, account, err := spool.read(ids[0])
	if err != nil || !bytes.Equal(raw, payload) || account != historySpoolCanaryAccount {
		t.Fatal("committed batch is unreadable")
	}
}

func TestHistorySpoolCancelledSaveCreatesNothing(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	payload := []byte("cancelled payload")
	if _, err := spool.save(ctx, historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled save did not report cancellation")
	}
	if got := historySpoolTestEntries(t, dir); !slices.Equal(got, []string{"key"}) {
		t.Fatal("cancelled save created files")
	}
}

func TestHistorySpoolCloseIsIdempotentAndFailsClosed(t *testing.T) {
	spool := historySpoolTestOpen(t, historySpoolTestDir(t))
	id := historySpoolTestSave(t, spool, []byte("closed payload"))
	if err := spool.close(); err != nil {
		t.Fatal(err)
	}
	if err := spool.close(); err != nil {
		t.Fatal("second close failed")
	}
	payload := []byte("after close")
	if _, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("save succeeded after close")
	}
	if _, _, _, err := spool.read(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("read succeeded after close")
	}
	if err := spool.remove(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("remove succeeded after close")
	}
	if _, err := spool.pending(); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("pending succeeded after close")
	}
}

func TestHistorySpoolDoesNotLeakDescriptors(t *testing.T) {
	dir := historySpoolTestDir(t)
	warm, err := openHistorySpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.close(); err != nil {
		t.Fatal(err)
	}
	payload := []byte("descriptor payload")
	before := historySpoolTestDescriptors(t)
	for range 3 {
		spool, err := openHistorySpool(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := spool.read(id); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.pending(); err != nil {
			t.Fatal(err)
		}
		if err := spool.remove(id); err != nil {
			t.Fatal(err)
		}
		if err := spool.close(); err != nil {
			t.Fatal(err)
		}
	}
	if after := historySpoolTestDescriptors(t); after != before {
		t.Fatalf("descriptors changed from %d to %d", before, after)
	}
	keyPath := filepath.Join(dir, "key")
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := openHistorySpool(dir); !errors.Is(err, errHistorySpoolUnavailable) {
			t.Fatal("unsafe key was accepted")
		}
	}
	if after := historySpoolTestDescriptors(t); after != before {
		t.Fatalf("failed construction changed descriptors from %d to %d", before, after)
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHistorySpoolRejectsReplacedDirectory(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	payload := []byte("replaced payload")
	id := historySpoolTestSave(t, spool, payload)
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("save followed a replaced directory")
	}
	if _, _, _, err := spool.read(id); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("read followed a replaced directory")
	}
	if got := historySpoolTestEntries(t, dir); len(got) != 0 {
		t.Fatal("writes reached the replacement directory")
	}
}

func TestHistorySpoolSerializesConcurrentSaves(t *testing.T) {
	spool := historySpoolTestOpen(t, historySpoolTestDir(t))
	spool.maxBatches = 16
	const writers = 8
	ids := make([]string, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("concurrent payload %d", i))
			ids[i], errs[i] = spool.save(t.Context(), historySpoolTestNotice(payload), payload, historySpoolCanaryAccount)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := spool.pending()
	want := slices.Clone(ids)
	slices.Sort(want)
	if err != nil || !slices.Equal(got, want) {
		t.Fatal("concurrent saves were not all retained exactly once")
	}
}

func TestHistorySpoolPendingFailsClosedOnUnexpectedEntry(t *testing.T) {
	dir := historySpoolTestDir(t)
	spool := historySpoolTestOpen(t, dir)
	historySpoolTestSave(t, spool, []byte("intruder payload"))
	intruder := filepath.Join(dir, "unexpected.txt")
	historySpoolTestWrite(t, intruder, 0600, []byte("unexpected"))
	if _, err := spool.pending(); !errors.Is(err, errHistorySpoolUnavailable) {
		t.Fatal("unexpected entry was skipped silently")
	}
	if _, err := os.Stat(intruder); err != nil {
		t.Fatal("unexpected entry was removed")
	}
}
