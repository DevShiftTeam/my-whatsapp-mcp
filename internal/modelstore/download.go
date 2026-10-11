package modelstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type partialState struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	ETag   string `json:"etag"`
}

func (s *Store) download(ctx context.Context, stage *directory, a Component, progress func(int64), readmit func() error) error {
	if a.URL == "" {
		return ErrOfflineOnly
	}
	if err := s.catalog.checkURL(a.URL); err != nil {
		return err
	}
	// Previously completed components and partials are always rehashed. No size
	// or timestamp shortcut authorizes a native worker to load an asset.
	if final, err := stage.open(a.Filename, unix.O_RDONLY, 0, true); err == nil {
		err = checkFD(final, false, true, true)
		if err == nil {
			err = hashFile(ctx, final, a)
		}
		_ = final.Close()
		if err == nil {
			return nil
		}
		if err := stage.unlink(a.Filename); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	partName, metaName := a.ID+".part", a.ID+".meta"
	if completed, err := stage.open(partName, unix.O_RDONLY, 0, true); err == nil {
		st, e := completed.Stat()
		if e == nil && st.Mode().Perm()&0222 == 0 {
			e = hashFile(ctx, completed, a)
			if e == nil {
				e = publishComponent(stage, completed, partName, metaName, a)
			}
			_ = completed.Close()
			if e == nil {
				return nil
			}
			if err := stage.unlink(partName); err != nil {
				return err
			}
		} else {
			_ = completed.Close()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := stage.open(partName, unix.O_RDWR|unix.O_CREAT, 0600, true)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	offset := stat.Size()
	var prior partialState
	b, metaErr := stage.read(metaName, 2048)
	if metaErr == nil {
		_ = json.Unmarshal(b, &prior)
	} else if !errors.Is(metaErr, os.ErrNotExist) {
		return metaErr
	}
	if offset > a.Bytes || offset < 0 || prior.URL != a.URL || prior.SHA256 != a.SHA256 || prior.Bytes != a.Bytes || !strongETag(prior.ETag) {
		offset = 0
	}
	if offset == a.Bytes {
		if err := hashFile(ctx, f, a); err == nil {
			return publishComponent(stage, f, partName, metaName, a)
		}
		offset = 0
	}
	if offset == 0 {
		if err := f.Truncate(0); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := readmit(); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", prior.ETag)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := s.catalog.checkURL(resp.Request.URL.String()); err != nil {
		return err
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return ErrSizeMismatch
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return ErrHashMismatch
	}
	if resp.StatusCode == http.StatusPartialContent {
		if offset == 0 || resp.Header.Get("ETag") != prior.ETag || !strongETag(prior.ETag) || !validRange(resp.Header.Get("Content-Range"), offset, a.Bytes) {
			return ErrRange
		}
	} else if resp.StatusCode == http.StatusOK {
		// An ignored Range or changed representation restarts on the validated FD.
		hadPrefix := offset > 0
		offset = 0
		if err := f.Truncate(0); err != nil {
			return err
		}
		if hadPrefix {
			if err := f.Sync(); err != nil {
				return err
			}
			if err := readmit(); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("model download HTTP status %d", resp.StatusCode)
	}
	remaining := a.Bytes - offset
	if resp.ContentLength >= 0 && resp.ContentLength != remaining {
		return ErrSizeMismatch
	}
	state := partialState{URL: a.URL, SHA256: a.SHA256, Bytes: a.Bytes, ETag: resp.Header.Get("ETag")}
	if strongETag(state.ETag) {
		meta, _ := json.Marshal(state)
		if err := stage.atomicWrite(metaName, meta); err != nil {
			return err
		}
	} else {
		if err := stage.unlink(metaName); err != nil {
			return err
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	n, copyErr := copyBounded(ctx, f, io.LimitReader(resp.Body, remaining+1), remaining, func(n int64) {
		if progress != nil {
			progress(offset + n)
		}
	})
	// Preserve a bounded strong-ETag prefix on interruption; fsync before returning
	// so a restart does not trust bytes that have not reached the filesystem.
	if err := f.Sync(); err != nil {
		return err
	}
	if copyErr != nil {
		return copyErr
	}
	if n != remaining {
		return ErrSizeMismatch
	}
	if err := hashFile(ctx, f, a); err != nil {
		_ = f.Truncate(0)
		_ = f.Sync()
		_ = stage.unlink(metaName)
		return err
	}
	return publishComponent(stage, f, partName, metaName, a)
}

func publishComponent(stage *directory, f *os.File, partName, metaName string, a Component) error {
	if err := unix.Fchmod(int(f.Fd()), 0400); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := stage.renameTo(partName, stage, a.Filename); err != nil {
		return err
	}
	if err := stage.unlink(metaName); err != nil {
		return err
	}
	return stage.file.Sync()
}

func validRange(raw string, start, total int64) bool {
	if !strings.HasPrefix(raw, "bytes ") {
		return false
	}
	pieces := strings.Split(strings.TrimPrefix(raw, "bytes "), "/")
	if len(pieces) != 2 {
		return false
	}
	rangeParts := strings.Split(pieces[0], "-")
	if len(rangeParts) != 2 {
		return false
	}
	s, e1 := strconv.ParseInt(rangeParts[0], 10, 64)
	e, e2 := strconv.ParseInt(rangeParts[1], 10, 64)
	n, e3 := strconv.ParseInt(pieces[1], 10, 64)
	return e1 == nil && e2 == nil && e3 == nil && s == start && e == total-1 && n == total && start < total
}
