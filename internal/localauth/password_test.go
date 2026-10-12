package localauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func TestPasswordBoundariesAndExactBytes(t *testing.T) {
	for _, p := range [][]byte{[]byte(strings.Repeat("x", 12)), []byte(strings.Repeat("x", 128)), []byte("שלום עולם! ")} {
		if !validPassword(p) {
			t.Fatal("valid byte boundary rejected")
		}
	}

	for _, p := range [][]byte{[]byte("short"), []byte(strings.Repeat("x", 129)), []byte("abcdefghijkl\n"), []byte("abcdefghijkl\u0085"), append([]byte("abcdefghijkl"), 255)} {
		if _, err := HashPassword(context.Background(), p, 1); !errors.Is(err, ErrPassword) {
			t.Fatalf("invalid password accepted: %v", err)
		}
	}
	p := []byte("  exact passphrase  ")
	r, err := HashPassword(context.Background(), p, 7)
	if err != nil {
		t.Fatal(err)
	}
	expected := argon2.IDKey(p, r.salt[:], 3, 32*1024, 1, 32)
	if !bytes.Equal(expected, r.digest[:]) {
		t.Fatal("password parameters differ from approved Argon2id")
	}
	clear(expected)
	if r.Generation() != 7 {
		t.Fatal("generation")
	}
	if ok, err := VerifyPassword(context.Background(), p, r); err != nil || !ok {
		t.Fatal("exact password rejected", err)
	}
	if ok, _ := VerifyPassword(context.Background(), []byte(strings.TrimSpace(string(p))), r); ok {
		t.Fatal("trimmed password accepted")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", r, r), string(p)) {
		t.Fatal("format leak")
	}
	data := encodeRecord(r)
	zeroGeneration := append([]byte(nil), data...)
	clear(zeroGeneration[18:26])
	if _, err := decodeRecord(zeroGeneration); err == nil {
		t.Fatal("zero generation accepted")
	}
	for _, i := range []int{0, 8, 9, 13, 17} {
		bad := append([]byte(nil), data...)
		bad[i] = 255
		if _, err := decodeRecord(bad); err == nil {
			t.Fatalf("bad record byte %d accepted", i)
		}
	}
	for _, bad := range [][]byte{data[:len(data)-1], append(data, 0)} {
		if _, err := decodeRecord(bad); err == nil {
			t.Fatal("unbounded record accepted")
		}
	}
}
func TestGlobalHashAdmissionAndCancellation(t *testing.T) {
	if err := hashSlots.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := hashSlots.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer hashSlots.release()
	defer hashSlots.release()
	if _, err := HashPassword(context.Background(), []byte("long passphrase"), 1); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashPassword(ctx, []byte("long passphrase"), 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestVerificationFailureBurstAndFixedCooldown(t *testing.T) {
	var l attemptLimiter
	now := time.Now()
	for i := 0; i < 5; i++ {
		if e := l.admit(now); e != nil {
			t.Fatal(e)
		}
		l.finish(now, false)
	}
	if l.admit(now.Add(29*time.Second)) != ErrBusy {
		t.Fatal("failure burst allowed")
	}
	if e := l.admit(now.Add(30 * time.Second)); e != nil {
		t.Fatal("cooldown extended", e)
	}
	var successful attemptLimiter
	for i := 0; i < 8; i++ {
		if e := successful.admit(now); e != nil {
			t.Fatal(e)
		}
		successful.finish(now, true)
	}
	if successful.admit(now) != ErrBusy {
		t.Fatal("admission burst unbounded")
	}
}
