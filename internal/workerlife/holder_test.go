//go:build darwin || linux

package workerlife

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHolderNOOPClosesGuardsAfterRegistrationAndEOF(t *testing.T) {
	guards := holderTestGuards(t, 2)
	var reply bytes.Buffer
	opened := false
	err := runHolder(strings.NewReader("INIT 2\nNOOP\n"), &reply,
		func(count int) ([]*os.File, error) {
			if count != 2 {
				t.Fatalf("guard count = %d", count)
			}
			opened = true
			return guards, nil
		}, func(int) (bool, error) {
			t.Fatal("NOOP must not probe a native PID")
			return false, nil
		}, func() { t.Fatal("valid NOOP must finish") })
	if err != nil || !opened || reply.String() != "READY\nACK\n" {
		t.Fatalf("result: opened=%v reply=%q error=%v", opened, reply.String(), err)
	}
	assertHolderGuardsClosed(t, guards)
}

func TestHolderGROUPHoldsUntilConfirmedAbsence(t *testing.T) {
	guards := holderTestGuards(t, 1)
	var reply bytes.Buffer
	probes, waits := 0, 0
	err := runHolder(strings.NewReader("INIT 1\nGROUP 123\n"), &reply,
		func(int) ([]*os.File, error) { return guards, nil },
		func(pid int) (bool, error) {
			if pid != 123 {
				t.Fatalf("PID = %d", pid)
			}
			assertHolderGuardsOpen(t, guards)
			probes++
			if probes == 2 {
				return true, errors.New("absence is uncertain")
			}
			return probes == 4, nil
		}, func() {
			waits++
			assertHolderGuardsOpen(t, guards)
		})
	if err != nil || probes != 4 || waits != 3 || reply.String() != "READY\nACK\n" {
		t.Fatalf("result: probes=%d waits=%d reply=%q error=%v", probes, waits, reply.String(), err)
	}
	assertHolderGuardsClosed(t, guards)
}

func TestHolderMalformedControlNeverReleasesGuards(t *testing.T) {
	cases := []struct {
		name, input, reply string
	}{
		{"EOF before INIT", "", ""},
		{"unknown INIT", "START 1\n", ""},
		{"too many guards", "INIT 9\n", ""},
		{"negative guard count", "INIT -1\n", ""},
		{"noncanonical guard count", "INIT 01\n", ""},
		{"EOF before registration", "INIT 1\n", "READY\n"},
		{"unknown registration", "INIT 1\nEXIT\n", "READY\n"},
		{"zero PID", "INIT 1\nGROUP 0\n", "READY\n"},
		{"negative PID", "INIT 1\nGROUP -123\n", "READY\n"},
		{"noncanonical PID", "INIT 1\nGROUP 0123\n", "READY\n"},
		{"PID overflow", "INIT 1\nGROUP 2147483648\n", "READY\n"},
		{"partial registration", "INIT 1\nGROUP 123", "READY\n"},
		{"CRLF", "INIT 1\r\nNOOP\n", ""},
		{"nonASCII", "INIT 1\nGROUP \xc3\xa9123\n", "READY\n"},
		{"NUL", "INIT 1\nNOOP\x00\n", "READY\n"},
		{"oversized frame", strings.Repeat("A", maxHolderFrameBytes) + "\n", ""},
		{"duplicate INIT", "INIT 1\nINIT 1\n", "READY\n"},
		{"duplicate GROUP", "INIT 1\nGROUP 123\nGROUP 123\n", "READY\nACK\n"},
		{"GROUP trailing byte", "INIT 1\nGROUP 123\nx", "READY\nACK\n"},
		{"duplicate NOOP", "INIT 1\nNOOP\nNOOP\n", "READY\nACK\n"},
		{"NOOP trailing byte", "INIT 1\nNOOP\nx", "READY\nACK\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guards := holderTestGuards(t, 1)
			var reply bytes.Buffer
			expectHolderRetention(t, func(wait func()) error {
				return runHolder(strings.NewReader(tc.input), &reply,
					func(int) ([]*os.File, error) { return guards, nil },
					func(int) (bool, error) {
						t.Fatal("malformed stream must not reach a release-capable probe")
						return true, nil
					}, wait)
			})
			if reply.String() != tc.reply {
				t.Fatalf("reply = %q, want %q", reply.String(), tc.reply)
			}
			assertHolderGuardsOpen(t, guards)
		})
	}
}

func TestHolderFailedGuardSetupRetainsPartialGuards(t *testing.T) {
	guards := holderTestGuards(t, 1)
	var reply bytes.Buffer
	expectHolderRetention(t, func(wait func()) error {
		return runHolder(strings.NewReader("INIT 2\nNOOP\n"), &reply,
			func(int) ([]*os.File, error) { return guards, errors.New("second guard invalid") },
			func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
	})
	if reply.Len() != 0 {
		t.Fatalf("guard setup failure replied %q", reply.String())
	}
	assertHolderGuardsOpen(t, guards)
}

func TestHolderInvalidGuardRetainsAllDescriptors(t *testing.T) {
	for _, usePipe := range []bool{false, true} {
		guards := holderTestGuards(t, 1)
		if usePipe {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
			guards = append(guards, reader)
		} else {
			guards = append(guards, nil)
		}
		var reply bytes.Buffer
		expectHolderRetention(t, func(wait func()) error {
			return runHolder(strings.NewReader("INIT 2\nNOOP\n"), &reply,
				func(int) ([]*os.File, error) { return guards, nil },
				func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
		})
		if reply.Len() != 0 {
			t.Fatalf("invalid guard replied %q", reply.String())
		}
		assertHolderGuardsOpen(t, guards[:1])
		if usePipe {
			assertHolderGuardsOpen(t, guards[1:])
		}
	}
}

func TestHolderREADYFailureRetainsUnknownOwnership(t *testing.T) {
	guards := holderTestGuards(t, 1)
	reply := &holderFailWriter{}
	expectHolderRetention(t, func(wait func()) error {
		return runHolder(strings.NewReader("INIT 1\nNOOP\n"), reply,
			func(int) ([]*os.File, error) { return guards, nil },
			func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
	})
	assertHolderGuardsOpen(t, guards)
}

func TestHolderACKFailurePreservesKnownRegistration(t *testing.T) {
	for _, registration := range []string{"GROUP 123", "NOOP"} {
		t.Run(registration, func(t *testing.T) {
			guards := holderTestGuards(t, 1)
			reply := &holderFailWriter{remaining: len("READY\n")}
			probes, waits := 0, 0
			err := runHolder(strings.NewReader("INIT 1\n"+registration+"\n"), reply,
				func(int) ([]*os.File, error) { return guards, nil },
				func(pid int) (bool, error) {
					if registration == "NOOP" || pid != 123 {
						t.Fatal("unexpected probe")
					}
					assertHolderGuardsOpen(t, guards)
					probes++
					return probes == 2, nil
				}, func() { waits++; assertHolderGuardsOpen(t, guards) })
			if err != nil {
				t.Fatal(err)
			}
			if registration == "GROUP 123" && (probes != 2 || waits != 1) {
				t.Fatalf("known group probes=%d waits=%d", probes, waits)
			}
			if registration == "NOOP" && (probes != 0 || waits != 0) {
				t.Fatalf("NOOP probes=%d waits=%d", probes, waits)
			}
			assertHolderGuardsClosed(t, guards)
		})
	}
}

func TestHolderACKFailureWithTrailingBytesStillRetains(t *testing.T) {
	guards := holderTestGuards(t, 1)
	reply := &holderFailWriter{remaining: len("READY\n")}
	expectHolderRetention(t, func(wait func()) error {
		return runHolder(strings.NewReader("INIT 1\nGROUP 123\nNOOP\n"), reply,
			func(int) ([]*os.File, error) { return guards, nil },
			func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
	})
	assertHolderGuardsOpen(t, guards)
}

func TestHolderDelimiterReadErrorRetainsGuards(t *testing.T) {
	guards := holderTestGuards(t, 1)
	control := &holderNewlineErrorReader{control: strings.NewReader("INIT 1\nNOOP\n")}
	var reply bytes.Buffer
	expectHolderRetention(t, func(wait func()) error {
		return runHolder(control, &reply,
			func(int) ([]*os.File, error) { return guards, nil },
			func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
	})
	if reply.Len() != 0 {
		t.Fatalf("failed delimiter replied %q", reply.String())
	}
	assertHolderGuardsOpen(t, guards)
}

func TestHolderPostRegistrationReadErrorRetainsGuards(t *testing.T) {
	guards := holderTestGuards(t, 1)
	control := io.MultiReader(strings.NewReader("INIT 1\nNOOP\n"), holderErrorReader{})
	var reply bytes.Buffer
	expectHolderRetention(t, func(wait func()) error {
		return runHolder(control, &reply,
			func(int) ([]*os.File, error) { return guards, nil },
			func(int) (bool, error) { t.Fatal("must not probe"); return false, nil }, wait)
	})
	assertHolderGuardsOpen(t, guards)
}

func TestHolderAllowsZeroGuardsForNeutralNOOP(t *testing.T) {
	var reply bytes.Buffer
	err := runHolder(strings.NewReader("INIT 0\nNOOP\n"), &reply,
		func(count int) ([]*os.File, error) {
			if count != 0 {
				t.Fatalf("guard count = %d", count)
			}
			return nil, nil
		}, func(int) (bool, error) { t.Fatal("must not probe"); return false, nil },
		func() { t.Fatal("valid neutral NOOP must finish") })
	if err != nil || reply.String() != "READY\nACK\n" {
		t.Fatalf("reply=%q error=%v", reply.String(), err)
	}
}

func TestHolderFrameBoundIncludesNewline(t *testing.T) {
	frame := strings.Repeat("A", maxHolderFrameBytes-1) + "\n"
	got, err := readHolderFrame(strings.NewReader(frame))
	if err != nil || got != frame[:len(frame)-1] {
		t.Fatalf("bounded frame result length=%d error=%v", len(got), err)
	}
	if _, err := readHolderFrame(strings.NewReader(frame[:len(frame)-1] + "A\n")); err == nil {
		t.Fatal("frame beyond bound accepted")
	}
}

func holderTestGuards(t *testing.T, count int) []*os.File {
	t.Helper()
	files := make([]*os.File, 0, count)
	for i := 0; i < count; i++ {
		file, err := os.CreateTemp(filepath.Clean(t.TempDir()), "guard-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		files = append(files, file)
	}
	return files
}

func assertHolderGuardsOpen(t *testing.T, files []*os.File) {
	t.Helper()
	for _, file := range files {
		if _, err := file.Stat(); err != nil {
			t.Fatalf("guard unexpectedly closed: %v", err)
		}
	}
}

func assertHolderGuardsClosed(t *testing.T, files []*os.File) {
	t.Helper()
	for _, file := range files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("guard was not closed: %v", err)
		}
	}
}

func expectHolderRetention(t *testing.T, run func(func()) error) {
	t.Helper()
	marker := new(int)
	defer func() {
		if value := recover(); value != marker {
			t.Fatalf("retention stopped incorrectly: %v", value)
		}
	}()
	_ = run(func() { panic(marker) })
	t.Fatal("uncertain holder returned instead of retaining guards")
}

type holderFailWriter struct{ remaining int }

func (writer *holderFailWriter) Write(p []byte) (int, error) {
	if writer.remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	n := len(p)
	if n > writer.remaining {
		n = writer.remaining
	}
	writer.remaining -= n
	return n, nil
}

type holderErrorReader struct{}

func (holderErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type holderNewlineErrorReader struct{ control *strings.Reader }

func (reader *holderNewlineErrorReader) Read(p []byte) (int, error) {
	n, err := reader.control.Read(p)
	if n == 1 && p[0] == '\n' {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}
