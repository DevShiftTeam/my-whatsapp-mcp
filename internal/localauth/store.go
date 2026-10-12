package localauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

// RecordFilename is independently owned by localauth. Provider removal must
// validate and preserve it; archive ResetOwned removes the entire remote tree.
const RecordFilename = "owner-password.v1"

type Authenticator struct {
	mu          sync.Mutex
	paths       appdata.Paths
	lease       *appdata.InstanceLock
	record      Record
	closed      bool
	afterHash   func()       // test checkpoint, invoked outside auth/app/lease locks
	afterCommit func() error // test-only durability failure checkpoint
}

func NewAuthenticator(paths appdata.Paths, lease *appdata.InstanceLock) (*Authenticator, error) {
	a := &Authenticator{paths: paths, lease: lease}
	err := appdata.WithRemoteAccess(paths, lease, func(root *os.Root) error {
		r, e := readRecord(root)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		a.record = r
		return e
	})
	if err != nil {
		return nil, ErrState
	}
	return a, nil
}
func (a *Authenticator) Generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return 0
	}
	return a.record.generation
}
func (a *Authenticator) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.record = Record{}
}
func (a *Authenticator) SetPassword(ctx context.Context, password []byte) error {
	a.mu.Lock()
	generation, closed := a.record.generation, a.closed
	a.mu.Unlock()
	if closed || generation == math.MaxUint64 {
		return ErrState
	}
	r, err := HashPassword(ctx, password, generation+1)
	if a.afterHash != nil {
		a.afterHash()
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.record.generation != generation || ctx.Err() != nil {
		return ErrState
	}
	committed := false
	err = appdata.WithRemoteAccess(a.paths, a.lease, func(root *os.Root) error {
		current, e := readRecord(root)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return ErrState
		}
		if current.generation != generation {
			return ErrState
		}
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return ErrState
		}
		name := ".owner-password-" + hex.EncodeToString(nonce[:])
		defer root.Remove(name)
		data := encodeRecord(r)
		defer clear(data)
		f, e := appdata.OpenRemoteAccessFile(root, name, true)
		if e != nil {
			return ErrState
		}
		n, we := f.Write(data)
		se := f.Sync()
		ce := f.Close()
		if n != len(data) || we != nil || se != nil || ce != nil {
			return ErrState
		}
		check, e := readNamedRecord(root, name)
		if e != nil || check != r {
			return ErrState
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = root.Rename(name, RecordFilename); e != nil {
			return ErrState
		}
		// Even a post-commit sync failure must invalidate the previous generation.
		a.record = r
		committed = true
		if a.afterCommit != nil {
			if e := a.afterCommit(); e != nil {
				return e
			}
		}
		d, e := root.Open(".")
		if e != nil {
			return ErrState
		}
		se = d.Sync()
		ce = d.Close()
		if se != nil || ce != nil {
			return ErrState
		}
		return nil
	})
	if err != nil {
		if committed {
			a.closed = true
			a.record = Record{}
		}
		return ErrState
	}
	return nil
}
func (a *Authenticator) Verify(ctx context.Context, password []byte, expected uint64) (bool, error) {
	a.mu.Lock()
	r, closed := a.record, a.closed
	a.mu.Unlock()
	if closed || expected == 0 || r.generation != expected {
		return false, ErrState
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := verificationAttempts.admit(time.Now()); err != nil {
		return false, err
	}
	ok, err := VerifyPassword(ctx, password, r)
	if a.afterHash != nil {
		a.afterHash()
	}
	verificationAttempts.finish(time.Now(), ok || errors.Is(err, ErrBusy) || errors.Is(err, context.Canceled))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.record.generation != expected || ctx.Err() != nil {
		return false, ErrState
	}
	return ok, err
}

// ValidateRecordFile borrows root synchronously. Callers still enforce the full
// private tree policy; this never exempts an unsafe or unknown entry.
func ValidateRecordFile(root *os.Root) error   { _, err := readRecord(root); return err }
func readRecord(root *os.Root) (Record, error) { return readNamedRecord(root, RecordFilename) }
func readNamedRecord(root *os.Root, name string) (Record, error) {
	f, err := appdata.OpenRemoteAccessFile(root, name, false)
	if err != nil {
		return Record{}, err
	}
	before, e1 := f.Stat()
	data, e2 := io.ReadAll(io.LimitReader(f, recordSize+1))
	after, e3 := f.Stat()
	current, e4 := root.Lstat(name)
	e5 := f.Close()
	defer clear(data)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return Record{}, ErrState
	}
	return decodeRecord(data)
}

func (a *Authenticator) String() string               { return "[remote owner authenticator]" }
func (a *Authenticator) GoString() string             { return a.String() }
func (a *Authenticator) MarshalJSON() ([]byte, error) { return nil, ErrState }
