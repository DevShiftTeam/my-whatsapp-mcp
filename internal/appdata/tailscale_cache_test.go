package appdata

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"tailscale.com/atomicfile"
	"tailscale.com/tka"
)

func cacheCertificate(t *testing.T, domain string) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func writeSDKCache(t *testing.T, path string) string {
	t.Helper()
	if err := os.Mkdir(filepath.Join(path, "certs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.WriteFile(filepath.Join(path, "certs", "archive.tail123.ts.net.crt"), cacheCertificate(t, "archive.tail123.ts.net"), 0644); err != nil {
		t.Fatal(err)
	}
	fs, err := tka.ChonkDir(filepath.Join(path, "tka-profiles", "abcd"))
	if err != nil {
		t.Fatal(err)
	}
	aum := tka.AUM{MessageKind: tka.AUMNoOp}
	if err := fs.CommitVerifiedAUMs([]tka.AUM{aum}); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetLastActiveAncestor(aum.Hash()); err != nil {
		t.Fatal(err)
	}
	return aum.Hash().String()
}
func TestTailscalePinnedSDKPublicCacheSurvivesInactiveAdmission(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "joined", true: "crash-shaped"}[crash], func(t *testing.T) {
			p := syntheticPaths(t)
			l, err := Acquire(p)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			path := filepath.Join(p.RemoteDir, "tailscale")
			if crash {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				writeSDKCache(t, path)
			} else {
				d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
				if err != nil {
					t.Fatal(err)
				}
				path, _ = d.Path()
				writeSDKCache(t, path)
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := WithRemoteAccess(p, l, func(*os.Root) error { return nil }); err != nil {
				t.Fatal(err)
			}
			d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			if err := ResetOwned(p, l); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTailscalePublicCacheRejectsUnsafeShapes(t *testing.T) {
	for _, kind := range []string{"wrong-domain", "private-key", "unknown-file", "unknown-directory", "outside-anchor", "cloudflare-cache", "direct-credential", "symlink", "hardlink", "acl", "bad-profile", "bad-shard", "bad-hash", "bad-cbor-type", "unknown-cbor-field", "unpurged-nil", "oversize-cert", "oversize-record", "too-many-certificates"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "acl" && runtime.GOOS != "darwin" {
				t.Skip("macOS ACL fixture")
			}
			p := syntheticPaths(t)
			l, err := Acquire(p)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
			if err != nil {
				t.Fatal(err)
			}
			path, _ := d.Path()
			hash := writeSDKCache(t, path)
			d.Close()
			cert := filepath.Join(path, "certs", "archive.tail123.ts.net.crt")
			record := filepath.Join(path, "tka-profiles", "abcd", hash[:2], hash)
			switch kind {
			case "wrong-domain":
				err = atomicfile.WriteFile(cert, cacheCertificate(t, "other.tail123.ts.net"), 0644)
			case "private-key":
				err = atomicfile.WriteFile(cert, []byte("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"), 0644)
			case "unknown-file":
				err = atomicfile.WriteFile(filepath.Join(path, "secret.crt"), cacheCertificate(t, "archive.tail123.ts.net"), 0644)
			case "unknown-directory":
				err = os.Mkdir(filepath.Join(path, "public"), 0755)
			case "outside-anchor":
				err = os.Mkdir(filepath.Join(p.RemoteDir, "cloudflare"), 0755)
			case "cloudflare-cache":
				other := filepath.Join(p.RemoteDir, "cloudflare", "certs")
				if err := os.MkdirAll(other, 0700); err != nil {
					t.Fatal(err)
				}
				err = atomicfile.WriteFile(filepath.Join(other, "archive.tail123.ts.net.crt"), cacheCertificate(t, "archive.tail123.ts.net"), 0644)
			case "direct-credential":
				err = atomicfile.WriteFile(filepath.Join(p.RemoteDir, "credential.json"), []byte("secret"), 0644)
			case "symlink":
				os.Remove(cert)
				err = os.Symlink(record, cert)
			case "hardlink":
				err = os.Link(cert, filepath.Join(path, "certs", "another.crt"))
			case "acl":
				err = exec.Command("/bin/chmod", "+a", "everyone allow read", cert).Run()
			case "bad-profile":
				err = os.Rename(filepath.Join(path, "tka-profiles", "abcd"), filepath.Join(path, "tka-profiles", "ABCD"))
			case "bad-shard":
				err = os.Rename(filepath.Dir(record), filepath.Join(path, "tka-profiles", "abcd", "00"))
			case "bad-hash":
				err = os.Rename(record, record[:len(record)-1]+"B")
			case "bad-cbor-type":
				err = atomicfile.WriteFile(record, []byte{0x61, 0x78}, 0644)
			case "unknown-cbor-field":
				data, _ := cbor.Marshal(map[int]any{2: nil, 4: int64(1), 9: "secret"})
				err = atomicfile.WriteFile(record, data, 0644)
			case "unpurged-nil":
				data, _ := cbor.Marshal(map[int]any{2: nil})
				err = atomicfile.WriteFile(record, data, 0644)
			case "oversize-cert":
				err = atomicfile.WriteFile(cert, make([]byte, 256*1024+1), 0644)
			case "oversize-record":
				err = atomicfile.WriteFile(record, make([]byte, 1024*1024+1), 0644)
			case "too-many-certificates":
				data := cacheCertificate(t, "archive.tail123.ts.net")
				var chain []byte
				for n := 0; n < 17; n++ {
					chain = append(chain, data...)
				}
				err = atomicfile.WriteFile(cert, chain, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if WithRemoteAccess(p, l, func(*os.Root) error { return nil }) == nil {
				t.Fatal("unsafe public cache accepted")
			}
			if _, err := Prepare(p.Root); err != nil {
				t.Fatal("optional cache blocked local archive", err)
			}
		})
	}
}
func TestTailscalePurgedSDKRecords(t *testing.T) {
	p := syntheticPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := d.Path()
	hash := writeSDKCache(t, path)
	fs, err := tka.ChonkDir(filepath.Join(path, "tka-profiles", "abcd"))
	if err != nil {
		t.Fatal(err)
	}
	var h tka.AUMHash
	h.UnmarshalText([]byte(hash))
	if err := fs.PurgeAUMs([]tka.AUMHash{h}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if err := WithRemoteAccess(p, l, func(*os.Root) error { return nil }); err != nil {
		t.Fatal(err)
	}
	enc, _ := cbor.CTAP2EncOptions().EncMode()
	data, _ := enc.Marshal(map[int]any{2: nil, 4: int64(1)})
	if err := atomicfile.WriteFile(filepath.Join(path, "tka-profiles", "abcd", hash[:2], hash), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(p, l, func(*os.Root) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestTailscalePublicCacheBudgetBoundaries(t *testing.T) {
	p := syntheticPaths(t)
	l, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	d, err := OpenRemoteProviderDirectory(p, l, TailscaleProvider)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := d.Path()
	if err := os.Mkdir(filepath.Join(path, "certs"), 0700); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for n := 0; n < 33; n++ {
		domain := fmt.Sprintf("archive%d.tail123.ts.net", n)
		cert := cacheCertificate(t, domain)
		cert = append(cert, bytes.Repeat([]byte(" "), tailscaleCertificateBytes-len(cert))...)
		if err := atomicfile.WriteFile(filepath.Join(path, "certs", domain+".crt"), cert, 0644); err != nil {
			t.Fatal(err)
		}
		if n == 31 {
			if err := WithRemoteAccess(p, l, func(*os.Root) error { return nil }); err != nil {
				t.Fatal("exact 8MiB public-cache budget", err)
			}
		}
	}
	if WithRemoteAccess(p, l, func(*os.Root) error { return nil }) == nil {
		t.Fatal("aggregate budget exceeded")
	}
}
func TestTailscaleCertificateBlockBoundary(t *testing.T) {
	data := cacheCertificate(t, "archive.tail123.ts.net")
	if !tailscaleCertificateChain(bytes.Repeat(data, 16), "archive.tail123.ts.net") {
		t.Fatal("16-block public chain refused")
	}
	if tailscaleCertificateChain(bytes.Repeat(data, 17), "archive.tail123.ts.net") {
		t.Fatal("17-block chain accepted")
	}
}
func TestTailscaleHashRecordStrictSchema(t *testing.T) {
	aum := tka.AUM{MessageKind: tka.AUMNoOp}
	hash := aum.Hash()
	encode := func(v any) []byte {
		data, err := tailscaleCacheEncoder.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	good := encode(tailscaleFSHashInfo{AUM: &aum, CreatedUnix: 1})
	if !tailscaleHashRecord(good, hash) {
		t.Fatal("valid record")
	}
	for name, data := range map[string][]byte{
		"legacy-children":      encode(map[int]any{1: []any{}, 2: nil, 4: 1}),
		"unknown-field":        encode(map[int]any{2: nil, 4: 1, 9: "secret"}),
		"unknown-nested-field": encode(map[int]any{2: map[int]any{1: 3, 2: nil, 99: "secret"}}),
		"negative-timestamp":   encode(tailscaleFSHashInfo{AUM: &aum, CreatedUnix: -1}),
		"null-timestamp":       encode(map[int]any{2: nil, 3: nil, 4: 1}),
		"missing-aum-field":    encode(map[int]any{4: 1}),
		"nil-unpurged":         encode(map[int]any{2: nil}),
		"wrong-aum-type":       encode(map[int]any{2: "private key"}),
		"unknown-kind":         encode(tailscaleFSHashInfo{AUM: &tka.AUM{MessageKind: 255}}),
		"private-key-bytes":    encode(map[int]any{2: []byte("-----BEGIN PRIVATE KEY-----")}),
		"duplicate-key":        []byte{0xa3, 2, 0xf6, 4, 1, 4, 1},
		"indefinite-map":       []byte{0xbf, 2, 0xf6, 4, 1, 0xff},
		"tag":                  append([]byte{0xc0}, good...),
		"trailing-data":        append(append([]byte{}, good...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if tailscaleHashRecord(data, hash) {
				t.Fatal("unsafe CBOR accepted")
			}
		})
	}
	hash[0] ^= 1
	if tailscaleHashRecord(good, hash) {
		t.Fatal("content/filename hash mismatch")
	}
}

func TestTailscaleRecordAndDecoderBoundaries(t *testing.T) {
	// A pinned typed UpdateKey has public metadata. Its full record is capped,
	// even though upstream permits large metadata in this message kind.
	aum := tka.AUM{MessageKind: tka.AUMUpdateKey, KeyID: make([]byte, 32), Meta: map[string]string{"label": strings.Repeat("x", tailscaleRecordBytes)}}
	record := tailscaleFSHashInfo{AUM: &aum}
	data, err := tailscaleCacheEncoder.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	aum.Meta["label"] = aum.Meta["label"][:tailscaleRecordBytes-(len(data)-tailscaleRecordBytes)]
	data, err = tailscaleCacheEncoder.Marshal(record)
	if err != nil || len(data) != tailscaleRecordBytes {
		t.Fatal("record fixture size", len(data), err)
	}
	if !tailscaleHashRecord(data, aum.Hash()) {
		t.Fatal("exact 1MiB record refused")
	}
	if tailscaleHashRecord(append(data, 0), aum.Hash()) {
		t.Fatal("oversize record admitted")
	}
	aum.Meta = make(map[string]string)
	for n := 0; n < 1024; n++ {
		aum.Meta[fmt.Sprint(n)] = "x"
	}
	data, _ = tailscaleCacheEncoder.Marshal(record)
	if !tailscaleHashRecord(data, aum.Hash()) {
		t.Fatal("1024 map pairs refused")
	}
	aum.Meta["overflow"] = "x"
	data, _ = tailscaleCacheEncoder.Marshal(record)
	if tailscaleHashRecord(data, aum.Hash()) {
		t.Fatal("1025 map pairs admitted")
	}
}
func TestTailscaleCertificateFilenameCanonicalDomain(t *testing.T) {
	for _, domain := range []string{"127.0.0.1", "127.1", "0177.1", "example.0x7f", "*.ts.net", "UPPER.ts.net", "node.ts.net.", "node.local", "single", "node..ts.net"} {
		if tailscaleCertificateDomain(domain) {
			t.Fatal(domain)
		}
	}
}

func TestTailscaleCertificateAdmissionAccountsForEveryBlock(t *testing.T) {
	const domain = "archive.tail123.ts.net"
	valid := cacheCertificate(t, domain)
	malformed := []byte("-----BEGIN CERTIFICATE-----\nSYNTHETIC-PRIVATE-CONTENT:discarded!\n-----END CERTIFICATE-----\n")
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	cases := []struct {
		name    string
		data    []byte
		allowed bool
	}{
		{"single-valid", valid, true},
		{"multiple-with-whitespace", join([]byte(" \r\n\t"), valid, []byte("\r\n \t\n"), valid, []byte(" \t\r\n")), true},
		{"crlf-valid", bytes.ReplaceAll(valid, []byte("\n"), []byte("\r\n")), true},
		{"malformed-first-then-valid", join(malformed, valid), false},
		{"missing-end-before-next-begin", join([]byte("-----BEGIN CERTIFICATE-----\ninvalid-prefix\n"), valid), false},
		{"nested-begin", join([]byte("-----BEGIN CERTIFICATE-----\n"), valid, []byte("-----END CERTIFICATE-----\n")), false},
		{"malformed-middle", join(valid, malformed, valid), false},
		{"wrong-end-label", join(bytes.Replace(malformed, []byte("-----END CERTIFICATE-----"), []byte("-----END PRIVATE KEY-----"), 1), valid), false},
		{"arbitrary-body-bytes", join([]byte("-----BEGIN CERTIFICATE-----\nprivate-token = !discard-me!\n-----END CERTIFICATE-----\n"), valid), false},
		{"arbitrary-between-blocks", join(valid, []byte("arbitrary noncertificate bytes\n"), valid), false},
		{"header", bytes.Replace(valid, []byte("\n"), []byte("\nProc-Type: 4,ENCRYPTED\n"), 1), false},
		{"private-key-between-blocks", join(valid, []byte("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n"), valid), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := syntheticPaths(t)
			l, err := Acquire(p)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			certs := filepath.Join(p.RemoteDir, "tailscale", "certs")
			if err := os.MkdirAll(certs, 0700); err != nil {
				t.Fatal(err)
			}
			if err := atomicfile.WriteFile(filepath.Join(certs, domain+".crt"), tc.data, 0644); err != nil {
				t.Fatal(err)
			}
			err = WithRemoteAccess(p, l, func(*os.Root) error { return nil })
			if (err == nil) != tc.allowed {
				t.Fatalf("inactive admission=%v, allowed=%v", err, tc.allowed)
			}
		})
	}
}
