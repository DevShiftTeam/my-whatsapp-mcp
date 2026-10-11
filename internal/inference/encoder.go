// Package inference isolates native model execution behind bounded private IPC.
package inference

import (
	"context"
	"errors"
	"math"
	"os"
)

var (
	ErrUnavailable  = errors.New("native_encoder_unavailable")
	ErrArtifact     = errors.New("encoder_artifact_invalid")
	ErrProtocol     = errors.New("encoder_protocol_invalid")
	ErrWorkerFailed = errors.New("encoder_worker_failed")
	ErrClosed       = errors.New("encoder_closed")
	ErrLimit        = errors.New("encoder_limit_exceeded")
	ErrCleanup      = errors.New("encoder_cleanup_incomplete")
)

type Encoder interface {
	EncodeQuery(context.Context, string) ([]float32, error)
	EncodePassages(context.Context, []string) ([][]float32, error)
	Close() error
}

// Backend is private worker functionality; Count never applies truncation.
type Backend interface {
	Encoder
	Count(context.Context, string) (int, error)
	Split(context.Context, string, int) ([]string, error)
}
type TokenizerIdentity struct{ ProfileID, TokenizerSHA256, Implementation, PrefixVersion string }
type TextSpan struct {
	Start, End int
	Text       string
	Tokens     int
}
type Tokenizer interface {
	TokenizerIdentity() TokenizerIdentity
	CountPassage(context.Context, string) (int, error)
	SplitPassage(context.Context, string, int) ([]TextSpan, error)
}

// PassageTokenizer is the archive extension contract's exact tokenizer seam.
type PassageTokenizer = Tokenizer
type ComponentPaths struct{ Model, Tokenizer, Runtime string }
type NativeConfig struct {
	Profile Profile
	Paths   ComponentPaths
}

// ComponentLease represents app-verified readonly components under a lifetime
// shared lock. The owner releases it only after the complete process group exits.
// Open returns an owned duplicate descriptor; names are model/tokenizer/runtime.
type ComponentLease interface {
	Open(string) (*os.File, error)
	// WorkerGuard duplicates the same held shared-lock OFD. Once exported,
	// all owner releases close descriptors without an explicit LOCK_UN.
	WorkerGuard() (*os.File, error)
	Close() error
}
type LeaseProvider func(context.Context, Profile) (ComponentLease, error)
type AdmissionClass string

const (
	ForegroundQuery AdmissionClass = "foreground_query"
	BackgroundText  AdmissionClass = "background_text"
)

// ComputeLease cancellation requests native shutdown, but its reservation
// remains held until the actor releases it after the complete cleanup boundary.
type ComputeLease interface {
	Context() context.Context
	Release()
}
type AdmissionProvider func(context.Context, AdmissionClass, int) (ComputeLease, error)

func PoolEmbedding(hidden []float32, mask []int64, dimension int) ([]float32, error) {
	if dimension != 384 || len(mask) == 0 || len(hidden) != len(mask)*dimension {
		return nil, ErrProtocol
	}
	sum := make([]float64, dimension)
	count := 0.
	for i, m := range mask {
		if m != 0 && m != 1 {
			return nil, ErrProtocol
		}
		if m == 0 {
			continue
		}
		count++
		for j := range sum {
			x := float64(hidden[i*dimension+j])
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, ErrProtocol
			}
			sum[j] += x
		}
	}
	if count == 0 {
		return nil, ErrProtocol
	}
	norm := 0.
	for j := range sum {
		sum[j] /= count
		norm += sum[j] * sum[j]
	}
	norm = math.Sqrt(norm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, ErrProtocol
	}
	v := make([]float32, dimension)
	for j := range v {
		v[j] = float32(sum[j] / norm)
	}
	return v, nil
}
