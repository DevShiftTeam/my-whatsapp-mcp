package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func documentPDFFixture() []byte {
	var pdf bytes.Buffer
	_, _ = pdf.WriteString("%PDF-1.7\n")
	catalog := pdf.Len()
	_, _ = pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	pages := pdf.Len()
	_, _ = pdf.WriteString("2 0 obj\n<< /Type /Pages /Count 0 /Kids [] >>\nendobj\n")
	xref := pdf.Len()
	_, _ = fmt.Fprintf(&pdf, "xref\n0 3\n0000000000 65535 f \n%010d 00000 n \n%010d 00000 n \ntrailer\n<< /Size 3 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", catalog, pages, xref)
	return pdf.Bytes()
}

func TestDocumentVerificationClassifiesPDFAndRetainsOpaqueBytes(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		data         []byte
	}{
		{"PDF 1", "pdf", documentPDFFixture()},
		{"PDF 2", "pdf", bytes.Replace(documentPDFFixture(), []byte("%PDF-1.7"), []byte("%PDF-2.0"), 1)},
		{"PDF without EOF", "document", []byte("%PDF-1.7\ntruncated recording")},
		{"PDF with suffix", "document", append(documentPDFFixture(), []byte("unframed suffix")...)},
		{"PDF embedded in another file", "document", append([]byte("prefix"), documentPDFFixture()...)},
		{"ordinary binary", "document", []byte{0, 1, 2, 255, 4}},
		{"ZIP bytes", "document", []byte("PK\x03\x04bounded opaque archive")},
		{"HTML bytes", "document", []byte("<html><script>untrusted attachment</script></html>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := verificationFile(t, tc.data)
			if _, err := file.Seek(1, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			digest, size, format, err := verifyMediaFile(context.Background(), file, archive.MessageKind("document"), int64(len(tc.data)))
			if err != nil || size != int64(len(tc.data)) || format != tc.format {
				t.Fatalf("document classification: bytes=%d format=%q err=%v", size, format, err)
			}
			want := sha256.Sum256(tc.data)
			if digest != hex.EncodeToString(want[:]) {
				t.Fatal("complete attachment digest changed")
			}
			if position, err := file.Seek(0, io.SeekCurrent); err != nil || position != 1 {
				t.Fatalf("verification changed caller offset: %d %v", position, err)
			}
		})
	}
}

func TestDocumentVerificationEnforcesHardCapAndPrivateFD(t *testing.T) {
	kind := archive.MessageKind("document")
	oversized := verificationFile(t, []byte("bounded synthetic document"))
	if err := oversized.Truncate((8 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := verifyMediaFile(context.Background(), oversized, kind, 25<<20); !errors.Is(err, ErrLimit) {
		t.Fatalf("generic ceiling expanded the document hard cap: %v", err)
	}
	data := documentPDFFixture()
	file := verificationFile(t, data)
	if err := file.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := verifyMediaFile(context.Background(), file, kind, int64(len(data))); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("public document accepted: %v", err)
	}
	if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, nil), kind, 1); !errors.Is(err, ErrFormat) {
		t.Fatalf("empty document accepted: %v", err)
	}
}

func TestDocumentVerificationAcceptsExactHardCap(t *testing.T) {
	data := make([]byte, 8<<20)
	copy(data, "bounded opaque document")
	digest, size, format, err := verifyMediaFile(context.Background(), verificationFile(t, data), archive.MessageKind("document"), int64(len(data)))
	if err != nil || size != int64(len(data)) || format != "document" {
		t.Fatal("exact hard-cap document rejected", err)
	}
	want := sha256.Sum256(data)
	if digest != hex.EncodeToString(want[:]) {
		t.Fatal("hard-cap document not fully hashed")
	}
}

func TestDocumentVerificationObservesCancellationAndChangedExtent(t *testing.T) {
	data := bytes.Repeat([]byte("synthetic document block\n"), 10000)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &verificationMutationContext{Context: base, at: 3, mutate: cancel}
	if _, _, _, err := verifyMediaFile(ctx, verificationFile(t, data), archive.MessageKind("document"), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-progress document cancellation ignored: %v", err)
	}
	file := verificationFile(t, data)
	ctx = &verificationMutationContext{Context: context.Background(), mutate: func() {
		if err := file.Truncate(int64(len(data) - 1)); err != nil {
			t.Fatal(err)
		}
	}}
	if _, _, _, err := verifyMediaFile(ctx, file, archive.MessageKind("document"), int64(len(data))); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("changed document extent accepted: %v", err)
	}
}
