package inference

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// A splitter that decodes tokenizer tokens back into text would lose the spaces,
// combining marks and format characters here. Fragment counts are a dependency;
// reconstruction and valid source boundaries belong to the splitter itself.
func TestNativeSplitPreservesOriginalUTF8(t *testing.T) {
	count := func(_ context.Context, text string) (int, error) {
		return utf8.RuneCountInString(text) + 5, nil
	}
	for _, text := range []string{"", "  שלום\tworld\n", "e\u0301  👩\u200d💻\r\n", strings.Repeat("a", 96), strings.Repeat("hello שלום 👋  ", 80)} {
		parts, err := splitNativeText(context.Background(), text, 12, count)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(parts, "") != text {
			t.Fatal("split lost or changed original text")
		}
		for _, part := range parts {
			if !utf8.ValidString(part) {
				t.Fatal("split inside UTF-8 codepoint")
			}
			tokens, err := count(context.Background(), part)
			if err != nil || tokens > 12 {
				t.Fatalf("fragment exceeds complete prefixed token limit: %d %v", tokens, err)
			}
			if text != "" && part == "" {
				t.Fatal("split failed to make progress")
			}
		}
	}
}

func TestNativeSplitRejectsImpossibleLimit(t *testing.T) {
	count := func(_ context.Context, text string) (int, error) {
		return len(text) + 5, nil
	}
	for _, limit := range []int{0, 4, 6} {
		if _, err := splitNativeText(context.Background(), "👋", limit, count); !errors.Is(err, ErrLimit) {
			t.Fatalf("impossible limit %d: %v", limit, err)
		}
	}
}

func TestNativeSplitChecksCancellationAfterCount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := func(_ context.Context, _ string) (int, error) {
		cancel()
		return 6, nil
	}
	if _, err := splitNativeText(ctx, "hello", 20, count); !errors.Is(err, context.Canceled) {
		t.Fatalf("count completion swallowed cancellation: %v", err)
	}
}

func TestNativeSplitPropagatesCountFailure(t *testing.T) {
	failure := errors.New("count failed")
	count := func(context.Context, string) (int, error) { return 0, failure }
	if _, err := splitNativeText(context.Background(), "hello", 20, count); !errors.Is(err, failure) {
		t.Fatalf("count failure: %v", err)
	}
}

func TestNativeSplitBoundsFragmentCount(t *testing.T) {
	count := func(_ context.Context, text string) (int, error) {
		return len(text) + 5, nil
	}
	if _, err := splitNativeText(context.Background(), strings.Repeat("a", MaxSplitChunks+1), 6, count); !errors.Is(err, ErrLimit) {
		t.Fatalf("unbounded fragment count: %v", err)
	}
}

func TestNativeSplitEmptyTextHasNoFragments(t *testing.T) {
	count := func(context.Context, string) (int, error) { return 5, nil }
	parts, err := splitNativeText(context.Background(), "", 256, count)
	if err != nil || len(parts) != 0 {
		t.Fatalf("empty text emitted an empty protocol fragment: %v %v", parts, err)
	}
}

func TestNativeSplitFindsFitWhenSubwordCountDecreases(t *testing.T) {
	count := func(_ context.Context, text string) (int, error) {
		if text == "a" {
			return 7, nil
		}
		if text == "ab" {
			return 6, nil
		}
		return len(text) + 5, nil
	}
	parts, err := splitNativeText(context.Background(), "abcdef", 6, count)
	if err != nil || strings.Join(parts, "") != "abcdef" {
		t.Fatalf("valid subword fragment was rejected: %v %v", parts, err)
	}
	for _, part := range parts {
		n, _ := count(context.Background(), part)
		if n > 6 {
			t.Fatalf("unchecked fragment count: %d", n)
		}
	}
}

func TestNativeTextRejectsCStringsThatWouldLoseData(t *testing.T) {
	for _, text := range []string{"hello\x00world", string([]byte{0xff, 0xfe})} {
		if err := validateNativeText(text); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid FFI text accepted: %v", err)
		}
	}
	if err := validateNativeText(strings.Repeat("a", maxNativeTextBytes+1)); !errors.Is(err, ErrLimit) {
		t.Fatalf("unbounded FFI text accepted: %v", err)
	}
}
