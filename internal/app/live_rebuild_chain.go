package app

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// LiveRebuildSourceGuards owns every predecessor until destination workers join.
// All paths are derived from privately sealed receipts, never from descriptors.
type LiveRebuildSourceGuards struct {
	mu         sync.Mutex
	leases     []*appdata.InstanceLock
	roots      []string
	provenance archive.RebuildProvenance
}

func (g *LiveRebuildSourceGuards) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, lease := range g.leases {
		if err := lease.Close(); err != nil {
			return err
		}
	}
	return nil
}

type liveRebuildChainEdge struct {
	paths   appdata.Paths
	receipt LiveRebuildBootstrapReceipt
}
type liveRebuildChainFile struct{ path, digest string }
type liveRebuildChain struct {
	edges  []liveRebuildChainEdge
	files  []liveRebuildChainFile
	absent []string
}

func liveRebuildLoadReady(root string) (*LiveRebuildBootstrapReceipt, []byte, error) {
	if _, err := os.Lstat(filepath.Join(root, liveRebuildPendingFile)); !errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrLiveRebuildBootstrap
	}
	name := filepath.Join(root, liveRebuildReceiptFile)
	if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	data, err := liveRebuildReadPrivate(name, 131072)
	if err != nil {
		return nil, nil, err
	}
	var r LiveRebuildBootstrapReceipt
	if liveRebuildDecode(data, &r) != nil || r.Version != 1 || r.DestinationProfileDir != root || r.SourceProfileDir == root || r.Archive.Generation == "" || r.Archive.Account == "" || r.Archive.DeviceID == "" || r.Archive.Boundary.IsZero() || r.Archive.Boundary.After(time.Now()) || !liveRebuildValidDigest(r.Archive.SourceSHA256) {
		return nil, nil, ErrLiveRebuildBootstrap
	}
	return &r, data, nil
}

func liveRebuildRootsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

func liveRebuildResolveChain(root string) (*liveRebuildChain, error) {
	paths, err := appdata.ExistingPaths(root)
	if err != nil || paths.Root != root {
		return nil, ErrLiveRebuildBootstrap
	}
	r, raw, err := liveRebuildLoadReady(root)
	if err != nil || r == nil {
		return nil, err
	}
	c := &liveRebuildChain{}
	seen := map[string]bool{root: true}
	generations := map[string]bool{r.Archive.Generation: true}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, ErrLiveRebuildBootstrap
	}
	inodes := []os.FileInfo{info}
	for {
		if len(c.edges) >= archive.MaxForwardAncestorProfiles {
			return nil, ErrLiveRebuildBootstrap
		}
		c.files = append(c.files, liveRebuildChainFile{filepath.Join(r.DestinationProfileDir, liveRebuildReceiptFile), liveRebuildDigest(raw)})
		c.absent = append(c.absent, filepath.Join(r.DestinationProfileDir, liveRebuildPendingFile))
		if seen[r.SourceProfileDir] || !filepath.IsAbs(r.SourceProfileDir) || filepath.Clean(r.SourceProfileDir) != r.SourceProfileDir {
			return nil, ErrLiveRebuildBootstrap
		}
		for prior := range seen {
			if liveRebuildRootsOverlap(prior, r.SourceProfileDir) {
				return nil, ErrLiveRebuildBootstrap
			}
		}
		source, err := appdata.ExistingPaths(r.SourceProfileDir)
		if err != nil || source.Root != r.SourceProfileDir {
			return nil, ErrLiveRebuildBootstrap
		}
		stat, err := os.Lstat(source.Root)
		if err != nil {
			return nil, ErrLiveRebuildBootstrap
		}
		for _, prior := range inodes {
			if os.SameFile(prior, stat) {
				return nil, ErrLiveRebuildBootstrap
			}
		}
		inodes = append(inodes, stat)
		seen[source.Root] = true
		cfg := LiveRebuildBootstrapConfig{SourceProfileDir: source.Root, DestinationProfileDir: r.DestinationProfileDir, ExpectedBackupSHA256: r.BackupSHA256, ExpectedDeviceID: r.Archive.DeviceID, SourceStoppedGuardPath: appdata.LiveRebuildSourceGuardPath(source.Root), SourceStoppedGuardSHA256: r.GuardSHA256}
		if err := liveRebuildReadGuard(cfg); err != nil {
			return nil, err
		}
		c.files = append(c.files, liveRebuildChainFile{cfg.SourceStoppedGuardPath, r.GuardSHA256})
		c.edges = append(c.edges, liveRebuildChainEdge{source, *r})
		parent, parentRaw, err := liveRebuildLoadReady(source.Root)
		if err != nil {
			return nil, err
		}
		c.absent = append(c.absent, filepath.Join(source.Root, liveRebuildPendingFile))
		if parent == nil {
			if r.SourceReceiptSHA256 != "" || len(r.Archive.Ancestors) != 0 {
				return nil, ErrLiveRebuildBootstrap
			}
			c.absent = append(c.absent, filepath.Join(source.Root, liveRebuildReceiptFile))
			break
		}
		expected := append([]archive.RebuildProvenance{parent.Archive.RebuildProvenance}, parent.Archive.Ancestors...)
		if !liveRebuildValidDigest(r.SourceReceiptSHA256) || liveRebuildDigest(parentRaw) != r.SourceReceiptSHA256 || r.Files[liveRebuildReceiptFile] != r.SourceReceiptSHA256 || !reflect.DeepEqual(r.Archive.Ancestors, expected) || generations[parent.Archive.Generation] || parent.Archive.Account != r.Archive.Account || parent.Archive.DeviceID != r.Archive.DeviceID || parent.Archive.Boundary.After(r.Archive.Boundary) {
			return nil, ErrLiveRebuildBootstrap
		}
		generations[parent.Archive.Generation] = true
		r, raw = parent, parentRaw
	}
	return c, nil
}
func (c *liveRebuildChain) verify() error {
	for _, f := range c.files {
		data, err := liveRebuildReadPrivate(f.path, 131072)
		if err != nil || liveRebuildDigest(data) != f.digest {
			return ErrLiveRebuildBootstrap
		}
	}
	for _, name := range c.absent {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return ErrLiveRebuildBootstrap
		}
	}
	return nil
}

// The first descriptor retains compatibility with the immediate predecessor.
// Additional descriptors are matched to required ancestors by their lock inode.
func AcquireLiveRebuildSourceGuard(root string, immediateFD *int, ancestorFDs ...int) (*LiveRebuildSourceGuards, error) {
	if len(ancestorFDs) >= archive.MaxForwardAncestorProfiles {
		return nil, ErrLiveRebuildBootstrap
	}
	chain, err := liveRebuildResolveChain(root)
	if err != nil {
		return nil, err
	}
	if chain == nil {
		if immediateFD != nil || len(ancestorFDs) != 0 {
			return nil, ErrLiveRebuildBootstrap
		}
		return nil, nil
	}
	seenFD := map[int]bool{}
	if immediateFD != nil {
		seenFD[*immediateFD] = true
	}
	for _, fd := range ancestorFDs {
		if fd < 3 || fd > 1048576 || seenFD[fd] {
			return nil, ErrLiveRebuildBootstrap
		}
		seenFD[fd] = true
	}
	g := &LiveRebuildSourceGuards{provenance: chain.edges[0].receipt.Archive.RebuildProvenance, leases: make([]*appdata.InstanceLock, len(chain.edges))}
	for _, edge := range chain.edges {
		g.roots = append(g.roots, edge.paths.Root)
	}
	fail := func(err error) (*LiveRebuildSourceGuards, error) { return nil, errors.Join(err, g.Close()) }
	if immediateFD != nil {
		g.leases[0], err = appdata.AcquireInheritedExisting(chain.edges[0].paths, *immediateFD)
		if err != nil {
			return fail(err)
		}
	}
	for _, fd := range ancestorFDs {
		matched := false
		for i := 1; i < len(chain.edges); i++ {
			if g.leases[i] != nil {
				continue
			}
			lease, e := appdata.AcquireInheritedExisting(chain.edges[i].paths, fd)
			if e == nil {
				g.leases[i] = lease
				matched = true
				break
			}
		}
		if !matched {
			return fail(ErrLiveRebuildBootstrap)
		}
	}
	for i, edge := range chain.edges {
		if g.leases[i] == nil {
			g.leases[i], err = appdata.AcquireExisting(edge.paths)
			if err != nil {
				return fail(err)
			}
		}
	}
	if err := chain.verify(); err != nil {
		return fail(err)
	}
	return g, nil
}
