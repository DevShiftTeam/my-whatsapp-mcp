package remoteaccess

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

// CloudflareOrigin is the dedicated same-namespace ingress. Dashboard routing
// must set Host to the saved public authority and preserve client Origin.
const CloudflareOrigin = "http://127.0.0.1:8124"
const CloudflareVersion = "2026.10.0"

type cloudflareAdapter struct {
	paths   appdata.Paths
	lease   *appdata.InstanceLock
	verify  func(context.Context, string) error
	command func(string, []string, []string) *exec.Cmd
	grace   time.Duration
}

// NewCloudflareAdapter performs no IO, download, process launch or connection.
// Packaging puts the pinned helper next to the application executable.
func NewCloudflareAdapter(paths appdata.Paths, lease *appdata.InstanceLock) Adapter {
	return &cloudflareAdapter{paths: paths, lease: lease, verify: verifyCloudflare, command: cloudflareCommand, grace: time.Second}
}
func cloudflareCommand(path string, args, env []string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	return cmd
}
func cloudflareEnvironment(home string) []string {
	// Deliberate allowlist: no provider credentials, config, proxy, loader,
	// diagnostic, transport or inherited HOME/PATH overrides.
	return []string{"HOME=" + home, "LANG=C", "LC_ALL=C"}
}
func cloudflareFDPath(fd int) string {
	prefix := "/proc/self/fd/"
	if runtime.GOOS == "darwin" {
		prefix = "/dev/fd/"
	}
	return prefix + strconv.Itoa(fd)
}
func (a *cloudflareAdapter) Open(ctx context.Context, i Intent, key Secret, _ func(Notice)) (Session, error) {
	if ctx.Err() != nil {
		return nil, ErrTransient
	}
	if validateIntent(i) != nil || i.Provider != Cloudflare || !i.Enabled || i.NodeName != "" {
		return nil, ErrInvalidIntent
	}
	if len(key) == 0 || len(key) > maxSecretBytes || strings.ContainsAny(string(key), " \t\r\n\x00") {
		return nil, ErrCredentialsRejected
	}
	app, err := os.Executable()
	if err != nil {
		return nil, ErrState
	}
	helper := filepath.Join(filepath.Dir(app), "cloudflared")
	if err = a.verify(ctx, helper); err != nil {
		return nil, err
	}
	dir, err := appdata.OpenRemoteProviderDirectory(a.paths, a.lease, appdata.CloudflareProvider)
	if err != nil {
		return nil, ErrState
	}
	owned := false
	defer func() {
		if !owned {
			_ = dir.Close()
		}
	}()
	home, err := dir.Path()
	if err != nil {
		return nil, ErrState
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:8124")
	if err != nil {
		return nil, ErrState
	}
	defer func() {
		if !owned {
			_ = listener.Close()
		}
	}()
	config, err := cloudflareSnapshot(dir, "config.yml", []byte("{}\n"))
	if err != nil {
		return nil, ErrState
	}
	defer config.Close()
	token, err := cloudflareSnapshot(dir, "token", key)
	if err != nil {
		return nil, ErrState
	}
	defer token.Close()
	if ctx.Err() != nil {
		return nil, ErrTransient
	}
	if dir.Validate() != nil {
		return nil, ErrState
	}
	cmd := a.command(helper, []string{"tunnel", "--no-autoupdate", "--config", cloudflareFDPath(3), "--metrics", "127.0.0.1:8125", "run", "--token-file", cloudflareFDPath(4)}, cloudflareEnvironment(home))
	cmd.ExtraFiles = []*os.File{config, token}
	output := &cloudflareOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	// Bound os/exec copy workers if the trusted helper ever leaves inherited pipes
	// open. The process is always reaped before registration is released.
	cmd.WaitDelay = time.Second
	if cmd.Start() != nil {
		output.discard()
		return nil, ErrState
	}
	s := &cloudflareSession{origin: i.SavedOrigin, listener: listener, cmd: cmd, dir: dir, output: output, grace: a.grace, waited: make(chan struct{}), closed: make(chan struct{}), done: make(chan error, 1)}
	owned = true
	go func() { _ = cmd.Wait(); output.discard(); close(s.waited); s.done <- ErrTransient }()
	go func() {
		select {
		case <-ctx.Done():
			s.beginClose()
		case <-s.waited:
			s.beginClose()
		case <-s.closed:
		}
	}()
	// Process start is not edge connection, route correctness or client acceptance.
	return s, nil
}
func cloudflareSnapshot(dir *appdata.RemoteProviderDirectory, name string, data []byte) (*os.File, error) {
	if err := dir.RemoveFile(name); err != nil {
		return nil, err
	}
	f, err := dir.OpenFile(name, true)
	if err != nil {
		return nil, err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return nil, ErrState
	}
	// Fresh independently opened readonly descriptor at offset zero. On Darwin
	// the config strict second decode sees EOF; only the fixed validated mapping
	// above is allowed. The token has a separate single-read descriptor.
	return dir.OpenFile(name, false)
}

type cloudflareOutput struct {
	mu    sync.Mutex
	bytes []byte
}

func (b *cloudflareOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), 1024-len(b.bytes))
	b.bytes = append(b.bytes, p[:n]...)
	return len(p), nil
}
func (b *cloudflareOutput) discard()                 { b.mu.Lock(); defer b.mu.Unlock(); clear(b.bytes); b.bytes = nil }
func (*cloudflareOutput) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[cloudflare output]")) }

type cloudflareSession struct {
	origin         string
	listener       net.Listener
	cmd            *exec.Cmd
	dir            *appdata.RemoteProviderDirectory
	output         *cloudflareOutput
	grace          time.Duration
	waited, closed chan struct{}
	done           chan error
	once           sync.Once
	closeErr       error
}

func (s *cloudflareSession) Origin() string           { return s.origin }
func (s *cloudflareSession) Listener() net.Listener   { return s.listener }
func (s *cloudflareSession) Done() <-chan error       { return s.done }
func (*cloudflareSession) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte("[cloudflare session]")) }
func (s *cloudflareSession) beginClose() {
	s.once.Do(func() {
		go func() {
			defer close(s.closed)
			_ = s.listener.Close()
			select {
			case <-s.waited:
			default:
				_ = s.cmd.Process.Signal(syscall.SIGTERM)
				timer := time.NewTimer(s.grace)
				select {
				case <-s.waited:
					if !timer.Stop() {
						<-timer.C
					}
				case <-timer.C:
					_ = s.cmd.Process.Kill()
					<-s.waited
				}
			}
			if s.dir.Close() != nil {
				s.closeErr = ErrState
			}
		}()
	})
}
func (s *cloudflareSession) Close(ctx context.Context) error {
	s.beginClose()
	select {
	case <-s.closed:
		return s.closeErr
	case <-ctx.Done():
		return ErrTransient
	}
}
