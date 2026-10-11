//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/inference"
)

func TestNativeFactoryRejectsUninheritedComponentPaths(t *testing.T) {
	valid := inference.ComponentPaths{Model: "/dev/fd/5", Tokenizer: "/dev/fd/6", Runtime: "/dev/fd/7"}
	for name, paths := range map[string]inference.ComponentPaths{
		"arbitrary model":     {Model: "/tmp/private-message", Tokenizer: valid.Tokenizer, Runtime: valid.Runtime},
		"arbitrary tokenizer": {Model: valid.Model, Tokenizer: "/tmp/tokenizer", Runtime: valid.Runtime},
		"arbitrary library":   {Model: valid.Model, Tokenizer: valid.Tokenizer, Runtime: "/tmp/library"},
		"descriptor swap":     {Model: valid.Tokenizer, Tokenizer: valid.Model, Runtime: valid.Runtime},
	} {
		t.Run(name, func(t *testing.T) {
			backend, err := nativeWorkerFactory(context.Background(), inference.ReferenceProfile(), paths)
			if backend != nil || !errors.Is(err, inference.ErrArtifact) {
				t.Fatalf("got backend=%v err=%v", backend, err)
			}
		})
	}
}

func TestInheritedComponentsRequireReadonlyRegularFiles(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "component")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if inheritedComponentFile(int(file.Fd())) {
		t.Fatal("writable component accepted")
	}
	readonly, err := os.Open(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if !inheritedComponentFile(int(readonly.Fd())) {
		t.Fatal("readonly regular component rejected")
	}
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	if inheritedComponentFile(int(input.Fd())) {
		t.Fatal("pipe component accepted")
	}
}

func TestInheritedPipesRequireCorrectDirections(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	if !inheritedPipe(int(input.Fd()), false) || !inheritedPipe(int(output.Fd()), true) {
		t.Fatal("private pipes rejected")
	}
	if inheritedPipe(int(input.Fd()), true) || inheritedPipe(int(output.Fd()), false) {
		t.Fatal("wrong-direction pipe accepted")
	}
}

func TestMissingInheritedPipesExitsWithoutStdoutOrStderr(t *testing.T) {
	if os.Getenv("WA_INDEXER_WORKER_MAIN_TEST") == "1" {
		os.Exit(runMain())
	}
	command := exec.Command(os.Args[0], "-test.run=^TestMissingInheritedPipesExitsWithoutStdoutOrStderr$")
	command.Env = []string{"WA_INDEXER_WORKER_MAIN_TEST=1"}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("worker without pipes succeeded")
	}
	if len(output) != 0 {
		t.Fatalf("worker wrote to stdout/stderr: %q", output)
	}
}
