//go:build darwin || linux

package media

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"golang.org/x/sys/unix"
)

// RecoverOriginalCache verifies an existing app-owned original before restoring
// its metadata. The caller must hold the profile's offline instance lease. It
// neither imports arbitrary files nor starts downloads, cleanup or extraction.
func (s *Store) RecoverOriginalCache(ctx context.Context, w archive.OriginalCacheWitness) (accepted bool, result error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if w.ArtifactID == "" || len(w.ArtifactID) > 128 || w.MediaID == "" || len(w.MediaID) > 128 || w.Key.Account != w.Source.Account || w.Bytes <= 0 || w.Bytes > maximumMediaBytes || len(w.SHA256) != 64 || strings.ToLower(w.SHA256) != w.SHA256 {
		return false, ErrInvalidRequest
	}
	if digest, err := hex.DecodeString(w.SHA256); err != nil || len(digest) != 32 {
		return false, ErrInvalidRequest
	}
	maximum := s.options.ImageMaximumBytes
	if w.Kind == archive.MessageVoice {
		maximum = s.options.VoiceMaximumBytes
	} else if w.Kind != archive.MessageImage {
		return false, ErrInvalidRequest
	}
	if w.Bytes > maximum {
		return false, ErrLimit
	}
	port, ok := s.repo.(archive.OriginalCacheRecoveryStore)
	if !ok {
		return false, ErrUnavailable
	}
	if err := s.begin(); err != nil {
		return false, err
	}
	defer s.end()
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	busy := s.active != 1 || len(s.residents) != 0 || len(s.pending) != 0
	s.mu.Unlock()
	if busy {
		return false, ErrActiveUse
	}
	current, err := s.repo.CurrentSource(ctx, w.Key)
	if errors.Is(err, archive.ErrSourceUnavailable) || errors.Is(err, archive.ErrPolicyChanged) {
		return false, ErrSourceChanged
	}
	if err != nil {
		return false, err
	}
	if !sameSource(current, w.Source) {
		return false, ErrSourceChanged
	}
	readers, err := artifactLock(ctx, s.media, "media-readers.lock", true, false)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := releaseLock(readers); err != nil {
			accepted = false
			result = errors.Join(result, err)
		}
	}()
	guard, err := artifactLock(ctx, s.media, artifactName(w.ArtifactID, ".lock"), true, false)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := releaseLock(guard); err != nil {
			accepted = false
			result = errors.Join(result, err)
		}
	}()
	name := artifactName(w.ArtifactID, ".original")
	file, err := s.media.open(name, unix.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			accepted = false
			result = errors.Join(result, err)
		}
	}()
	if err := s.media.same(name, file); err != nil {
		return false, err
	}
	digest, bytes, format, err := verifyMediaFile(ctx, file, w.Kind, maximum)
	if err != nil {
		return false, err
	}
	if digest != w.SHA256 || bytes != w.Bytes || format != w.Format {
		return false, ErrAuthentication
	}
	if err := s.media.same(name, file); err != nil {
		return false, err
	}
	// The archive repeats current source, policy, completion and stopped-owner
	// checks in its serialized transaction; a verified file alone grants none.
	return port.RecoverOriginalCache(ctx, w)
}
