//go:build darwin || linux

package workerlife

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This synthetic diagnostic exposes raw os/exec errors only to the private Go
// test output. It does not change Start's fixed product error boundary. Both
// variants inherit the same fd3 control, fd4 reply and fd5 executable roles;
// neither starts a native process or owns any model/instance/media guard.
func TestDiagnosticHolderExecutablePaths(t *testing.T) {
	if os.Getenv(lifecycleFixturePrefix+"EXEC_DIAGNOSTIC") != "1" {
		t.Skip("private executable cause sample requires explicit diagnostic opt-in")
	}
	for _, variant := range []struct {
		name            string
		childDescriptor bool
	}{
		{name: "child-executable-fd", childDescriptor: true},
		{name: "trusted-absolute-path"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			executable, err := executableFile(Config{Executable: os.Args[0]})
			if err != nil {
				t.Fatal("synthetic executable setup", err)
			}
			defer executable.Close()
			info, err := executable.Stat()
			if err != nil {
				t.Fatal(err)
			}
			controlRead, controlWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer controlRead.Close()
			defer controlWrite.Close()
			replyRead, replyWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer replyRead.Close()
			defer replyWrite.Close()
			path := os.Args[0]
			if variant.childDescriptor {
				path = "/dev/fd/5"
			}
			reportPath := filepath.Join(t.TempDir(), "holder.json")
			helper := exec.Command(path, HolderModeArgument)
			helper.Env = lifecycleEnvironment("holder", map[string]string{
				"GUARDS": "0", "HOLDER_REPORT": reportPath,
			})
			helper.Stdout, helper.Stderr = io.Discard, io.Discard
			helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			helper.ExtraFiles = []*os.File{controlRead, replyWrite, executable}
			t.Logf("synthetic executable mode=%s parent-fd=%d child-fd=5 path=%s", info.Mode(), executable.Fd(), path)
			if err := helper.Start(); err != nil {
				var errno syscall.Errno
				found := errors.As(err, &errno)
				t.Errorf("raw synthetic Start error: type=%T error=%v errno_found=%t errno=%d", err, err, found, errno)
				return
			}
			// Until this sole Wait, the unreaped child pins its actual PID. The
			// diagnostic has no guards and only this isolated synthetic group.
			// Teardown kills that group before Wait even after a successful ACK;
			// this proves executable/role setup, not natural holder release.
			defer func() {
				_ = unix.Kill(-helper.Process.Pid, unix.SIGKILL)
				_ = helper.Process.Kill()
				_ = helper.Wait()
			}()
			controlRead.Close()
			replyWrite.Close()
			deadline := time.Now().Add(5 * time.Second)
			if err := controlWrite.SetWriteDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := replyRead.SetReadDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := writeHolderFrame(controlWrite, "INIT 0"); err != nil {
				t.Fatal("synthetic INIT", err)
			}
			frame, err := readHolderFrame(replyRead)
			if err != nil || frame != "READY" {
				t.Fatal("synthetic READY", frame, err)
			}
			if err := writeHolderFrame(controlWrite, "NOOP"); err != nil {
				t.Fatal("synthetic NOOP", err)
			}
			frame, err = readHolderFrame(replyRead)
			if err != nil || frame != "ACK" {
				t.Fatal("synthetic ACK", frame, err)
			}
			controlWrite.Close()
			report, ok := lifecycleTryReport(reportPath)
			if !ok || report.PID != helper.Process.Pid || report.PGID != helper.Process.Pid || report.Control&unix.FD_CLOEXEC != 0 || report.Reply&unix.FD_CLOEXEC != 0 {
				t.Fatal("synthetic inherited control/reply role mismatch", report)
			}
			t.Logf("synthetic READY/NOOP/ACK received; helper_pid=%d helper_pgid=%d control_flags=%d reply_flags=%d", report.PID, report.PGID, report.Control, report.Reply)
		})
	}
}
