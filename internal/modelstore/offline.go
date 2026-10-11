package modelstore

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Import accepts a directory containing the catalog's exact raw filenames.
// Archives are deliberately unsupported; no extraction path is accepted.
func (s *Store) Import(ctx context.Context, id, sourceDir string) error {
	source, err := openDirectory(sourceDir, false, false)
	if err != nil {
		return err
	}
	defer source.close()
	return s.install(ctx, id, nil, source)
}

func (s *Store) copyOffline(ctx context.Context, source, stage *directory, a Component, readmit func() error) error {
	in, err := source.open(a.Filename, unix.O_RDONLY|unix.O_NONBLOCK, 0, false)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := hashFile(ctx, in, a); err != nil {
		return err
	}
	if final, err := stage.open(a.Filename, unix.O_RDONLY, 0, true); err == nil {
		err = checkFD(final, false, true, true)
		if err == nil {
			err = hashFile(ctx, final, a)
		}
		_ = final.Close()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrUnsafeFile) {
			return err
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
	out, err := stage.open(partName, unix.O_RDWR|unix.O_CREAT, 0600, true)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(0); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := stage.unlink(metaName); err != nil {
		return err
	}
	if err := readmit(); err != nil {
		return err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := copyBounded(ctx, out, in, a.Bytes, nil)
	if err != nil {
		return err
	}
	if n != a.Bytes {
		return ErrSizeMismatch
	}
	if err := hashFile(ctx, out, a); err != nil {
		return err
	}
	return publishComponent(stage, out, partName, metaName, a)
}

func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, cap int64, progress func(int64)) (int64, error) {
	buf := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if int64(n) > cap-total {
			return total, ErrSizeMismatch
		}
		if n > 0 {
			written, e := dst.Write(buf[:n])
			total += int64(written)
			if progress != nil {
				progress(total)
			}
			if e != nil {
				return total, e
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
