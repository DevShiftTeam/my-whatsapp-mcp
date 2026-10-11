//go:build unix

// wa-indexer-worker is an internal child process, not a command-line service.
package main

import (
	"context"
	"os"

	"github.com/Bodokh/wa-indexer/internal/inference"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
	"golang.org/x/sys/unix"
)

func main() { os.Exit(runMain()) }

func runMain() (status int) {
	status = 1
	// Redirect the underlying descriptors as well as avoiding Go logging: native
	// libraries and panic/runtime diagnostics must never reach captured stdio.
	if !discardStdio() {
		return status
	}
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if len(os.Args) == 2 && os.Args[1] == workerlife.HolderModeArgument {
		if workerlife.RunInherited() != nil {
			return status
		}
		return 0
	}
	if len(os.Args) != 1 {
		return status
	}
	if !inheritedPipe(3, false) || !inheritedPipe(4, true) {
		return status
	}
	input := os.NewFile(3, "worker-input")
	output := os.NewFile(4, "worker-output")
	defer input.Close()
	defer output.Close()
	defer unix.Close(5)
	defer unix.Close(6)
	defer unix.Close(7)
	if inference.RunWorker(context.Background(), input, output, nativeWorkerFactory) != nil {
		return status
	}
	return 0
}

func discardStdio() bool {
	file, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	for _, descriptor := range []int{0, 1, 2} {
		if unix.Dup2(int(file.Fd()), descriptor) != nil {
			return false
		}
	}
	return true
}

func inheritedPipe(descriptor int, writable bool) bool {
	var stat unix.Stat_t
	if unix.Fstat(descriptor, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return false
	}
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	if err != nil {
		return false
	}
	expected := unix.O_RDONLY
	if writable {
		expected = unix.O_WRONLY
	}
	return flags&unix.O_ACCMODE == expected
}

func inheritedComponentFile(descriptor int) bool {
	var stat unix.Stat_t
	if unix.Fstat(descriptor, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	flags, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFL, 0)
	return err == nil && flags&unix.O_ACCMODE == unix.O_RDONLY
}

func nativeWorkerFactory(ctx context.Context, profile inference.Profile, paths inference.ComponentPaths) (inference.Backend, error) {
	if paths.Model != "/dev/fd/5" || paths.Tokenizer != "/dev/fd/6" || paths.Runtime != "/dev/fd/7" {
		return nil, inference.ErrArtifact
	}
	if profile.Validate() != nil || !inheritedComponentFile(5) || !inheritedComponentFile(6) || !inheritedComponentFile(7) {
		return nil, inference.ErrArtifact
	}
	if ctx.Err() != nil {
		return nil, inference.ErrUnavailable
	}
	return inference.NewNativeEncoder(inference.NativeConfig{Profile: profile, Paths: paths})
}
