package inference

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxNativeTextBytes = 1 << 20

func validateNativeText(text string) error {
	if len(text) > maxNativeTextBytes {
		return ErrLimit
	}
	// The tokenizer binding accepts C strings, so NUL must be rejected before
	// crossing FFI rather than silently dropping the rest of a user's message.
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return ErrProtocol
	}
	return nil
}

// splitNativeText returns source slices, never a decode of normalized tokens.
// Each selected boundary is checked by the untruncated prefixed tokenizer.
// Small byte windows keep splitting linear in input size rather than repeatedly
// tokenizing a potentially megabyte-long remainder for every small fragment.
func splitNativeText(ctx context.Context, text string, maxTokens int, count func(context.Context, string) (int, error)) ([]string, error) {
	if err := validateNativeText(text); err != nil {
		return nil, err
	}
	if maxTokens <= 0 {
		return nil, ErrLimit
	}
	checkCount := func(candidate string) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := count(ctx, candidate)
		if err != nil {
			return 0, err
		}
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		if n < 0 {
			return 0, ErrProtocol
		}
		return n, nil
	}
	base, err := checkCount("")
	if err != nil {
		return nil, err
	}
	if base > maxTokens {
		return nil, ErrLimit
	}
	if text == "" {
		return []string{}, nil
	}
	parts := make([]string, 0)
	for start := 0; start < len(text); {
		rest := text[start:]
		end := len(rest)
		if maxTokens <= len(rest)/8 {
			end = maxTokens * 8
			if end < len(rest) {
				for end > 0 && !utf8.RuneStart(rest[end]) {
					end--
				}
			}
		}
		n, err := checkCount(rest[:end])
		if err != nil {
			return nil, err
		}
		if n > maxTokens {
			// Token counts need not be monotonic for growing prefixes. Binary
			// search is only a bounded way to find a checked fitting boundary;
			// it does not claim to find the longest possible fragment.
			low, high := 0, end
			for {
				mid := low + (high-low)/2
				for mid > low && !utf8.RuneStart(rest[mid]) {
					mid--
				}
				if mid <= low {
					break
				}
				candidateCount, err := checkCount(rest[:mid])
				if err != nil {
					return nil, err
				}
				if candidateCount <= maxTokens {
					low = mid
				} else {
					high = mid
				}
			}
			if low == 0 {
				_, width := utf8.DecodeRuneInString(rest)
				firstCount, err := checkCount(rest[:width])
				if err != nil {
					return nil, err
				}
				if firstCount > maxTokens {
					// A standalone rune may tokenize into more pieces than a
					// later combined prefix. Check remaining boundaries in this
					// bounded window before declaring it impossible to split.
					for candidate := width; candidate < end; {
						_, nextWidth := utf8.DecodeRuneInString(rest[candidate:])
						candidate += nextWidth
						candidateCount, err := checkCount(rest[:candidate])
						if err != nil {
							return nil, err
						}
						if candidateCount <= maxTokens {
							low = candidate
							break
						}
					}
					if low == 0 {
						return nil, ErrLimit
					}
				} else {
					low = width
				}
			}
			end = low
		}
		if len(parts) == MaxSplitChunks {
			return nil, ErrLimit
		}
		parts = append(parts, rest[:end])
		start += end
	}
	return parts, nil
}

func nativeRSS() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "/bin/ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	return value * 1024, err
}
