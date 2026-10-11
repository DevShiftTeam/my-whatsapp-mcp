package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"sync"
)

type bundledAIAssets struct {
	mu        sync.Mutex
	root      *os.Root
	files     map[string]*os.File
	artifacts map[string]bundleArtifact
}

func (b *bundledAIAssets) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, file := range b.files {
		if err := file.Close(); err != nil {
			return errBundledAI
		}
		delete(b.files, name)
	}
	if b.root != nil {
		if err := b.root.Close(); err != nil {
			return errBundledAI
		}
		b.root = nil
	}
	return nil
}

func (b *bundledAIAssets) duplicate(name string) (*os.File, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	file := b.files[name]
	if b.root == nil || file == nil {
		return nil, errBundledAI
	}
	return duplicateBundleFile(file)
}

func (b *bundledAIAssets) open(name string, artifact *bundleArtifact, maximum int64) (*os.File, error) {
	if !canonicalBundlePath(name) || b.root == nil {
		return nil, errBundledAI
	}
	parts := strings.Split(name, "/")
	for index := 1; index < len(parts); index++ {
		parent := strings.Join(parts[:index], "/")
		info, err := b.root.Lstat(parent)
		if err != nil || !privateBundleEntry(info, true, false) {
			return nil, errBundledAI
		}
		file, err := b.root.OpenFile(parent, os.O_RDONLY|stateNoFollow(), 0)
		if err != nil {
			return nil, errBundledAI
		}
		opened, err := file.Stat()
		valid := err == nil && os.SameFile(info, opened) && privateBundleEntry(opened, true, false) && stateNoExtendedACL(file)
		closeErr := file.Close()
		if !valid || closeErr != nil {
			return nil, errBundledAI
		}
	}
	info, err := b.root.Lstat(name)
	executable := artifact != nil && artifact.Executable
	if err != nil || !privateBundleEntry(info, false, executable) || info.Size() <= 0 || info.Size() > maximum {
		return nil, errBundledAI
	}
	file, err := b.root.OpenFile(name, os.O_RDONLY|stateNoFollow(), 0)
	if err != nil {
		return nil, errBundledAI
	}
	fail := func() (*os.File, error) { file.Close(); return nil, errBundledAI }
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !privateBundleEntry(opened, false, executable) || !stateNoExtendedACL(file) {
		return fail()
	}
	if artifact != nil {
		if opened.Size() != artifact.SizeBytes || !canonicalHex(artifact.SHA256, 32) {
			return fail()
		}
		digest := sha256.New()
		count, err := io.Copy(digest, io.LimitReader(file, artifact.SizeBytes+1))
		if err != nil || count != artifact.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
			return fail()
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fail()
		}
	}
	after, err := file.Stat()
	current, pathErr := b.root.Lstat(name)
	if err != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || !privateBundleEntry(after, false, executable) {
		return fail()
	}
	return file, nil
}

func readBundleBytes(file *os.File, maximum int64) ([]byte, error) {
	before, err := file.Stat()
	if err != nil || before.Size() <= 0 || before.Size() > maximum {
		return nil, errBundledAI
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(data)) != before.Size() || int64(len(data)) > maximum || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errBundledAI
	}
	return data, nil
}
