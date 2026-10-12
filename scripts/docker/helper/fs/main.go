// Developer-only synthetic-volume helper. Never included in the application image.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const root = "/var/lib/wa-indexer/demo"

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Println("synthetic helper failed")
		os.Exit(1)
	}
}
func run() error {
	switch os.Args[1] {
	case "attachment-download":
		if len(os.Args) != 3 {
			return fs.ErrInvalid
		}
		target, err := url.Parse(os.Args[2])
		if err != nil || target.Scheme != "https" || target.Host != "docker-synthetic.invalid" || !strings.HasPrefix(target.Path, "/mcp/attachments/") || target.RawQuery != "" || target.Fragment != "" {
			return fs.ErrInvalid
		}
		req, err := http.NewRequest("GET", "http://127.0.0.1:6968"+target.EscapedPath(), nil)
		if err != nil {
			return err
		}
		req.Host = target.Host
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": response.StatusCode, "sha256": hex.EncodeToString(sum[:]), "bytes": len(body)})

	case "lock":
		paths, e := appdata.ExistingPaths(root)
		if e != nil {
			return e
		}
		lease, e := appdata.Acquire(paths)
		if lease != nil {
			lease.Close()
		}
		if !errors.Is(e, appdata.ErrInstanceAlreadyRunning) {
			return fs.ErrInvalid
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"locked": true})
	case "probe":
		return probe()
	case "metrics":
		b, e := os.ReadFile("/proc/1/status")
		if e != nil {
			return e
		}
		var rss int64
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fmt.Sscanf(line, "VmRSS: %d kB", &rss)
			}
		}
		b, e = os.ReadFile("/proc/1/stat")
		if e != nil {
			return e
		}
		parts := strings.Fields(string(b))
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"rssKiB": rss, "cpuUserTicks": parts[13], "cpuSystemTicks": parts[14]})
	case "remove-wrong":
		if len(os.Args) != 3 || !strings.HasPrefix(os.Args[2], root+"/") || filepath.Base(os.Args[2]) != "task4-wrong.json" {
			return fs.ErrInvalid
		}
		return os.Remove(os.Args[2])
	case "wrong":
		if len(os.Args) != 3 || !strings.HasPrefix(os.Args[2], root+"/") {
			return fs.ErrInvalid
		}
		b, e := os.ReadFile(os.Args[2])
		if e != nil || len(b) > 4096 {
			return fs.ErrInvalid
		}
		var c map[string]any
		if json.Unmarshal(b, &c) != nil {
			return fs.ErrInvalid
		}
		c["token"] = strings.Repeat("A", 43)
		b, _ = json.Marshal(c)
		p := filepath.Join(filepath.Dir(os.Args[2]), "task4-wrong.json")
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(b)
		f.Close()
		if e == nil {
			fmt.Println(p)
		}
		return e
	case "sentinel":
		p := filepath.Join(root, "models")
		if e := os.MkdirAll(p, 0700); e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(p, "task4-inert-sentinel"), []byte("synthetic inert model persistence sentinel\n"), 0600)
	case "inspect":
		var count int
		var private = true
		parent, err := os.Stat("/var/lib/wa-indexer")
		if err != nil {
			return err
		}
		ps := parent.Sys().(*syscall.Stat_t)
		if parent.Mode().Perm() != 0700 || ps.Uid != 10001 || ps.Gid != 10001 {
			private = false
		}
		var sentinel = false
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			i, e := d.Info()
			if e != nil {
				return e
			}
			s := i.Sys().(*syscall.Stat_t)
			if s.Uid != 10001 || s.Gid != 10001 || i.Mode().Perm()&0077 != 0 {
				private = false
			}
			if d.Type()&os.ModeSymlink != 0 {
				private = false
			}
			count++
			if strings.HasSuffix(p, "task4-inert-sentinel") {
				b, e := os.ReadFile(p)
				sentinel = e == nil && string(b) == "synthetic inert model persistence sentinel\n"
			}
			return nil
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"private": private, "entries": count, "sentinel": sentinel})
	case "hash":
		h := sha256.New()
		e := filepath.WalkDir("/var/lib/wa-indexer", func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			i, e := os.Lstat(p)
			if e != nil {
				return e
			}
			s := i.Sys().(*syscall.Stat_t)
			fmt.Fprintf(h, "%s:%d:%d:%d:%d", p, i.Mode(), s.Uid, s.Gid, i.Size())
			if i.Mode().IsRegular() {
				b, e := os.ReadFile(p)
				if e != nil {
					return e
				}
				h.Write(b)
			}
			if i.Mode()&os.ModeSymlink != 0 {
				v, e := os.Readlink(p)
				if e != nil {
					return e
				}
				h.Write([]byte(v))
			}
			return nil
		})
		if e != nil {
			return e
		}
		fmt.Println(hex.EncodeToString(h.Sum(nil)))
		return nil
	case "bad-owner", "bad-nonempty", "bad-symlink":
		if e := os.MkdirAll(root, 0700); e != nil {
			return e
		}
		if os.Args[1] != "bad-owner" {
			if e := os.Chown(root, 10001, 10001); e != nil {
				return e
			}
		}
		switch os.Args[1] {
		case "bad-owner":
			return os.Chown(root, 12345, 12345)
		case "bad-nonempty":
			return os.WriteFile(root+"/foreign", []byte("synthetic foreign file"), 0600)
		case "bad-symlink":
			if e := os.Remove(root); e != nil {
				return e
			}
			return os.Symlink("/tmp", root)
		}
	}
	return fs.ErrInvalid
}
