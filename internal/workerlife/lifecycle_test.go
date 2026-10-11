//go:build darwin || linux

package workerlife

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These source-first fixtures exercise real flock OFDs, exec descriptor roles,
// and process groups. No native model/runtime asset is required. They have NOT
// RUN until the root's separately admitted normal-test lane executes them.
const lifecycleFixturePrefix = "WA_WORKERLIFE_FIXTURE_"

// TestMain handles fixture roles before m.Run parses flags. Config.Args is a
// complete override, so a fixture may have ordinary argv rather than Go flags.
func TestMain(m *testing.M) {
	role := os.Getenv(lifecycleFixturePrefix + "ROLE")
	if role != "" {
		if err := lifecycleFixture(role); err != nil {
			os.Exit(41)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type lifecycleFDReport struct {
	FD, Flags     int
	Device, Inode uint64
}

type lifecycleReport struct {
	Event                                        string
	Identity, NativeIdentity, DescendantIdentity string
	PID, PGID, NativePID, DescendantPID          int
	Args                                         []string
	Executable                                   string
	LoaderOverrides                              []string
	Guards                                       []lifecycleFDReport
	Control, Reply                               int
}

func lifecycleFixture(role string) error {
	switch role {
	case "holder":
		count, err := strconv.Atoi(os.Getenv(lifecycleFixturePrefix + "GUARDS"))
		if err != nil || count < 0 || count > 8 {
			return ErrProtocol
		}
		report := lifecycleReport{Event: "holder-entry", PID: os.Getpid(), Args: os.Args[1:]}
		report.Executable, err = os.Executable()
		if err != nil {
			return err
		}
		for _, value := range os.Environ() {
			key, _, _ := strings.Cut(value, "=")
			if strings.HasPrefix(key, "DYLD_") || key == "LD_PRELOAD" || key == "LD_LIBRARY_PATH" || key == "LD_AUDIT" {
				report.LoaderOverrides = append(report.LoaderOverrides, key)
			}
		}
		report.PGID, err = unix.Getpgid(0)
		if err != nil {
			return err
		}
		report.Identity = lifecycleProcessIdentity(report.PID)
		if report.Identity == "" {
			return ErrUnavailable
		}
		report.Control, err = unix.FcntlInt(3, unix.F_GETFD, 0)
		if err != nil {
			return err
		}
		report.Reply, err = unix.FcntlInt(4, unix.F_GETFD, 0)
		if err != nil {
			return err
		}
		for index := 0; index < count; index++ {
			guard, err := lifecycleDescribeFD(5 + index)
			if err != nil {
				return err
			}
			report.Guards = append(report.Guards, guard)
		}
		// Publication belongs only to this test fixture. Production never exposes
		// a holder PID as permission to kill it or release ownership.
		path := os.Getenv(lifecycleFixturePrefix + "HOLDER_REPORT")
		if path != "" {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			err = json.NewEncoder(file).Encode(report)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if gate := os.Getenv(lifecycleFixturePrefix + "READY_GATE"); gate != "" {
			deadline := time.Now().Add(15 * time.Second)
			for {
				if _, err := os.Stat(gate); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if time.Now().After(deadline) {
					return ErrUnavailable
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if gate := os.Getenv(lifecycleFixturePrefix + "ACK_GATE"); gate != "" {
			if err := lifecycleGateReplyACK(gate, os.Getenv(lifecycleFixturePrefix+"ACK_WAIT")); err != nil {
				return err
			}
		}
		return RunInherited()
	case "native", "tree", "moved-leader":
		if marker := os.Getenv(lifecycleFixturePrefix + "NATIVE_STARTED"); marker != "" {
			if err := os.WriteFile(marker, []byte("started"), 0600); err != nil {
				return err
			}
		}
		for index, expected := range []string{"payload-three", "payload-four"} {
			file := os.NewFile(uintptr(3+index), "fixture-payload")
			if file == nil {
				return ErrProtocol
			}
			body := make([]byte, len(expected))
			if _, err := file.ReadAt(body, 0); err != nil || string(body) != expected {
				return ErrProtocol
			}
			_ = file.Close()
		}
		ready := os.NewFile(5, "fixture-events")
		stop := os.NewFile(6, "fixture-stop")
		if ready == nil || stop == nil {
			return ErrProtocol
		}
		defer ready.Close()
		defer stop.Close()
		count, err := strconv.Atoi(os.Getenv(lifecycleFixturePrefix + "GUARDS"))
		if err != nil || count < 0 || count > 8 {
			return ErrProtocol
		}
		report := lifecycleReport{Event: "native-ready", PID: os.Getpid(), Identity: lifecycleProcessIdentity(os.Getpid())}
		if report.Identity == "" {
			return ErrUnavailable
		}
		for index := 0; index < count; index++ {
			guard, err := lifecycleDescribeFD(7 + index)
			if err != nil {
				return err
			}
			report.Guards = append(report.Guards, guard)
			if err := unix.Close(7 + index); err != nil {
				return err
			}
		}
		if role == "moved-leader" {
			group, err := strconv.Atoi(os.Getenv(lifecycleFixturePrefix + "MOVE_TO_PGID"))
			if err != nil || group <= 1 {
				return ErrProtocol
			}
			if err := unix.Setpgid(0, group); err != nil {
				return err
			}
		}
		report.PGID, err = unix.Getpgid(0)
		if err != nil {
			return err
		}
		if path := os.Getenv(lifecycleFixturePrefix + "NATIVE_REPORT"); path != "" {
			if err := lifecyclePublishReport(path, report); err != nil {
				return err
			}
		}
		if role == "tree" {
			read, write, err := os.Pipe()
			if err != nil {
				return err
			}
			defer read.Close()
			child := exec.Command(os.Args[0])
			child.Env = lifecycleEnvironment("descendant", nil)
			child.ExtraFiles = []*os.File{write, stop}
			child.Stdout, child.Stderr = io.Discard, io.Discard
			if err := child.Start(); err != nil {
				write.Close()
				return err
			}
			write.Close()
			_ = read.SetReadDeadline(time.Now().Add(10 * time.Second))
			var signal [1]byte
			if _, err := io.ReadFull(read, signal[:]); err != nil || signal[0] != 1 {
				_ = child.Process.Kill()
				_ = child.Wait()
				return ErrUnavailable
			}
			report.DescendantPID = child.Process.Pid
			report.DescendantIdentity = lifecycleProcessIdentity(child.Process.Pid)
			if report.DescendantIdentity == "" {
				_ = child.Process.Kill()
				_ = child.Wait()
				return ErrUnavailable
			}
			if path := os.Getenv(lifecycleFixturePrefix + "NATIVE_REPORT"); path != "" {
				if err := lifecyclePublishReport(path, report); err != nil {
					_ = child.Process.Kill()
					_ = child.Wait()
					return err
				}
			}
			if err := json.NewEncoder(ready).Encode(report); err != nil {
				_ = child.Process.Kill()
				_ = child.Wait()
				return err
			}
			// Stay alive independently of the descendant's stop pipe. The outer
			// fixture kills this leader first, then observes the remaining group.
			for {
				time.Sleep(time.Hour)
			}
		}
		if err := json.NewEncoder(ready).Encode(report); err != nil {
			return err
		}
		var signal [1]byte
		_, err = stop.Read(signal[:])
		if err != nil && err != io.EOF {
			return err
		}
		return nil
	case "descendant":
		ready := os.NewFile(3, "descendant-ready")
		stop := os.NewFile(4, "descendant-stop")
		if ready == nil || stop == nil {
			return ErrProtocol
		}
		if _, err := ready.Write([]byte{1}); err != nil {
			return err
		}
		ready.Close()
		var signal [1]byte
		_, err := stop.Read(signal[:])
		stop.Close()
		if err != nil && err != io.EOF {
			return err
		}
		return nil
	case "parent":
		return lifecycleParentFixture()
	default:
		return ErrProtocol
	}
}

func lifecycleParentFixture() error {
	files := make([]*os.File, 4)
	for index := range files {
		files[index] = os.NewFile(uintptr(3+index), "fixture-borrowed")
	}
	guard := os.NewFile(7, "fixture-parent-guard")
	if guard == nil {
		return ErrProtocol
	}
	root := os.Getenv(lifecycleFixturePrefix + "ROOT")
	namespace, err := lifecycleNamespace(filepath.Join(root, "native-exec"))
	if err != nil {
		return err
	}
	defer namespace.Close()
	native := exec.Command(os.Args[0])
	native.Env = lifecycleEnvironment("tree", map[string]string{"GUARDS": "1", "NATIVE_REPORT": filepath.Join(root, "native.json")})
	native.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	native.ExtraFiles = files
	native.Stdout, native.Stderr = io.Discard, io.Discard
	lease, err := Start(context.Background(), Config{Executable: os.Args[0], ExecutableProvider: namespace,
		Environment: lifecycleEnvironment("holder", map[string]string{"GUARDS": "1", "HOLDER_REPORT": filepath.Join(root, "holder.json")}), ReadyTimeout: 10 * time.Second}, native, []*os.File{guard})
	// Start may return a cleanup-owning lease and an error after native.Start.
	// Publish that actual child immediately, independently of native-ready or
	// successful parent-ready events. Do not overwrite its descendant registry.
	owned := lifecycleReport{Event: "parent-native-owned"}
	var publishErr error
	if native.Process != nil {
		owned.PID, owned.PGID = native.Process.Pid, native.Process.Pid
		owned.Identity = lifecycleProcessIdentity(owned.PID)
		if owned.Identity == "" {
			if recorded, ok := lifecycleTryReport(filepath.Join(root, "native.json")); ok && recorded.PID == owned.PID {
				owned.Identity = recorded.Identity
			}
		}
		publishErr = lifecyclePublishReport(filepath.Join(root, "parent-native.json"), owned)
	}
	failed := func(cause error) error {
		defer guard.Close()
		if native.Process != nil {
			// No Wait owner has been launched yet. The unreaped caller-owned
			// child pins this PID, and Setpgid/Pgid0 made it the group's leader.
			// Reap the entire trusted group, including descendants whose fixture
			// event has not arrived; a leader-only Kill would leave them alive.
			_ = unix.Kill(-native.Process.Pid, unix.SIGKILL)
			_ = native.Process.Kill()
			_ = native.Wait() // This consumes the caller's sole Wait even on error.
			if !lifecycleGroupAbsentWithin(native.Process.Pid, 5*time.Second) {
				return errors.Join(cause, ErrCleanup)
			}
		}
		// Holder retention outlives the group reaper and its confirmation. No
		// holder PID is signaled merely because Start returned an error.
		if lease != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr := lease.AwaitCleanup(ctx)
			cancel()
			if cleanupErr != nil {
				return errors.Join(cause, cleanupErr)
			}
		}
		return cause
	}
	if err != nil {
		return failed(err)
	}
	if publishErr != nil {
		return failed(publishErr)
	}
	if owned.PID <= 1 || owned.Identity == "" {
		return failed(ErrUnavailable)
	}
	if err := guard.Close(); err != nil {
		return failed(err)
	}
	if err := json.NewEncoder(files[2]).Encode(lifecycleReport{Event: "parent-ready", PID: os.Getpid(), NativePID: owned.PID, NativeIdentity: owned.Identity}); err != nil {
		return failed(err)
	}
	// This application fixture remains the sole native Wait owner. SIGKILL
	// intentionally interrupts it, while the independent holder survives.
	go func() { _ = native.Wait() }()
	for {
		time.Sleep(time.Hour)
	}
}

func lifecycleEnvironment(role string, values map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(values)+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, lifecycleFixturePrefix) && !strings.HasPrefix(value, "GORACE=") {
			result = append(result, value)
		}
	}
	result = append(result, lifecycleFixturePrefix+"ROLE="+role, "GORACE=atexit_sleep_ms=0")
	for key, value := range values {
		result = append(result, lifecycleFixturePrefix+key+"="+value)
	}
	return result
}

func lifecycleDescribeFD(fd int) (lifecycleFDReport, error) {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return lifecycleFDReport{}, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG {
		return lifecycleFDReport{}, ErrProtocol
	}
	return lifecycleFDReport{FD: fd, Flags: flags, Device: uint64(info.Dev), Inode: uint64(info.Ino)}, nil
}

type lifecycleFixtureState struct {
	root, holderReport, marker, gate             string
	guards                                       []*os.File
	guardPaths                                   []string
	payloads                                     []*os.File
	eventsRead, eventsWrite, stopRead, stopWrite *os.File
	native                                       *exec.Cmd
	lease                                        *Lease
	waited                                       bool
	namespace                                    *ExecutableNamespace
	namespacePath                                string
}

type lifecycleGrowthLease struct{}

func (lifecycleGrowthLease) Release() error { return nil }

func lifecycleNamespace(path string) (*ExecutableNamespace, error) {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	namespace, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: path,
		Admit: func(ctx context.Context, maximum int64) (GrowthLease, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if maximum <= 64<<10 || maximum > (64<<20)+(64<<10) {
				return nil, ErrLimit
			}
			return lifecycleGrowthLease{}, nil
		}})
	if err != nil {
		return nil, err
	}
	if err := namespace.Recover(context.Background()); err != nil {
		_ = namespace.Close()
		return nil, err
	}
	return namespace, nil
}

func newLifecycleFixture(t *testing.T, count int, role string) *lifecycleFixtureState {
	t.Helper()
	root := t.TempDir()
	s := &lifecycleFixtureState{root: root, holderReport: filepath.Join(root, "holder.json"), marker: filepath.Join(root, "native-started"), gate: filepath.Join(root, "allow-ready")}
	s.namespacePath = filepath.Join(root, "native-exec")
	var namespaceErr error
	s.namespace, namespaceErr = lifecycleNamespace(s.namespacePath)
	if namespaceErr != nil {
		t.Fatal(namespaceErr)
	}
	t.Cleanup(func() {
		for _, file := range s.guards {
			_ = file.Close()
		}
		for _, file := range s.payloads {
			_ = file.Close()
		}
		if s.stopWrite != nil {
			_ = s.stopWrite.Close()
		}
		if s.native != nil && s.native.Process != nil && !s.waited {
			// The caller-owned Process handle is authoritative until our Wait.
			_ = s.native.Process.Kill()
			_ = s.native.Wait()
			s.waited = true
		}
		if s.lease != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cleaned := s.lease.AwaitCleanup(ctx) == nil
			cancel()
			if !cleaned && !lifecycleHolderReaped(s.lease) {
				if report, ok := lifecycleTryReport(s.holderReport); ok {
					lifecycleKillOwnedGroup(report)
				}
			}
		}
		for _, file := range []*os.File{s.stopRead, s.eventsWrite, s.eventsRead} {
			if file != nil {
				_ = file.Close()
			}
		}
		if err := s.namespace.Close(); err != nil {
			t.Error("fixture namespace cleanup remained owned", err)
		}
	})
	for index := 0; index < count; index++ {
		path := filepath.Join(root, fmt.Sprintf("guard-%d", index))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			file.Close()
			t.Fatal(err)
		}
		s.guards = append(s.guards, file)
		s.guardPaths = append(s.guardPaths, path)
	}
	for index, body := range []string{"payload-three", "payload-four"} {
		file, err := os.CreateTemp(root, fmt.Sprintf("payload-%d-", index))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(body); err != nil {
			file.Close()
			t.Fatal(err)
		}
		s.payloads = append(s.payloads, file)
	}
	var err error
	s.eventsRead, s.eventsWrite, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s.stopRead, s.stopWrite, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s.native = exec.Command(os.Args[0])
	s.native.Env = lifecycleEnvironment(role, map[string]string{"GUARDS": strconv.Itoa(count), "NATIVE_STARTED": s.marker, "MOVE_TO_PGID": strconv.Itoa(unix.Getpgrp())})
	s.native.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.native.ExtraFiles = append(append([]*os.File(nil), s.payloads...), s.eventsWrite, s.stopRead)
	s.native.Stdout, s.native.Stderr = io.Discard, io.Discard

	return s
}

func (s *lifecycleFixtureState) config(count int) Config {
	return Config{Executable: os.Args[0], ExecutableProvider: s.namespace, ReadyTimeout: 10 * time.Second,
		Environment: lifecycleEnvironment("holder", map[string]string{"GUARDS": strconv.Itoa(count), "HOLDER_REPORT": s.holderReport})}
}

func (s *lifecycleFixtureState) closeBorrowedGuards(t *testing.T) {
	t.Helper()
	for _, file := range s.guards {
		if _, err := file.Stat(); err != nil {
			t.Fatal("Start closed a borrowed guard", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s.guards = nil
}

func (s *lifecycleFixtureState) stopAndWait(t *testing.T) {
	t.Helper()
	if err := s.stopWrite.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.native.Wait()
	s.waited = true
	if err != nil {
		t.Fatal("native caller Wait failed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.lease.AwaitCleanup(ctx); err != nil {
		t.Fatal("holder cleanup", err)
	}
}

func TestStartPreservesDescriptorRolesAndInheritedGuards(t *testing.T) {
	for _, count := range []int{0, 2, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := newLifecycleFixture(t, count, "native")
			original := append([]*os.File(nil), s.native.ExtraFiles...)
			lease, err := Start(context.Background(), s.config(count), s.native, s.guards)
			s.lease = lease
			if err != nil || lease == nil {
				t.Fatal("Start", err)
			}
			if !reflect.DeepEqual(s.native.ExtraFiles, original) {
				t.Fatal("Start mutated the caller's ExtraFiles roles")
			}
			native := lifecycleReadEvent(t, s.eventsRead)
			holder := lifecycleWaitReport(t, s.holderReport)
			if native.Event != "native-ready" || native.PID != s.native.Process.Pid || native.PGID != native.PID {
				t.Fatal("native did not receive its own group")
			}
			if holder.PGID != holder.PID || holder.PID == native.PID || holder.PGID == unix.Getpgrp() {
				t.Fatal("holder group is not independent")
			}
			if !reflect.DeepEqual(holder.Args, []string{HolderModeArgument}) {
				t.Fatal("nil Args did not select the hidden holder mode")
			}
			if holder.Control&unix.FD_CLOEXEC != 0 || holder.Reply&unix.FD_CLOEXEC != 0 {
				t.Fatal("holder control descriptors have CLOEXEC")
			}
			if len(holder.Guards) != count || len(native.Guards) != count {
				t.Fatal("inherited guard count mismatch")
			}
			for index, guard := range s.guards {
				parent, err := lifecycleDescribeFD(int(guard.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				for _, inherited := range []lifecycleFDReport{holder.Guards[index], native.Guards[index]} {
					if inherited.Flags&unix.FD_CLOEXEC != 0 || inherited.Device != parent.Device || inherited.Inode != parent.Inode {
						t.Fatal("guard inheritance changed the file or CLOEXEC state")
					}
				}
				if holder.Guards[index].FD != 5+index || native.Guards[index].FD != 7+index {
					t.Fatal("guard overwrote an existing descriptor role")
				}
			}
			s.closeBorrowedGuards(t)
			for _, path := range s.guardPaths {
				lifecycleAssertLocked(t, path)
			}
			s.stopAndWait(t)
			for _, path := range s.guardPaths {
				lifecycleAssertUnlocked(t, path)
			}
		})
	}
}

func TestStartCompleteArgsAndBorrowedExecutableFile(t *testing.T) {
	s := newLifecycleFixture(t, 1, "native")
	executable, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	config := s.config(1)
	config.Executable = filepath.Join(s.root, "missing-path-must-not-be-used")
	config.ExecutableFile = executable
	config.Args = []string{"fixture-argument", "literal-value"}
	s.lease, err = Start(context.Background(), config, s.native, s.guards)
	if err != nil || s.lease == nil {
		t.Fatal("borrowed executable Start", err)
	}
	if _, err := executable.Stat(); err != nil {
		t.Fatal("Start closed the borrowed executable", err)
	}
	holder := lifecycleWaitReport(t, s.holderReport)
	if !reflect.DeepEqual(holder.Args, config.Args) {
		t.Fatal("Start changed the complete argv override")
	}
	_ = lifecycleReadEvent(t, s.eventsRead)
	s.closeBorrowedGuards(t)
	s.stopAndWait(t)
}

func TestStartCancellationBeforeREADYRetainsOwnerAndPreventsLateNativeStart(t *testing.T) {
	s := newLifecycleFixture(t, 1, "native")
	config := s.config(1)
	config.Environment = append(config.Environment, lifecycleFixturePrefix+"READY_GATE="+s.gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() { lease, err := Start(ctx, config, s.native, s.guards); done <- result{lease, err} }()
	consumed := false
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(s.gate, []byte("ready"), 0600)
		if !consumed {
			select {
			case got := <-done:
				s.lease = got.lease
			case <-time.After(10 * time.Second):
				t.Error("Start fixture owner did not return")
			}
		}
	})
	_ = lifecycleWaitReport(t, s.holderReport)
	if _, err := os.Stat(s.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native started before holder READY")
	}
	cancel()
	select {
	case got := <-done:
		consumed = true
		s.lease = got.lease
		if got.lease == nil || !errors.Is(got.err, context.Canceled) {
			t.Fatal("public cancellation lost cleanup ownership", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not respect its public wait cancellation")
	}
	s.closeBorrowedGuards(t)
	lifecycleAssertLocked(t, s.guardPaths[0])
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := s.lease.AwaitCleanup(short); !errors.Is(err, context.DeadlineExceeded) {
		stop()
		t.Fatal("unfinished handshake released its lease", err)
	}
	stop()
	if err := os.WriteFile(s.gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	complete, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := s.lease.AwaitCleanup(complete); err != nil {
		t.Fatal("late READY did not complete acknowledged NOOP", err)
	}
	if s.native.Process != nil {
		t.Fatal("background protocol owner started native after early return")
	}
	if _, err := os.Stat(s.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late native fixture execution")
	}
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}

func TestStartCancellationAfterSpawnRetainsUntilCallerReaps(t *testing.T) {
	s := newLifecycleFixture(t, 1, "native")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := s.config(1)
	ackGate, ackWait := filepath.Join(s.root, "allow-ack"), filepath.Join(s.root, "ack-waiting")
	config.Environment = append(config.Environment, lifecycleFixturePrefix+"ACK_GATE="+ackGate, lifecycleFixturePrefix+"ACK_WAIT="+ackWait)
	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() { lease, err := Start(ctx, config, s.native, s.guards); done <- result{lease, err} }()
	consumed := false
	t.Cleanup(func() {
		cancel()
		_ = os.WriteFile(ackGate, []byte("ready"), 0600)
		if !consumed {
			select {
			case got := <-done:
				s.lease = got.lease
			case <-time.After(10 * time.Second):
				t.Error("Start fixture owner did not return")
			}
		}
	})
	_ = lifecycleReadEvent(t, s.eventsRead)
	lifecycleWaitMarker(t, ackWait)
	cancel()
	select {
	case got := <-done:
		consumed = true
		s.lease = got.lease
		if got.lease == nil || !errors.Is(got.err, context.Canceled) {
			t.Fatal("post-spawn cancellation lost its owner", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not cancel its ACK wait")
	}
	s.closeBorrowedGuards(t)
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := s.lease.AwaitCleanup(short); !errors.Is(err, context.DeadlineExceeded) {
		stop()
		t.Fatal("canceling ACK wait released live native ownership", err)
	}
	stop()
	lifecycleAssertLocked(t, s.guardPaths[0])
	if s.native.ProcessState != nil {
		t.Fatal("Start stole native.Wait from its caller")
	}
	if err := os.WriteFile(ackGate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	s.stopAndWait(t)
	if s.native.ProcessState == nil || !s.native.ProcessState.Success() {
		t.Fatal("caller did not own successful native reap")
	}
	if err := s.lease.AwaitCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}

// A structural interface keeps the branch tests compilable before the bounded
// production diagnostic exists, so unchanged launch returns fail assertions.
type lifecycleLaunchDiagnostic interface {
	error
	Stage() string
	ErrnoKnown() bool
	Errno() uint32
}

func lifecycleAssertLaunchFailure(t *testing.T, err error, stage string, known bool, errno uint32) lifecycleLaunchDiagnostic {
	t.Helper()
	var failure lifecycleLaunchDiagnostic
	if err == nil || !errors.As(err, &failure) || failure == nil {
		t.Fatal("launch failure lost its bounded diagnostic")
	}
	if failure.Stage() != stage || failure.ErrnoKnown() != known || failure.Errno() != errno {
		t.Fatal("launch failure changed its fixed stage or numeric errno")
	}
	if err.Error() != ErrUnavailable.Error() || !errors.Is(err, ErrUnavailable) || errors.Unwrap(failure) != ErrUnavailable {
		t.Fatal("launch failure changed its fixed public error boundary")
	}
	var pathError *os.PathError
	if errors.As(failure, &pathError) {
		t.Fatal("launch failure retained a private path error")
	}
	return failure
}

func TestLaunchFailureSanitizesSyscallCause(t *testing.T) {
	overlarge := syscall.Errno(^uint32(0))
	overlarge++
	for _, stage := range []struct {
		name  string
		value launchStage
	}{
		{name: "holder_start", value: holderLaunchStage},
		{name: "native_start", value: nativeLaunchStage},
	} {
		for _, sample := range []struct {
			name  string
			cause error
			known bool
			errno uint32
		}{
			{name: "nil-cause"},
			{name: "zero-errno", cause: syscall.Errno(0)},
			{name: "oversize-errno", cause: overlarge},
			{name: "path-errno", cause: &os.PathError{Op: "private-operation-canary", Path: "/private/path-canary", Err: syscall.EACCES}, known: true, errno: uint32(syscall.EACCES)},
			{name: "wrapped-errno", cause: fmt.Errorf("private-argv-canary: %w", syscall.ENOENT), known: true, errno: uint32(syscall.ENOENT)},
			{name: "unknown-errno", cause: errors.New("private-stderr-canary")},
		} {
			t.Run(stage.name+"/"+sample.name, func(t *testing.T) {
				failure := newLaunchFailure(stage.value, sample.cause)
				lifecycleAssertLaunchFailure(t, failure, stage.name, sample.known, sample.errno)
				if errors.Is(failure, sample.cause) || strings.Contains(failure.Error(), "canary") {
					t.Fatal("launch failure exposed its discarded raw cause")
				}
				var diagnostic LaunchFailure
				if !errors.As(failure, &diagnostic) {
					t.Fatal("launch failure lost its value diagnostic")
				}
				diagnostic = LaunchFailure{}
				lifecycleAssertLaunchFailure(t, failure, stage.name, sample.known, sample.errno)
				kind := reflect.TypeOf(failure)
				for index := 0; index < kind.NumField(); index++ {
					if kind.Field(index).PkgPath == "" {
						t.Fatal("launch diagnostic fields must remain private")
					}
				}
			})
		}
	}
}

func TestNativeStartFailureCompletesAcknowledgedNOOP(t *testing.T) {
	s := newLifecycleFixture(t, 1, "native")
	s.native.Path = filepath.Join(s.root, "missing-native")
	original := append([]*os.File(nil), s.native.ExtraFiles...)
	var err error
	s.lease, err = Start(context.Background(), s.config(1), s.native, s.guards)
	if err == nil || s.lease == nil || s.native.Process != nil {
		t.Fatal("failed native start lost the NOOP cleanup owner", err)
	}
	lifecycleAssertLaunchFailure(t, err, "native_start", true, uint32(syscall.ENOENT))
	if !reflect.DeepEqual(s.native.ExtraFiles, original) {
		t.Fatal("failed start changed descriptor roles")
	}
	s.closeBorrowedGuards(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.lease.AwaitCleanup(ctx); err != nil {
		t.Fatal("native start failure never acknowledged NOOP", err)
	}
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}

func TestStartRejectsInvalidOwnershipBeforeStartingNative(t *testing.T) {
	for _, name := range []string{"nil-context", "nil-native", "no-group", "foreign-group", "new-session", "too-many-guards", "nil-guard", "pipe-guard", "closed-guard", "negative-ready-timeout"} {
		t.Run(name, func(t *testing.T) {
			s := newLifecycleFixture(t, 1, "native")
			ctx := context.Background()
			config, native, guards := s.config(1), s.native, s.guards
			switch name {
			case "nil-context":
				ctx = nil
			case "nil-native":
				native = nil
			case "no-group":
				native.SysProcAttr = nil
			case "foreign-group":
				native.SysProcAttr.Pgid = unix.Getpgrp()
			case "new-session":
				native.SysProcAttr.Setsid = true
			case "too-many-guards":
				guards = make([]*os.File, 9)
				for index := range guards {
					guards[index] = s.guards[0]
				}
			case "nil-guard":
				guards = []*os.File{nil}
			case "pipe-guard":
				guards = []*os.File{s.stopRead}
			case "closed-guard":
				_ = s.guards[0].Close()
			case "negative-ready-timeout":
				config.ReadyTimeout = -time.Second
			}
			lease, err := Start(ctx, config, native, guards)
			if err == nil || lease != nil || s.native.Process != nil {
				t.Fatal("invalid ownership started a helper/native or returned a lease", err)
			}
			if _, err := os.Stat(s.holderReport); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid ownership executed the holder")
			}
		})
	}
}

func TestLeaseRetainsWhenGroupAbsentButNativeLeaderLives(t *testing.T) {
	s := newLifecycleFixture(t, 1, "moved-leader")
	var err error
	s.lease, err = Start(context.Background(), s.config(1), s.native, s.guards)
	if err != nil || s.lease == nil {
		t.Fatal("Start", err)
	}
	report := lifecycleReadEvent(t, s.eventsRead)
	if report.PGID != unix.Getpgrp() || report.PGID == report.PID {
		t.Fatal("fixture did not move its still-live leader")
	}
	if err := unix.Kill(-report.PID, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatal("original fixture group remains", err)
	}
	if err := unix.Kill(report.PID, 0); err != nil {
		t.Fatal("fixture leader is absent", err)
	}
	s.closeBorrowedGuards(t)
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := s.lease.AwaitCleanup(short); !errors.Is(err, context.DeadlineExceeded) {
		stop()
		t.Fatal("group absence alone released a live leader", err)
	}
	stop()
	lifecycleAssertLocked(t, s.guardPaths[0])
	s.stopAndWait(t)
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}

func TestInheritedHolderEOFWithoutRegistrationNeverReleases(t *testing.T) {
	for _, sendInit := range []bool{false, true} {
		t.Run(strconv.FormatBool(sendInit), func(t *testing.T) {
			s := newLifecycleFixture(t, 1, "native")
			controlRead, controlWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			replyRead, replyWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer controlWrite.Close()
			defer replyRead.Close()
			helper := exec.Command(os.Args[0], HolderModeArgument)
			helper.Env = lifecycleEnvironment("holder", map[string]string{"GUARDS": "1", "HOLDER_REPORT": s.holderReport})
			helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			helper.ExtraFiles = []*os.File{controlRead, replyWrite, s.guards[0]}
			helper.Stdout, helper.Stderr = io.Discard, io.Discard
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			controlRead.Close()
			replyWrite.Close()
			done := make(chan error, 1)
			go func() { done <- helper.Wait() }()
			defer func() {
				_ = helper.Process.Kill()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("unknown holder fixture was not reaped")
				}
			}()
			_ = lifecycleWaitReport(t, s.holderReport)
			if sendInit {
				if _, err := io.WriteString(controlWrite, "INIT 1\n"); err != nil {
					t.Fatal(err)
				}
				_ = replyRead.SetReadDeadline(time.Now().Add(10 * time.Second))
				line, err := bufio.NewReaderSize(replyRead, 256).ReadString('\n')
				if err != nil || line != "READY\n" {
					t.Fatal("holder INIT readiness", err)
				}
			}
			controlWrite.Close()
			s.closeBorrowedGuards(t)
			select {
			case err := <-done:
				t.Fatal("EOF without registration released its holder", err)
			case <-time.After(150 * time.Millisecond):
			}
			lifecycleAssertLocked(t, s.guardPaths[0])
		})
	}
}

func TestParentSIGKILLHolderRetainsAfterNativeClosesGuardsAndLeaderExits(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("parent-death fixture needs a verified orphan reaper; Linux adoption prerequisite NOT_RUN")
	}
	s := newLifecycleFixture(t, 1, "native")
	parent := exec.Command(os.Args[0])
	parent.Env = lifecycleEnvironment("parent", map[string]string{"ROOT": s.root})
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	parent.ExtraFiles = append(append([]*os.File(nil), s.payloads...), s.eventsWrite, s.stopRead, s.guards[0])
	parent.Stdout, parent.Stderr = io.Discard, io.Discard
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	var native, parentReady lifecycleReport
	defer func() {
		_ = parent.Process.Kill()
		if !waited {
			_ = parent.Wait()
			waited = true
		}
		// Close the test-owned stop writer first, then terminate any still-live
		// owned group/member. The registry covers either event arrival order.
		_ = s.stopWrite.Close()
		owned := native
		if recorded, ok := lifecycleTryReport(filepath.Join(s.root, "native.json")); ok {
			owned = recorded
		}
		if owned.PID <= 1 {
			if published, ok := lifecycleTryReport(filepath.Join(s.root, "parent-native.json")); ok {
				owned = published
			}
		}
		if owned.PID <= 1 && parentReady.NativePID > 1 {
			owned = lifecycleReport{PID: parentReady.NativePID, PGID: parentReady.NativePID, Identity: parentReady.NativeIdentity}
		}
		if owned.PID > 1 {
			lifecycleKillOwnedGroup(owned)
		}
		gone := owned.PID > 1 && lifecycleGroupAbsentWithin(owned.PID, 5*time.Second)
		if !gone {
			t.Error("native fixture cleanup is unconfirmed; holder intentionally retained")
			return
		}
		if holder, ok := lifecycleTryReport(s.holderReport); ok {
			// No stale-PID signal follows confirmed holder exit. A fallback kill
			// additionally verifies the recorded live process birth identity.
			if lifecycleProcessIdentity(holder.PID) == holder.Identity {
				lifecycleKillOwnedGroup(holder)
			}
		}
	}()
	s.closeBorrowedGuards(t)
	for count := 0; count < 2; count++ {
		report := lifecycleReadEvent(t, s.eventsRead)
		switch report.Event {
		case "native-ready":
			native = report
		case "parent-ready":
			parentReady = report
		default:
			t.Fatal("unexpected fixture event")
		}
	}
	if native.PID <= 1 || native.PGID != native.PID || native.DescendantPID <= 1 || parentReady.NativePID != native.PID {
		t.Fatal("fixture tree registration failed")
	}
	holder := lifecycleWaitReport(t, s.holderReport)
	if holder.PGID != holder.PID || holder.PID == native.PID || holder.PID == parent.Process.Pid {
		t.Fatal("holder shares an application/native group")
	}
	lifecycleAssertLocked(t, s.guardPaths[0])
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("parent survived SIGKILL")
	}
	waited = true
	if err := unix.Kill(native.PID, 0); err != nil {
		t.Fatal("parent death killed the trusted native", err)
	}
	if entries, err := os.ReadDir(s.namespacePath); err != nil || len(entries) == 0 {
		t.Fatal("parent death lost the retained private executable namespace", err)
	}
	lifecycleAssertLocked(t, s.guardPaths[0])
	if err := unix.Kill(native.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	lifecycleWaitAbsent(t, native.PID, false)
	if err := unix.Kill(-native.PID, 0); err != nil {
		t.Fatal("fixture descendant did not retain the group", err)
	}
	lifecycleAssertLocked(t, s.guardPaths[0])
	// The descendant inherits only its ready/stop pipes, never a guard. Closing
	// this test-owned pipe allows group disappearance without killing holder.
	if err := s.stopWrite.Close(); err != nil {
		t.Fatal(err)
	}
	lifecycleWaitAbsent(t, native.PID, true)
	deadline := time.Now().Add(10 * time.Second)
	for lifecycleLocked(t, s.guardPaths[0]) {
		if time.Now().After(deadline) {
			t.Fatal("holder retained the guard after leader and group absence")
		}
		time.Sleep(20 * time.Millisecond)
	}
	lifecycleWaitAbsent(t, holder.PID, false)
	// Startup reconciliation is authorized only after the existing guard can
	// be reacquired and the recorded fixture holder is positively absent.
	if err := s.namespace.Recover(context.Background()); err != nil {
		t.Fatal("post-death namespace recovery", err)
	}
	if entries, err := os.ReadDir(s.namespacePath); err != nil || len(entries) != 0 {
		t.Fatal("recovered namespace still retains executable bytes", err)
	}
}

func lifecycleReadEvent(t *testing.T, file *os.File) lifecycleReport {
	t.Helper()
	if err := file.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// One-byte reads cannot buffer and lose the next event from a shared pipe.
	var line []byte
	var next [1]byte
	for len(line) < 4096 {
		if _, err := io.ReadFull(file, next[:]); err != nil {
			t.Fatal("fixture event", err)
		}
		line = append(line, next[0])
		if next[0] == '\n' {
			var report lifecycleReport
			if err := json.Unmarshal(line, &report); err != nil {
				t.Fatal(err)
			}
			return report
		}
	}
	t.Fatal("fixture event exceeded its finite bound")
	return lifecycleReport{}
}

func lifecycleTryReport(path string) (lifecycleReport, bool) {
	body, err := os.ReadFile(path)
	if err != nil || len(body) > 4096 {
		return lifecycleReport{}, false
	}
	var report lifecycleReport
	if json.Unmarshal(body, &report) != nil || report.PID <= 1 || report.PGID != report.PID {
		return lifecycleReport{}, false
	}
	return report, true
}

func lifecycleWaitReport(t *testing.T, path string) lifecycleReport {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if report, ok := lifecycleTryReport(path); ok {
			return report
		}
		if time.Now().After(deadline) {
			t.Fatal("holder fixture did not publish its test-only PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lifecycleLocked(t *testing.T, path string) bool {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true
	}
	if err != nil {
		t.Fatal("independent flock probe", err)
	}
	return false
}

func lifecycleAssertLocked(t *testing.T, path string) {
	t.Helper()
	if !lifecycleLocked(t, path) {
		t.Fatal("live holder lost the shared flock OFD")
	}
}
func lifecycleAssertUnlocked(t *testing.T, path string) {
	t.Helper()
	if lifecycleLocked(t, path) {
		t.Fatal("stopped native/holder retained the flock")
	}
}

func lifecycleHolderReaped(lease *Lease) bool {
	// This internal channel is used only for test teardown; it adds no product
	// PID/force-release API and does not affect the public behavior assertions.
	select {
	case <-lease.helperExited:
		return true
	default:
		return false
	}
}

func lifecycleProcessIdentity(pid int) string {
	if pid <= 1 {
		return ""
	}
	if runtime.GOOS == "linux" {
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return ""
		}
		end := strings.LastIndexByte(string(body), ')')
		if end < 0 {
			return ""
		}
		fields := strings.Fields(string(body[end+1:]))
		if len(fields) <= 19 {
			return ""
		}
		return "linux-start-ticks:" + fields[19]
	}
	// Darwin's installed ps provides a body-free process birth identity. This
	// fixed bounded read never captures environment, archive text or model data.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	command.Stderr = io.Discard
	body, err := command.Output()
	if err != nil || len(body) > 128 {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func lifecycleKillOwnedGroup(report lifecycleReport) {
	pid := report.PID
	if pid <= 1 || pid == os.Getpid() || pid == unix.Getpgrp() {
		return
	}
	matched := report.Identity != "" && lifecycleProcessIdentity(pid) == report.Identity
	if matched {
		group, err := unix.Getpgid(pid)
		if err == nil && group == pid {
			_ = unix.Kill(-pid, unix.SIGKILL)
			return
		}
	}
	if report.DescendantPID > 1 && report.DescendantIdentity != "" && lifecycleProcessIdentity(report.DescendantPID) == report.DescendantIdentity {
		group, err := unix.Getpgid(report.DescendantPID)
		if err == nil && group == pid {
			_ = unix.Kill(-pid, unix.SIGKILL)
		}
	}
}

func lifecycleGroupAbsentWithin(pid int, duration time.Duration) bool {
	deadline := time.Now().Add(duration)
	for {
		if errors.Is(unix.Kill(pid, 0), unix.ESRCH) && errors.Is(unix.Kill(-pid, 0), unix.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lifecyclePublishReport(path string, report lifecycleReport) error {
	body, err := json.Marshal(report)
	if err != nil || len(body) > 4096 {
		return ErrProtocol
	}
	if err := os.WriteFile(path+".next", body, 0600); err != nil {
		return err
	}
	return os.Rename(path+".next", path)
}

func lifecycleWaitMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture phase marker timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lifecycleGateReplyACK(gate, waiting string) error {
	fd, err := unix.Dup(4)
	if err != nil {
		return err
	}
	original := os.NewFile(uintptr(fd), "fixture-real-reply")
	read, write, err := os.Pipe()
	if err != nil {
		original.Close()
		return err
	}
	if err := unix.Dup2(int(write.Fd()), 4); err != nil {
		original.Close()
		read.Close()
		write.Close()
		return err
	}
	write.Close()
	go func() {
		defer read.Close()
		defer original.Close()
		for _, expected := range []string{"READY", "ACK"} {
			frame, err := readHolderFrame(read)
			if err != nil || frame != expected {
				return
			}
			if frame == "ACK" {
				if err := os.WriteFile(waiting, []byte("waiting"), 0600); err != nil {
					return
				}
				deadline := time.Now().Add(15 * time.Second)
				for {
					if _, err := os.Stat(gate); err == nil {
						break
					}
					if time.Now().After(deadline) {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if writeHolderFrame(original, frame) != nil {
				return
			}
		}
	}()
	return nil
}

func lifecycleWaitAbsent(t *testing.T, pid int, group bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	target := pid
	if group {
		target = -pid
	}
	for {
		err := unix.Kill(target, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatal("fixture process/group remained present", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
