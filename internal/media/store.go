package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"golang.org/x/sys/unix"
)

type Store struct {
	mu               sync.Mutex
	operation        sync.RWMutex
	repo             archive.MediaStore
	options          Options
	media, working   *directory
	owner            *os.File
	active           int
	pending          map[archive.ArtifactID]*pendingSpace
	residents        map[archive.ArtifactID]stoppedOwner
	stopping, closed bool
}
type pendingSpace struct {
	lease   diskspace.Lease
	cleaned bool
}
type stoppedOwner interface{ recoverStopped() error }

func (s *Store) register(id archive.ArtifactID, o stoppedOwner) {
	s.mu.Lock()
	if s.residents == nil {
		s.residents = map[archive.ArtifactID]stoppedOwner{}
	}
	s.residents[id] = o
	s.mu.Unlock()
}
func (s *Store) unregister(id archive.ArtifactID) {
	s.mu.Lock()
	delete(s.residents, id)
	s.mu.Unlock()
}
func (s *Store) recoverStopped() error {
	s.mu.Lock()
	owners := make([]stoppedOwner, 0, len(s.residents))
	for _, o := range s.residents {
		owners = append(owners, o)
	}
	s.mu.Unlock()
	for _, o := range owners {
		if err := o.recoverStopped(); err != nil && !errors.Is(err, ErrActiveUse) {
			return err
		}
	}
	return nil
}

func (s *Store) beginRecovery() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.active++
	return nil
}

func Open(paths appdata.Paths, repo archive.MediaStore, o Options) (*Store, error) {
	if repo == nil || o.Admission == nil || paths.Root == "" || paths.MediaDir != filepath.Join(paths.Root, "media") || paths.WorkingDir != filepath.Join(paths.Root, "working") {
		return nil, ErrInvalidRequest
	}
	if o.ImageMaximumBytes == 0 {
		o.ImageMaximumBytes = 10 << 20
	}
	if o.VoiceMaximumBytes == 0 {
		o.VoiceMaximumBytes = 25 << 20
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = 2 * time.Minute
	}
	if o.ImageMaximumBytes < 1 || o.VoiceMaximumBytes < 1 || o.ImageMaximumBytes > maximumMediaBytes || o.VoiceMaximumBytes > maximumMediaBytes || o.LeaseTTL < time.Second || o.LeaseTTL > time.Hour {
		return nil, ErrInvalidRequest
	}
	root, err := openDirectory(paths.Root)
	if err != nil {
		return nil, err
	}
	defer root.close()
	if err := checkFilesystem(root.file, o.ExpectedDevice); err != nil {
		return nil, err
	}
	root.expectedDevice = o.ExpectedDevice
	marker, err := root.open(".wa-indexer-owner", unix.O_RDONLY)
	if err != nil {
		return nil, ErrUnsafeFile
	}
	b, err := io.ReadAll(io.LimitReader(marker, 64))
	_ = marker.Close()
	if err != nil || string(b) != "wa-indexer-private-data-v1\n" {
		return nil, ErrUnsafeFile
	}
	media, err := root.subdirectory("media")
	if err != nil {
		return nil, err
	}
	working, err := root.subdirectory("working")
	if err != nil {
		_ = media.close()
		return nil, err
	}
	for _, d := range []*directory{media, working} {
		if err := checkFilesystem(d.file, o.ExpectedDevice); err != nil {
			_ = working.close()
			_ = media.close()
			return nil, err
		}
	}
	owner, err := artifactLock(context.Background(), media, "media-owner.lock", true, false)
	if err != nil {
		_ = working.close()
		_ = media.close()
		return nil, err
	}
	return &Store{repo: repo, options: o, media: media, working: working, owner: owner}, nil
}

func (s *Store) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.closed {
		return ErrClosed
	}
	s.active++
	return nil
}
func (s *Store) end() { s.mu.Lock(); s.active--; s.mu.Unlock() }
func (s *Store) transition(id archive.ArtifactID, phase string) {
	if s.options.OnTransition != nil {
		s.options.OnTransition(Transition{ArtifactID: id, Phase: phase})
	}
}
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.mu.Unlock()
	// Keep failed Close ownership reachable after a connector/worker drops its
	// local handle. Retry only objects whose writers have already stopped.
	if err := s.recoverStopped(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.stopping = true
	if s.active != 0 || len(s.pending) != 0 {
		return ErrActiveUse
	}
	s.closed = true
	return errors.Join(releaseLock(s.owner), s.working.close(), s.media.close())
}

func artifactName(id archive.ArtifactID, suffix string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + suffix
}

// Original retains the verified inode and its cleanup guard. Callers must close
// every Open descriptor after the complete native process group has exited,
// then Close the Original. Close refuses live duplicated descriptors.
type Original struct {
	mu          sync.Mutex
	store       *Store
	file, guard *os.File
	readers     *os.File
	children    []*os.File
	metadata    OriginalMetadata
	closed      bool
	exported    bool
}

func (s *Store) OpenOriginal(ctx context.Context, scope archive.ReadScope, id archive.MediaID) (*Original, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			s.end()
		}
	}()
	a, err := s.repo.AuthorizeOriginal(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if a.ID == "" || a.Budget != archive.BudgetOriginals || a.State != "retained" || a.Bytes <= 0 || a.Bytes > maximumMediaBytes {
		return nil, ErrUnavailable
	}
	readers, err := artifactLock(ctx, s.media, "media-readers.lock", false, true)
	if err != nil {
		return nil, err
	}
	guard, err := artifactLock(ctx, s.media, artifactName(a.ID, ".lock"), false, true)
	if err != nil {
		_ = releaseLock(readers)
		return nil, err
	}
	f, err := s.media.open(artifactName(a.ID, ".original"), unix.O_RDONLY)
	if err != nil {
		_ = releaseLock(guard)
		_ = releaseLock(readers)
		return nil, err
	}
	closeFail := func(err error) (*Original, error) {
		_ = f.Close()
		_ = releaseLock(guard)
		_ = releaseLock(readers)
		return nil, err
	}
	digest, size, format, err := verifyMediaFile(ctx, f, "", a.Bytes)
	if err != nil {
		return closeFail(err)
	}
	if size != a.Bytes || digest != a.SHA256 {
		return closeFail(ErrAuthentication)
	}
	current, err := s.repo.AuthorizeOriginal(ctx, scope, id)
	if err != nil {
		return closeFail(err)
	}
	if current.ID != a.ID || current.SHA256 != a.SHA256 || current.Bytes != a.Bytes || !sameSource(current.Source, a.Source) {
		return closeFail(ErrSourceChanged)
	}
	failed = false
	return &Original{store: s, file: f, guard: guard, readers: readers, metadata: OriginalMetadata{MediaID: id, ArtifactID: a.ID, Source: a.Source, SHA256: digest, Bytes: size, Format: format}}, nil
}
func (o *Original) Metadata() OriginalMetadata { return o.metadata }
func (o *Original) Reader() io.ReaderAt {
	o.mu.Lock()
	defer o.mu.Unlock()
	return io.NewSectionReader(o.file, 0, o.metadata.Bytes)
}
func (o *Original) Open() (*os.File, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, os.ErrClosed
	}
	f, err := o.store.media.open(artifactName(o.metadata.ArtifactID, ".original"), unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	if err := sameInode(f, o.file); err != nil {
		_ = f.Close()
		return nil, err
	}
	o.children = append(o.children, f)
	return f, nil
}

// WorkerGuards returns artifact then global reader guard duplicates of the
// same shared flock OFDs. The trusted independent worker holder owns these
// through full process-group exit, even if the app dies. Recipients must close
// the descriptors without LOCK_UN; they are independent of Original.Close.
func (o *Original) WorkerGuards() ([]*os.File, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, os.ErrClosed
	}
	if err := o.store.media.same(artifactName(o.metadata.ArtifactID, ".original"), o.file); err != nil {
		return nil, err
	}
	inputs := []*os.File{o.guard, o.readers}
	names := []string{artifactName(o.metadata.ArtifactID, ".lock"), "media-readers.lock"}
	guards := make([]*os.File, 0, len(inputs))
	fail := func(err error) ([]*os.File, error) {
		for _, guard := range guards {
			_ = guard.Close()
		}
		return nil, err
	}
	for i, input := range inputs {
		if err := o.store.media.same(names[i], input); err != nil {
			return fail(err)
		}
		fd, err := unix.FcntlInt(input.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return fail(err)
		}
		guard := os.NewFile(uintptr(fd), "original-worker-guard")
		guards = append(guards, guard)
		if err := o.store.media.same(names[i], guard); err != nil {
			return fail(err)
		}
	}
	o.exported = true
	return guards, nil
}

func (o *Original) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	if liveChildren(o.children) {
		return ErrActiveUse
	}
	o.closed = true
	var err error
	if o.exported {
		// Explicit unlock would drop the independent holder's shared OFDs too.
		err = errors.Join(o.file.Close(), o.guard.Close(), o.readers.Close())
	} else {
		err = errors.Join(o.file.Close(), releaseLock(o.guard), releaseLock(o.readers))
	}
	o.store.end()
	return err
}
func sameInode(a, b *os.File) error {
	aa, e := a.Stat()
	if e != nil {
		return e
	}
	bb, e := b.Stat()
	if e != nil {
		return e
	}
	if !os.SameFile(aa, bb) {
		return ErrUnsafeFile
	}
	return nil
}
func liveChildren(files []*os.File) bool {
	for _, f := range files {
		if f.Fd() != ^uintptr(0) {
			return true
		}
	}
	return false
}
