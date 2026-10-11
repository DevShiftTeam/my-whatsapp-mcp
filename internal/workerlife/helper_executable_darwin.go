//go:build darwin

package workerlife

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Keep provider cause identity for admission retry without exposing its message.
// The public boundary remains unavailable; callers can classify a budget cause
// using errors.Is while Error never contains provider paths or diagnostics.
type executablePreparationFailure struct{ cause error }

func (e executablePreparationFailure) Error() string        { return ErrUnavailable.Error() }
func (e executablePreparationFailure) Unwrap() error        { return e.cause }
func (e executablePreparationFailure) Is(target error) bool { return target == ErrUnavailable }

func helperExecutable(ctx context.Context, config Config, source *os.File, _ int) (string, ExecutableLease, error) {
	if namespaceNil(config.ExecutableProvider) {
		return "", nil, ErrUnavailable
	}
	lease, err := config.ExecutableProvider.Prepare(ctx, source, 64<<20)
	if namespaceNil(lease) {
		lease = nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", lease, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", lease, context.DeadlineExceeded
		}
		return "", lease, executablePreparationFailure{cause: err}
	}
	if lease == nil || !filepath.IsAbs(lease.Path()) {
		return "", lease, ErrUnavailable
	}
	return lease.Path(), lease, nil
}

// ValidateExecutableConfig lets composition reject an unavailable Darwin
// holder before accepting native work. The provider owns the immutable private
// path; executable /dev/fd is unavailable on the qualified Darwin host.
func ValidateExecutableConfig(config Config) bool { return !namespaceNil(config.ExecutableProvider) }
