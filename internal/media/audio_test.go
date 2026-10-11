package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestAudioSlicesKeepExactSourceOffsets(t *testing.T) {
	pcm := make([]byte, 960002)
	pcm[0] = 1
	pcm[len(pcm)-2] = 2
	var slices []audioSlice
	err := walkPCMSlices(bytes.NewReader(pcm), int64(len(pcm)), func(s audioSlice) error { slices = append(slices, s); return nil })
	if err != nil || len(slices) != 2 {
		t.Fatalf("slices %d %v", len(slices), err)
	}
	if slices[0].StartMS != 0 || slices[0].EndMS != 30000 || slices[1].StartMS != 30000 || slices[1].EndMS != 30001 {
		t.Fatalf("wrong source offsets %+v", slices)
	}
	if string(slices[1].WAV[:4]) != "RIFF" || binary.LittleEndian.Uint32(slices[1].WAV[40:44]) != 2 || slices[1].WAV[44] != 2 {
		t.Fatal("last sample dropped")
	}
}
func TestAudioDurationAndTruncatedSampleRejected(t *testing.T) {
	for _, n := range []int64{38400002, 3, 0} {
		if err := walkPCMSlices(bytes.NewReader(nil), n, func(audioSlice) error { return nil }); !errors.Is(err, ErrLimit) && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("bad pcm length %d: %v", n, err)
		}
	}
	if err := walkPCMSlices(bytes.NewReader([]byte{1}), 2, func(audioSlice) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short stream: %v", err)
	}
	if !silentPCM(make([]byte, 32000)) || silentPCM([]byte{255, 127}) {
		t.Fatal("silence gate broken")
	}
}
func TestGeneratedOutputRemainsUntrustedAndBounded(t *testing.T) {
	text := "rm -rf / ; https://example.invalid/path שלום"
	if got, err := generatedText([]byte(text)); err != nil || got != text {
		t.Fatal("generated text altered or interpreted")
	}
	if _, err := generatedText(bytes.Repeat([]byte{'x'}, 65537)); !errors.Is(err, ErrLimit) {
		t.Fatalf("unbounded output %v", err)
	}
	if _, err := generatedText([]byte{255}); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := &limitedOutput{dst: &b, remaining: 2}
	if _, err := io.Copy(w, bytes.NewReader([]byte("abcd"))); !errors.Is(err, ErrLimit) || b.Len() > 2 {
		t.Fatalf("write limit escaped %d %v", b.Len(), err)
	}
}
