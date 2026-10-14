package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var errAdminConfig = errors.New("admin_auth_unavailable")

const adminPasswordHashEnv = "WA_INDEXER_ADMIN_PASSWORD_HASH"
const maxAdminEnvBytes = 4096

// The tunnel-facing route is trusted launch configuration. No forwarded header,
// DNS lookup, or HTTP argument may choose its listener or public authority.
func validAdminRoute(opts options) bool {
	address, err := netip.ParseAddrPort(opts.adminListen)
	if err != nil || address.Addr() != netip.MustParseAddr("127.0.0.1") || address.Port() == 0 || address.String() != opts.adminListen {
		return false
	}
	canonical, err := clientsetup.NormalizeRemoteURL(opts.adminPublicURL + "/mcp")
	if err != nil || canonical != opts.adminPublicURL+"/mcp" {
		return false
	}
	if opts.adminEnvFile != "" && (!filepath.IsAbs(opts.adminEnvFile) || filepath.Clean(opts.adminEnvFile) != opts.adminEnvFile) {
		return false
	}
	// Reject shared ports even across different loopback addresses; each service
	// has one unambiguous owner and one set of authentication rules.
	for _, other := range []string{opts.listen, opts.mcpListen} {
		_, port, err := net.SplitHostPort(other)
		if err == nil && port != "" {
			number, err := strconv.ParseUint(port, 10, 16)
			if err == nil && number == uint64(address.Port()) {
				return false
			}
		}
	}
	return opts.mcpPublicURL == "" || opts.mcpPublicURL == opts.adminPublicURL+"/mcp"
}

// Validate before any listener, profile, WhatsApp session, or worker is opened.
// A supplied env file is parsed as data and is never sourced as shell code.
func remoteAdminLaunchConfig(opts options) (*localui.RemoteConfig, error) {
	if opts.adminListen == "" && opts.adminPublicURL == "" && opts.adminEnvFile == "" {
		return nil, nil
	}
	if !validAdminRoute(opts) {
		return nil, errAdminConfig
	}
	hash := os.Getenv(adminPasswordHashEnv)
	if opts.adminEnvFile != "" {
		fromFile, err := readAdminEnvFile(opts.adminEnvFile)
		if err != nil || (hash != "" && hash != fromFile) {
			return nil, errAdminConfig
		}
		hash = fromFile
	}
	cfg := localui.RemoteConfig{Origin: opts.adminPublicURL, ListenAddress: opts.adminListen, PasswordHash: hash}
	if localui.ValidateRemoteConfig(cfg) != nil {
		return nil, errAdminConfig
	}
	return &cfg, nil
}

func privateAdminEnvEntry(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm() != 0700 {
			return false
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 1 || info.Size() > maxAdminEnvBytes {
		return false
	}
	if os.Geteuid() < 0 {
		return false
	}
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() != reflect.Pointer || stat.IsNil() {
		return false
	}
	stat = stat.Elem()
	if stat.Kind() != reflect.Struct {
		return false
	}
	uid := stat.FieldByName("Uid")
	if !uid.IsValid() || !uid.CanUint() || uid.Uint() != uint64(os.Geteuid()) {
		return false
	}
	if !directory {
		links := stat.FieldByName("Nlink")
		if !links.IsValid() || !links.CanUint() || links.Uint() != 1 {
			return false
		}
	}
	return true
}

func readPrivateEnvFile(path string) (data []byte, result error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errAdminConfig
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	canonical, err := filepath.EvalSymlinks(parentPath)
	if err != nil || canonical != parentPath {
		return nil, errAdminConfig
	}
	beforeParent, err := os.Lstat(parentPath)
	if err != nil || !privateAdminEnvEntry(beforeParent, true) {
		return nil, errAdminConfig
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errAdminConfig
	}
	defer func() {
		if root.Close() != nil {
			data, result = nil, errAdminConfig
		}
	}()
	parent, err := root.Open(".")
	if err != nil {
		return nil, errAdminConfig
	}
	heldParent, statErr := parent.Stat()
	safeParent := statErr == nil && privateAdminEnvEntry(heldParent, true) && os.SameFile(beforeParent, heldParent) && adminEnvNoACL(parent)
	closeErr := parent.Close()
	if !safeParent || closeErr != nil {
		return nil, errAdminConfig
	}
	before, err := root.Lstat(name)
	if err != nil || !privateAdminEnvEntry(before, false) {
		return nil, errAdminConfig
	}
	// Nonblocking/no-follow prevents a FIFO or symlink substituted after Lstat
	// from blocking startup or opening an unrelated file.
	file, err := root.OpenFile(name, adminEnvOpenFlags(), 0)
	if err != nil {
		return nil, errAdminConfig
	}
	defer func() {
		if file.Close() != nil {
			data, result = nil, errAdminConfig
		}
	}()
	opened, err := file.Stat()
	if err != nil || !privateAdminEnvEntry(opened, false) || !os.SameFile(before, opened) || !adminEnvNoACL(file) {
		return nil, errAdminConfig
	}
	data, err = io.ReadAll(io.LimitReader(file, maxAdminEnvBytes+1))
	if err != nil || len(data) > maxAdminEnvBytes || strings.ContainsRune(string(data), '\x00') {
		return nil, errAdminConfig
	}
	after, err := root.Lstat(name)
	afterOpened, openedErr := file.Stat()
	if err != nil || openedErr != nil || !privateAdminEnvEntry(after, false) || !privateAdminEnvEntry(afterOpened, false) || !os.SameFile(opened, after) || !os.SameFile(opened, afterOpened) || opened.Size() != afterOpened.Size() || !opened.ModTime().Equal(afterOpened.ModTime()) {
		return nil, errAdminConfig
	}
	return data, nil
}

func readAdminEnvFile(path string) (string, error) {
	data, err := readPrivateEnvFile(path)
	if err != nil {
		return "", errAdminConfig
	}
	var hash string
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != adminPasswordHashEnv || seen || value == "" || strings.TrimSpace(value) != value {
			return "", errAdminConfig
		}
		hash, seen = value, true
	}
	if !seen {
		return "", errAdminConfig
	}
	return hash, nil
}

type ownerInterface interface {
	Serve(context.Context, net.Listener) error
}
type ownerInterfaceBinding struct {
	server   ownerInterface
	listener net.Listener
}

// All interfaces have already bound and validated before publication. The
// first exit cancels the others, and their independent shutdowns join before
// composition may close the archive or release the instance lease.
func serveOwnerInterfaces(ctx context.Context, bindings []ownerInterfaceBinding) error {
	budget := newServeShutdown(serveShutdownDuration(false))
	defer budget.close()
	return serveOwnerInterfacesWithShutdown(ctx, bindings, budget.begin)
}

func serveOwnerInterfacesWithShutdown(ctx context.Context, bindings []ownerInterfaceBinding, shutdown func() context.Context) error {
	serving, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(bindings))
	for _, binding := range bindings {
		go func(binding ownerInterfaceBinding) {
			if server, ok := binding.server.(interface {
				ServeWithShutdown(context.Context, net.Listener, func() context.Context) error
			}); ok {
				done <- server.ServeWithShutdown(serving, binding.listener, shutdown)
			} else {
				done <- binding.server.Serve(serving, binding.listener)
			}
		}(binding)
	}
	var result error
	for range bindings {
		err := <-done
		shutdown()
		cancel()
		if errors.Is(err, localui.ErrShutdownIncomplete) || (result == nil && err != nil) {
			result = err
		}
	}
	return result
}
