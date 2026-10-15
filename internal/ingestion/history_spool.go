package ingestion

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

var (
	errHistorySpoolUnavailable = errors.New("history spool unavailable")
	errHistorySpoolLimit       = errors.New("history spool limit exceeded")
)

const (
	historySpoolVersion           = 1
	historySpoolKeySize           = 32
	historySpoolIDSize            = 16
	historySpoolNonceSize         = 12
	historySpoolTagSize           = 16
	historySpoolFieldPrefix       = 3 * 4
	historySpoolMaxAccount        = 256
	historySpoolMaxMetadata       = 64 << 10
	historySpoolMaxBatches        = 128
	historySpoolMaxBytes    int64 = 512 << 20
	historySpoolMaxFile     int64 = 1 + historySpoolNonceSize + historySpoolTagSize + historySpoolFieldPrefix + historySpoolMaxAccount + historySpoolMaxMetadata + compressedMax
)

// historySpool retains complete history batches as encrypted files before any
// projection. Each batch has a random name and is authenticated against that
// name. Methods are serialized, and close releases the held directory root.
type historySpool struct {
	mu         sync.Mutex
	dir        string
	root       *os.Root
	identity   os.FileInfo
	aead       cipher.AEAD
	closed     bool
	maxBatches int
	maxBytes   int64
	fault      func(stage string) error // test hook; nil in production
}

// openHistorySpool creates or validates a private spool directory and its key.
// Unexpected entries fail closed, and an existing key is never replaced.
func openHistorySpool(dir string) (*historySpool, error) {
	if err := privateDirectory(dir, true); err != nil {
		return nil, errHistorySpoolUnavailable
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errHistorySpoolUnavailable
	}
	spool := &historySpool{dir: dir, root: root, maxBatches: historySpoolMaxBatches, maxBytes: historySpoolMaxBytes}
	if err := spool.load(); err != nil {
		_ = root.Close()
		return nil, err
	}
	// Sync the containing directory too: syncing the new spool directory does
	// not by itself make that directory's name durable in its parent.
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		_ = root.Close()
		return nil, errHistorySpoolUnavailable
	}
	parentDir, err := parent.Open(".")
	if err == nil {
		err = parentDir.Sync()
		if closeErr := parentDir.Close(); err == nil {
			err = closeErr
		}
	}
	if closeErr := parent.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = root.Close()
		return nil, errHistorySpoolUnavailable
	}
	return spool, nil
}

func (s *historySpool) load() error {
	current, err := os.Lstat(s.dir)
	if err != nil {
		return errHistorySpoolUnavailable
	}
	held, err := s.root.Stat(".")
	if err != nil || !os.SameFile(current, held) || !privateSessionDirectory(held) {
		return errHistorySpoolUnavailable
	}
	s.identity = held
	state, err := s.scan()
	if err != nil {
		return err
	}
	if !state.key {
		if state.entries > 0 {
			return errHistorySpoolUnavailable
		}
		if err := s.createKey(); err != nil {
			return err
		}
	}
	key, err := s.loadKey()
	if err != nil {
		return err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return errHistorySpoolUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return errHistorySpoolUnavailable
	}
	s.aead = aead
	return nil
}

func (s *historySpool) createKey() error {
	key := make([]byte, historySpoolKeySize)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return errHistorySpoolUnavailable
	}
	id, err := newHistorySpoolID()
	if err != nil {
		return err
	}
	temp := id + ".tmp"
	if err := s.writeNew(temp, key); err != nil {
		return err
	}
	if _, err := s.root.Lstat("key"); !errors.Is(err, os.ErrNotExist) {
		return errHistorySpoolUnavailable
	}
	if err := s.root.Rename(temp, "key"); err != nil {
		return errHistorySpoolUnavailable
	}
	return s.syncDir()
}

func (s *historySpool) loadKey() ([]byte, error) {
	file, info, err := s.openLeaf("key", historySpoolKeySize)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info.Size() != historySpoolKeySize {
		return nil, errHistorySpoolUnavailable
	}
	key := make([]byte, historySpoolKeySize)
	if _, err := io.ReadFull(file, key); err != nil {
		clear(key)
		return nil, errHistorySpoolUnavailable
	}
	return key, nil
}

type historySpoolState struct {
	key     bool
	batches []string
	entries int
	bytes   int64
}

// scan validates every entry. Unknown names, links, widened modes, and oversize
// files fail closed; nothing is skipped, repaired, or deleted.
func (s *historySpool) scan() (historySpoolState, error) {
	var state historySpoolState
	dir, err := s.root.Open(".")
	if err != nil {
		return state, errHistorySpoolUnavailable
	}
	names, err := dir.Readdirnames(-1)
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil || len(names) > historySpoolMaxBatches+1 {
		return state, errHistorySpoolUnavailable
	}
	for _, name := range names {
		if name == "key" {
			info, err := s.statLeaf(name, historySpoolKeySize)
			if err != nil || info.Size() != historySpoolKeySize {
				return state, errHistorySpoolUnavailable
			}
			state.key = true
			continue
		}
		id, batch, ok := historySpoolEntry(name)
		if !ok {
			return state, errHistorySpoolUnavailable
		}
		info, err := s.statLeaf(name, historySpoolMaxFile)
		if err != nil {
			return state, err
		}
		state.entries++
		state.bytes += info.Size()
		if batch {
			state.batches = append(state.batches, id)
		}
	}
	slices.Sort(state.batches)
	return state, nil
}

func historySpoolEntry(name string) (string, bool, bool) {
	if id, found := strings.CutSuffix(name, ".batch"); found && validHistorySpoolID(id) {
		return id, true, true
	}
	if id, found := strings.CutSuffix(name, ".tmp"); found && validHistorySpoolID(id) {
		return id, false, true
	}
	return "", false, false
}

func validHistorySpoolID(id string) bool {
	if len(id) != 2*historySpoolIDSize {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func newHistorySpoolID() (string, error) {
	var raw [historySpoolIDSize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errHistorySpoolUnavailable
	}
	return hex.EncodeToString(raw[:]), nil
}

// openLeaf opens an existing private regular file under the held root without
// following links, then confirms that the descriptor is the same entry.
func (s *historySpool) openLeaf(name string, maxSize int64) (*os.File, os.FileInfo, error) {
	before, err := s.root.Lstat(name)
	if err != nil || !privateSessionFile(before) || before.Size() > maxSize {
		return nil, nil, errHistorySpoolUnavailable
	}
	file, err := s.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, errHistorySpoolUnavailable
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(before, info) || !privateSessionFile(info) || !sessionNoExtendedACL(file) {
		_ = file.Close()
		return nil, nil, errHistorySpoolUnavailable
	}
	return file, info, nil
}

func (s *historySpool) statLeaf(name string, maxSize int64) (os.FileInfo, error) {
	file, info, err := s.openLeaf(name, maxSize)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, errHistorySpoolUnavailable
	}
	return info, nil
}

// writeNew creates a private file and makes its bytes durable. A failed write
// leaves its residue in place; it is never deleted automatically.
func (s *historySpool) writeNew(name string, data []byte) error {
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errHistorySpoolUnavailable
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errHistorySpoolUnavailable
	}
	info, err := s.root.Lstat(name)
	if err != nil || !privateSessionFile(info) || info.Size() != int64(len(data)) {
		return errHistorySpoolUnavailable
	}
	return nil
}

func (s *historySpool) syncDir() error {
	dir, err := s.root.Open(".")
	if err != nil {
		return errHistorySpoolUnavailable
	}
	err = dir.Sync()
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errHistorySpoolUnavailable
	}
	return nil
}

func (s *historySpool) checkpoint(stage string) error {
	if s.fault != nil && s.fault(stage) != nil {
		return errHistorySpoolUnavailable
	}
	return nil
}

func (s *historySpool) checkLocked() error {
	if s.closed {
		return errHistorySpoolUnavailable
	}
	current, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(current, s.identity) || !privateSessionDirectory(current) {
		return errHistorySpoolUnavailable
	}
	held, err := s.root.Stat(".")
	if err != nil || !os.SameFile(held, s.identity) || !privateSessionDirectory(held) {
		return errHistorySpoolUnavailable
	}
	return nil
}

// save durably stores one complete compressed batch before any projection. For
// a downloaded batch, a notification digest must match compressed when present;
// an inline batch must equal its inline payload, which is stored only once.
// Nil or oversize bytes are refused, and no envelope is kept without bytes.
func (s *historySpool) save(ctx context.Context, notification *waE2E.HistorySyncNotification, compressed []byte, account string) (string, error) {
	if ctx == nil || notification == nil || !validHistorySpoolAccount(account) || len(compressed) == 0 {
		return "", errHistorySpoolUnavailable
	}
	if int64(len(compressed)) > compressedMax {
		return "", errHistorySpoolLimit
	}
	meta, err := historySpoolMetadata(notification, compressed)
	if err != nil {
		return "", err
	}
	plaintext := historySpoolEnvelope(account, meta, compressed)
	defer clear(plaintext)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	state, err := s.scan()
	if err != nil {
		return "", err
	}
	if state.entries+1 > s.maxBatches || state.bytes+historySpoolFileSize(len(plaintext)) > s.maxBytes {
		return "", errHistorySpoolLimit
	}
	id, err := newHistorySpoolID()
	if err != nil {
		return "", err
	}
	wire, err := s.seal(id, plaintext)
	if err != nil {
		return "", err
	}
	if err := s.commitLocked(id, wire); err != nil {
		return "", err
	}
	return id, nil
}

func historySpoolMetadata(notification *waE2E.HistorySyncNotification, compressed []byte) ([]byte, error) {
	if inline := notification.GetInitialHistBootstrapInlinePayload(); len(inline) != 0 {
		if !bytes.Equal(inline, compressed) {
			return nil, errHistorySpoolUnavailable
		}
	} else if digest := notification.GetFileSHA256(); len(digest) != 0 {
		sum := sha256.Sum256(compressed)
		if len(digest) != sha256.Size || !bytes.Equal(digest, sum[:]) {
			return nil, errHistorySpoolUnavailable
		}
	}
	stripped := proto.Clone(notification).(*waE2E.HistorySyncNotification)
	stripped.InitialHistBootstrapInlinePayload = nil
	meta, err := proto.Marshal(stripped)
	if err != nil {
		return nil, errHistorySpoolUnavailable
	}
	if len(meta) > historySpoolMaxMetadata {
		return nil, errHistorySpoolLimit
	}
	return meta, nil
}

func validHistorySpoolAccount(account string) bool {
	if account == "" || len(account) > historySpoolMaxAccount {
		return false
	}
	for i := 0; i < len(account); i++ {
		if account[i] < 0x20 || account[i] == 0x7f {
			return false
		}
	}
	return true
}

func historySpoolEnvelope(account string, meta, compressed []byte) []byte {
	out := make([]byte, 0, historySpoolFieldPrefix+len(account)+len(meta)+len(compressed))
	for _, field := range [][]byte{[]byte(account), meta, compressed} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
		out = append(out, field...)
	}
	return out
}

func historySpoolDecodeEnvelope(plaintext []byte) (string, []byte, []byte, bool) {
	account, rest, ok := historySpoolTakeField(plaintext, historySpoolMaxAccount)
	if !ok {
		return "", nil, nil, false
	}
	meta, rest, ok := historySpoolTakeField(rest, historySpoolMaxMetadata)
	if !ok {
		return "", nil, nil, false
	}
	compressed, rest, ok := historySpoolTakeField(rest, compressedMax)
	if !ok || len(rest) != 0 || len(compressed) == 0 || !validHistorySpoolAccount(string(account)) {
		return "", nil, nil, false
	}
	return string(account), meta, compressed, true
}

func historySpoolTakeField(buf []byte, limit int64) ([]byte, []byte, bool) {
	if len(buf) < 4 {
		return nil, nil, false
	}
	size := int64(binary.BigEndian.Uint32(buf))
	buf = buf[4:]
	if size > limit || size > int64(len(buf)) {
		return nil, nil, false
	}
	return buf[:size], buf[size:], true
}

func historySpoolFileSize(plaintextLen int) int64 {
	return int64(1 + historySpoolNonceSize + plaintextLen + historySpoolTagSize)
}

func historySpoolAAD(id string) []byte {
	return []byte("wa-indexer/history-spool/v1\x00" + id)
}

func (s *historySpool) seal(id string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, historySpoolNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errHistorySpoolUnavailable
	}
	wire := append([]byte{historySpoolVersion}, nonce...)
	return s.aead.Seal(wire, nonce, plaintext, historySpoolAAD(id)), nil
}

// commitLocked publishes a batch only after its temporary file is synced. A
// batch whose directory sync fails stays visible to pending rather than vanishing.
func (s *historySpool) commitLocked(id string, wire []byte) error {
	temp, name := id+".tmp", id+".batch"
	if err := s.writeNew(temp, wire); err != nil {
		return err
	}
	if err := s.checkpoint("temp-synced"); err != nil {
		return err
	}
	if _, err := s.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return errHistorySpoolUnavailable
	}
	if err := s.root.Rename(temp, name); err != nil {
		return errHistorySpoolUnavailable
	}
	if err := s.checkpoint("renamed"); err != nil {
		return err
	}
	return s.syncDir()
}

// read verifies and decrypts one retained batch for local replay or tests. Its
// output is not source authorization; callers must apply live checks again.
func (s *historySpool) read(id string) (*waE2E.HistorySyncNotification, []byte, string, error) {
	if !validHistorySpoolID(id) {
		return nil, nil, "", errHistorySpoolUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, nil, "", err
	}
	file, _, err := s.openLeaf(id+".batch", historySpoolMaxFile)
	if err != nil {
		return nil, nil, "", err
	}
	wire, err := io.ReadAll(io.LimitReader(file, historySpoolMaxFile+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil || int64(len(wire)) > historySpoolMaxFile || len(wire) < 1+historySpoolNonceSize+historySpoolTagSize || wire[0] != historySpoolVersion {
		return nil, nil, "", errHistorySpoolUnavailable
	}
	plaintext, err := s.aead.Open(nil, wire[1:1+historySpoolNonceSize], wire[1+historySpoolNonceSize:], historySpoolAAD(id))
	if err != nil {
		return nil, nil, "", errHistorySpoolUnavailable
	}
	defer clear(plaintext)
	account, meta, compressed, ok := historySpoolDecodeEnvelope(plaintext)
	if !ok {
		return nil, nil, "", errHistorySpoolUnavailable
	}
	notification := new(waE2E.HistorySyncNotification)
	if err := proto.Unmarshal(bytes.Clone(meta), notification); err != nil || len(notification.GetInitialHistBootstrapInlinePayload()) != 0 {
		return nil, nil, "", errHistorySpoolUnavailable
	}
	return notification, bytes.Clone(compressed), account, nil
}

// remove deletes exactly one named batch after its owner has finished with it.
// It never accepts paths, and the directory sync confirms the deletion.
func (s *historySpool) remove(id string) error {
	if !validHistorySpoolID(id) {
		return errHistorySpoolUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	name := id + ".batch"
	file, info, err := s.openLeaf(name, historySpoolMaxFile)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return errHistorySpoolUnavailable
	}
	current, err := s.root.Lstat(name)
	if err != nil || !os.SameFile(current, info) {
		return errHistorySpoolUnavailable
	}
	if err := s.root.Remove(name); err != nil {
		return errHistorySpoolUnavailable
	}
	return s.syncDir()
}

// pending lists retained batch identifiers in sorted order. Any unexpected
// entry makes the listing fail rather than being skipped.
func (s *historySpool) pending() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, err
	}
	state, err := s.scan()
	if err != nil {
		return nil, err
	}
	return state.batches, nil
}

// close releases the held directory root. It is idempotent; later calls to the
// other methods fail closed.
func (s *historySpool) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.aead = nil
	root := s.root
	s.root = nil
	if err := root.Close(); err != nil {
		return errHistorySpoolUnavailable
	}
	return nil
}
