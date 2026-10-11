package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/inference"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
)

func TestMaintenanceDoesNotConstructWhatsAppConnector(t *testing.T) {
	called := false
	a, err := newApplication(context.Background(), Config{DataDir: privateTestRoot(t), Offline: true}, func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		called = true
		return nil, errors.New("must_not_construct_connector")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	if called {
		t.Fatal("maintenance opened the WhatsApp runtime")
	}
}

func TestSharedComputePausePersistsAcrossRestart(t *testing.T) {
	root := privateTestRoot(t)
	a, err := New(context.Background(), Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.resources.compute.Snapshot().Paused {
		t.Fatal("persisted pause did not stop compute admission")
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err = New(context.Background(), Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	if !a.resources.compute.Snapshot().Paused {
		t.Fatal("restart lost compute pause")
	}
	if err = a.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.resources.compute.Snapshot().Paused {
		t.Fatal("explicit resume left admission paused")
	}
}

func TestUnacceptedNativeProfileStillServesKeyword(t *testing.T) {
	called := false
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Native: &NativeConfig{Profile: inference.ReferenceProfile(), WorkerExecutable: "/private/unused-worker", ModelProfileID: "reference", RuntimeLease: func(context.Context) (inference.ComponentLease, error) {
		called = true
		return nil, inference.ErrUnavailable
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.seedDemo(context.Background(), policy.Generation); err != nil {
		t.Fatal(err)
	}
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 20})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("native failure blocked keyword search: %v", err)
	}
	if called {
		t.Fatal("unaccepted profile acquired or loaded native components")
	}
}

type delayedNativeCleanup struct{ done chan struct{} }

func (*delayedNativeCleanup) Close() error { return inference.ErrCleanup }
func (n *delayedNativeCleanup) AwaitCleanup(ctx context.Context) error {
	select {
	case <-n.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*delayedNativeCleanup) Unload(context.Context) error { return nil }
func (*delayedNativeCleanup) EncodeQuery(context.Context, string) ([]float32, error) {
	return nil, inference.ErrUnavailable
}
func (*delayedNativeCleanup) EncodePassages(context.Context, []string) ([][]float32, error) {
	return nil, inference.ErrUnavailable
}
func (*delayedNativeCleanup) TokenizerIdentity() inference.TokenizerIdentity {
	return inference.TokenizerIdentity{}
}
func (*delayedNativeCleanup) CountPassage(context.Context, string) (int, error) {
	return 0, inference.ErrUnavailable
}
func (*delayedNativeCleanup) SplitPassage(context.Context, string, int) ([]inference.TextSpan, error) {
	return nil, inference.ErrUnavailable
}

func TestShutdownRetainsInstanceAndArchiveUntilNativeCleanupAcknowledgement(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	native := &delayedNativeCleanup{done: make(chan struct{})}
	a.resources.native = native
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = a.Close(ctx); err == nil {
		t.Fatal("shutdown acknowledged a surviving native owner")
	}
	if a.store == nil || a.closed {
		t.Fatal("shutdown dropped archive ownership before native cleanup")
	}
	if _, e := a.resources.disk.Reserve(context.Background(), diskspace.Request{Class: diskspace.Decoded, MaximumGrowthBytes: 1}); !errors.Is(e, diskspace.ErrClosed) {
		t.Fatal("shutdown admitted fresh physical growth")
	}
	if lease, err := appdata.Acquire(a.paths); err == nil {
		lease.Close()
		t.Fatal("another instance acquired native-owned data")
	}
	close(native.done)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(a.paths)
	if err != nil {
		t.Fatal("cleanup acknowledgement did not release instance ownership")
	}
	lease.Close()
}

func TestDeleteResourceReopenFailureClosesNewArchiveBeforeInstanceRelease(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	a.openResources = func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error) {
		return nil, errors.New("resource_reopen_failed")
	}
	if err = a.DeleteLocalData(context.Background(), true); err == nil {
		t.Fatal("resource failure was hidden")
	}
	if a.store != nil {
		t.Fatal("new archive owner leaked after resource failure")
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(a.paths)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	store, err := archive.Open(context.Background(), a.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal("archive ownership leaked")
	}
	store.Close()
}

func TestShutdownRetainsInstanceUntilIndexRunJoined(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	a.resources.runDone = done
	a.resources.runCancel = func() {}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = a.Close(ctx); err == nil {
		t.Fatal("unjoined indexing owner acknowledged")
	}
	if a.store == nil || a.closed {
		t.Fatal("live indexing released archive")
	}
	close(done)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type delayedExtractionCleanup struct{ done chan struct{} }

func (e *delayedExtractionCleanup) Close(ctx context.Context) error {
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestShutdownRetainsArchiveUntilExtractionCleanupAcknowledged(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	e := &delayedExtractionCleanup{done: make(chan struct{})}
	a.resources.extraction = e
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = a.Close(ctx); err == nil {
		t.Fatal("surviving extraction acknowledged")
	}
	if a.store == nil || a.closed {
		t.Fatal("extraction released archive ownership")
	}
	close(e.done)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Constructor failure cannot hand the instance to a second writer while a
// partial resource owner still awaits its terminal cleanup acknowledgement.
func TestConstructorFailureRetainsInstanceUntilCleanupAcknowledged(t *testing.T) {
	root := privateTestRoot(t)
	entered := make(chan struct{})
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := newApplicationWithResources(context.Background(), Config{DataDir: root, Offline: true}, nil,
			func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error) {
				close(entered)
				return &resources{native: &delayedNativeCleanup{done: ready}}, errors.New("constructor_failure")
			})
		result <- err
	}()
	<-entered
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := appdata.Acquire(paths); err == nil {
		lease.Close()
		t.Fatal("constructor released instance while cleanup remained owned")
	}
	select {
	case <-result:
		t.Fatal("constructor returned before cleanup acknowledgement")
	default:
	}
	close(ready)
	if err := <-result; err == nil {
		t.Fatal("constructor lost original failure")
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

type delayedConstructorConnector struct {
	*syntheticConnector
	acknowledged chan struct{}
	closeStarted chan struct{}
	once         sync.Once
}

func (c *delayedConstructorConnector) Close() error {
	c.once.Do(func() { close(c.closeStarted) })
	select {
	case <-c.acknowledged:
		return nil
	default:
		return errors.New("session_cleanup_pending")
	}
}
func (c *delayedConstructorConnector) Quiesced() bool {
	select {
	case <-c.acknowledged:
		return true
	default:
		return false
	}
}
func TestPartialConnectorFailureRetainsInstanceUntilSessionAcknowledgement(t *testing.T) {
	root := privateTestRoot(t)
	partial := &delayedConstructorConnector{syntheticConnector: &syntheticConnector{}, acknowledged: make(chan struct{}), closeStarted: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := newApplicationWithResources(context.Background(), Config{DataDir: root},
			func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
				return partial, errors.New("partial_connector_failure")
			},
			func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error) {
				return &resources{}, nil
			})
		result <- err
	}()
	<-partial.closeStarted
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := appdata.Acquire(paths); err == nil {
		lease.Close()
		t.Fatal("instance released with live session owner")
	}
	select {
	case <-result:
		t.Fatal("constructor returned before session acknowledgement")
	default:
	}
	close(partial.acknowledged)
	if err := <-result; err == nil {
		t.Fatal("constructor lost partial factory failure")
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestMediaModelAcquireFailureCleanupKeepsNilLease(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}()
	for _, tc := range []struct {
		name   string
		cancel bool
		want   error
	}{
		{"cancelled", true, context.Canceled},
		{"not_installed", false, modelstore.ErrNotInstalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			lease, directErr := a.resources.models.Acquire(ctx, "ivrit-whisper-large-v3-turbo")
			if lease != nil {
				_ = lease.Close()
				t.Fatal("empty fixture unexpectedly acquired a model owner")
			}
			if !errors.Is(directErr, tc.want) {
				t.Fatalf("fixture failed before expected acquisition boundary: %v", directErr)
			}
			returnedNil := false
			gotErr, panicked := func() (resultErr error, panicked bool) {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				lease, err := a.resources.acquireMediaModel(ctx, "ivrit-whisper-large-v3-turbo")
				returnedNil = lease == nil
				// Match extraction's ownership rule: retain a nonnil owner even
				// when acquisition also returns an error, then close on return.
				if lease != nil {
					defer lease.Close()
				}
				return err, false
			}()
			if !returnedNil {
				t.Error("nil concrete model acquisition returned a nonnil lease interface")
			}
			if panicked {
				t.Fatal("nil model acquisition became an owned interface and panicked during cleanup")
			}
			if !errors.Is(gotErr, tc.want) {
				t.Fatalf("adapter changed the original acquisition error: %v", gotErr)
			}
		})
	}
}
