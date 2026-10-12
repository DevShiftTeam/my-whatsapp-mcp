package remoteaccess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type cloudflarePin struct {
	size   int64
	hash   string
	header []byte
}

func cloudflarePlatformPin() (cloudflarePin, bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return cloudflarePin{40129756, "d33ff2d14475178d2012c2c56beba87389ac5ded27649519f198a7d3134a99db", []byte{0x7f, 69, 76, 70, 2, 1}}, true
	case "linux/arm64":
		return cloudflarePin{37687584, "e6422b9d4f72d3194bc5a38676f13667c06666523217b842a877d72a80b5ac08", []byte{0x7f, 69, 76, 70, 2, 1}}, true
	case "darwin/arm64":
		return cloudflarePin{39364384, "72edfd3eea463aef4d5cb89e2e209cecb048cc756c2b01915de2e0ad7cb39830", []byte{0xcf, 0xfa, 0xed, 0xfe}}, true
	default:
		return cloudflarePin{}, false
	}
}

// This is a pathname execution boundary, not descriptor-pinned exec. Qualified
// owned/root ancestors, no symlinks and no group/other writers prevent untrusted
// replacement; the trusted current owner can still modify its own installation.
func cloudflarePath(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrState
	}
	var leaf os.FileInfo
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return nil, ErrState
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 || (sys.Uid != 0 && sys.Uid != uint32(os.Geteuid())) {
			return nil, ErrState
		}

		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, ErrState
		}
		opened, statErr := f.Stat()
		aclSafe := cloudflareSafeACL(f)
		f.Close()
		if statErr != nil || !os.SameFile(st, opened) || !aclSafe {
			return nil, ErrState
		}
		if p == path {
			if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
				return nil, ErrState
			}
			leaf = st
		} else if !st.IsDir() {
			return nil, ErrState
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return leaf, nil
}
func verifyCloudflare(ctx context.Context, path string) error {
	pin, ok := cloudflarePlatformPin()
	if !ok {
		return ErrState
	}
	before, err := cloudflarePath(path)
	if err != nil || before.Size() != pin.size {
		return ErrState
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrState
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return ErrState
	}
	header := make([]byte, 32)
	if _, err := io.ReadFull(f, header); err != nil || !bytes.HasPrefix(header, pin.header) {
		return ErrState
	}
	if runtime.GOOS == "linux" {
		machine := uint16(183)
		if runtime.GOARCH == "amd64" {
			machine = 62
		}
		if binary.LittleEndian.Uint16(header[18:20]) != machine {
			return ErrState
		}
	} else if binary.LittleEndian.Uint32(header[4:8]) != 0x100000c {
		return ErrState
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ErrState
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(f, pin.size+1)); err != nil || n != pin.size || hex.EncodeToString(hash.Sum(nil)) != pin.hash {
		return ErrState
	}
	// Exact platform-specific digest already identifies architecture and version.
	// Probe only authenticated bytes, with no ambient helper/provider overrides.
	after, err := cloudflarePath(path)
	if err != nil || !os.SameFile(opened, after) {
		return ErrState
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, "--version")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	output := &cloudflareOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err = cmd.Run()
	defer output.discard()
	if ctx.Err() != nil {
		return ErrTransient
	}
	if err != nil || !strings.HasPrefix(string(output.bytes), "cloudflared version "+CloudflareVersion+" ") {
		return ErrState
	}
	return nil
}
