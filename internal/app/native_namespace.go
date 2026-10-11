package app

import (
	"context"
	"errors"
	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
	"os"
	"path/filepath"
)

var errNativeNamespace = errors.New("native_executable_namespace_unavailable")

type executableNamespace interface {
	workerlife.ExecutableProvider
	Recover(context.Context) error
	Close() error
}

// Only the app-owned activation boundary calls this after acquired-instance
// proof and the complete ordered extraction inventory. No request selects paths.
func (r *resources) initializeExecutableNamespace(ctx context.Context, paths appdata.Paths) error {
	if r.recovery == nil || !r.recovery.complete || r.instanceGuard == nil {
		return errExtractionRecovery
	}
	if r.namespace != nil {
		if !r.namespaceReady {
			if err := r.namespace.Recover(ctx); err != nil {
				return errNativeNamespace
			}
			r.namespaceReady = true
		}
		return r.ackNamespaceBootstrap(paths)
	}
	var namespace executableNamespace
	err := appdata.WithWorking(paths, r.instanceGuard, func(root *os.Root) error {
		info, err := root.Lstat("native-exec")
		if errors.Is(err, os.ErrNotExist) {
			// Exact EOF permits this single bounded bootstrap while other growth remains
			// held. Recover itself never allocates or calls the admission provider.
			if r.namespaceBootstrap == nil {
				lease, reserveErr := r.disk.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 64 << 10})
				if reserveErr != nil {
					return errNativeNamespace
				}
				r.namespaceBootstrap = lease
			}
			mkdir := r.namespaceMkdir
			if mkdir == nil {
				mkdir = func(root *os.Root) error { return root.Mkdir("native-exec", 0700) }
			}
			if err = mkdir(root); err != nil {
				return errNativeNamespace
			}
			info, err = root.Lstat("native-exec")
		}
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errNativeNamespace
		}
		directory, err := root.Open("native-exec")
		if err != nil {
			return errNativeNamespace
		}
		opened, statErr := directory.Stat()
		physical, physicalErr := r.disk.Snapshot()
		if statErr != nil || physicalErr != nil || !os.SameFile(info, opened) || !namespaceDeviceMatches(opened, physical.Device) {
			directory.Close()
			return errNativeNamespace
		}
		concrete, newErr := workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{
			Directory: directory, Path: filepath.Join(paths.WorkingDir, "native-exec"),
			Admit: func(ctx context.Context, growth int64) (workerlife.GrowthLease, error) {
				return r.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: growth})
			},
		})
		closeErr := directory.Close()
		if concrete != nil {
			namespace = concrete
			r.namespace = concrete
		}
		if newErr != nil || closeErr != nil {
			return errNativeNamespace
		}
		return nil
	})
	if err != nil {
		return errNativeNamespace
	}
	if namespace == nil {
		return errNativeNamespace
	}
	if err = namespace.Recover(ctx); err != nil {
		return errNativeNamespace
	}
	r.namespaceReady = true
	return r.ackNamespaceBootstrap(paths)
}

// A committed directory's growth reservation ends only after the directory
// writer has finished and the anchored parent durability acknowledgement.
func (r *resources) ackNamespaceBootstrap(paths appdata.Paths) error {
	if r.namespaceBootstrap == nil {
		return nil
	}
	if err := appdata.WithWorking(paths, r.instanceGuard, func(root *os.Root) error {
		directory, err := root.Open(".")
		if err != nil {
			return errNativeNamespace
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return errNativeNamespace
		}
		return nil
	}); err != nil {
		return errNativeNamespace
	}
	if err := r.namespaceBootstrap.Release(); err != nil {
		return errNativeNamespace
	}
	r.namespaceBootstrap = nil
	return nil
}
func (r *resources) configureWorkerLifetime(config workerlife.Config, executable string) workerlife.Config {
	config.ExecutableProvider = r.namespace
	if config.Executable == "" && config.ExecutableFile == nil {
		config.Executable = executable
	}
	if len(config.Args) == 0 {
		config.Args = []string{workerlife.HolderModeArgument}
	}
	return config
}
