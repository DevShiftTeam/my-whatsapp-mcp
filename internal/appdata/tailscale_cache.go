package appdata

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"tailscale.com/tka"
)

// Exact v1.104.1 SDK public-cache exceptions, under the fixed private Tailscale
// anchor only. No chmod or shutdown-only reconciliation: interrupted-process
// state is admitted with the same schema. All ordinary private rules stay intact.
const (
	tailscaleCertificateBytes = 256 << 10
	tailscaleRecordBytes      = 1 << 20
	tailscaleCacheWalkBytes   = 8 << 20
)

func tailscalePublicCache(root *os.Root, name string, info os.FileInfo, remaining *int64) bool {
	const prefix = "remote/tailscale/"
	if !strings.HasPrefix(name, prefix) || !ownedByCurrentUser(info) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(name, prefix), "/")
	if info.Mode() == os.ModeDir|0755 {
		return parts[0] == "tka-profiles" && (len(parts) == 1 || len(parts) == 2 && tailscaleProfile(parts[1]) || len(parts) == 3 && tailscaleProfile(parts[1]) && tailscaleShard(parts[2]))
	}
	if info.Mode() != 0644 || !singleLink(info) {
		return false
	}
	var check func([]byte) bool
	limit := int64(tailscaleRecordBytes)
	switch {
	case len(parts) == 2 && parts[0] == "certs" && strings.HasSuffix(parts[1], ".crt"):
		domain := strings.TrimSuffix(parts[1], ".crt")
		if !tailscaleCertificateDomain(domain) {
			return false
		}
		limit = tailscaleCertificateBytes
		check = func(data []byte) bool { return tailscaleCertificateChain(data, domain) }
	case len(parts) == 3 && parts[0] == "tka-profiles" && tailscaleProfile(parts[1]) && parts[2] == "last_active_ancestor":
		limit = 32
		check = func(data []byte) bool { return len(data) == 32 }
	case len(parts) == 4 && parts[0] == "tka-profiles" && tailscaleProfile(parts[1]) && tailscaleShard(parts[2]):
		var hash tka.AUMHash
		if len(parts[3]) != 52 || hash.UnmarshalText([]byte(parts[3])) != nil || hash.String() != parts[3] || parts[2] != parts[3][:2] {
			return false
		}
		check = func(data []byte) bool { return tailscaleHashRecord(data, hash) }
	default:
		return false
	}
	data, err := readTailscaleCache(root, name, info, limit, remaining)
	return err == nil && check(data)
}
func tailscaleProfile(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func tailscaleShard(s string) bool {
	if len(s) != 2 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
func tailscaleCertificateDomain(s string) bool {
	if len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	if _, err := strconv.ParseUint(last, 0, 64); err == nil {
		return false
	}
	digits := true
	for _, c := range last {
		digits = digits && c >= '0' && c <= '9'
	}
	if digits {
		return false
	}
	switch last {
	case "localhost", "local", "internal", "invalid", "test", "onion":
		return false
	}
	return true
}
func tailscaleCertificateChain(data []byte, domain string) bool {
	if len(data) > tailscaleCertificateBytes {
		return false
	}
	count := 0
	for len(bytes.TrimSpace(data)) > 0 {
		data = bytes.TrimSpace(data)
		const begin = "-----BEGIN CERTIFICATE-----"
		const end = "-----END CERTIFICATE-----"
		if !bytes.HasPrefix(data, []byte(begin)) {
			return false
		}
		// Decode searches past malformed candidates. Isolate the first block
		// and forbid nested delimiters so it cannot skip any input to find a
		// later certificate, including when the first candidate has no END.
		endOffset := bytes.Index(data[len(begin):], []byte(end))
		if endOffset < 0 {
			return false
		}
		body := data[len(begin) : len(begin)+endOffset]
		if !(bytes.HasPrefix(body, []byte("\n")) || bytes.HasPrefix(body, []byte("\r\n"))) ||
			len(body) == 0 || body[len(body)-1] != '\n' ||
			bytes.Contains(body, []byte("-----BEGIN")) || bytes.Contains(body, []byte("-----END")) {
			return false
		}
		blockEnd := len(begin) + endOffset + len(end)
		block, unconsumed := pem.Decode(data[:blockEnd])
		if block == nil || len(unconsumed) != 0 || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		rest := data[blockEnd:]
		count++
		if count > 16 {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		if count == 1 {
			exact := false
			for _, dns := range cert.DNSNames {
				if dns == domain {
					exact = true
				}
			}
			if !exact || cert.VerifyHostname(domain) != nil {
				return false
			}
		}
		data = rest
	}
	return count > 0
}
func readTailscaleCache(root *os.Root, name string, before os.FileInfo, limit int64, remaining *int64) ([]byte, error) {
	if before.Size() < 1 || before.Size() > limit || before.Size() > *remaining {
		return nil, ErrUnsafeDataPath
	}
	*remaining -= before.Size()
	f, err := root.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !samePublicCacheFile(before, opened) || !noExtendedACL(f) {
		return nil, ErrUnsafeDataPath
	}
	data, err := io.ReadAll(io.LimitReader(f, before.Size()+1))
	if err != nil || int64(len(data)) != before.Size() {
		return nil, ErrUnsafeDataPath
	}
	after, err := f.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || !samePublicCacheFile(before, after) || !samePublicCacheFile(before, current) || !noExtendedACL(f) {
		return nil, ErrUnsafeDataPath
	}
	return data, nil
}
func samePublicCacheFile(a, b os.FileInfo) bool {
	return b != nil && b.Mode() == 0644 && ownedByCurrentUser(b) && singleLink(b) && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// This mirrors pinned tka.fsHashInfo. Legacy Children key1 and unknown/new SDK
// fields are deliberately unsupported for this first app-owned state format.
// Purged records may lack AUM; positive PurgedUnix is then required. If present,
// even a purged AUM must be well-formed and match its canonical hash filename.
type tailscaleFSHashInfo struct {
	AUM         *tka.AUM `cbor:"2,keyasint"`
	CreatedUnix int64    `cbor:"3,keyasint,omitempty"`
	PurgedUnix  int64    `cbor:"4,keyasint,omitempty"`
}

var tailscaleCacheDecoder = func() cbor.DecMode {
	mode, err := (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 16, MaxArrayElements: 4096, MaxMapPairs: 1024, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}).DecMode()
	if err != nil {
		panic("invalid pinned Tailscale cache decoder")
	}
	return mode
}()
var tailscaleCacheEncoder = func() cbor.EncMode {
	mode, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		panic("invalid pinned Tailscale cache encoder")
	}
	return mode
}()

func tailscaleHashRecord(data []byte, hash tka.AUMHash) bool {
	if len(data) > tailscaleRecordBytes {
		return false
	}
	var record tailscaleFSHashInfo
	if tailscaleCacheDecoder.Unmarshal(data, &record) != nil || record.CreatedUnix < 0 || record.PurgedUnix < 0 {
		return false
	}
	// The SDK writes canonical CTAP2. Exact re-encoding also rejects absent
	// required fields, null numeric values, unknown aliases and implicit coercion.
	canonical, err := tailscaleCacheEncoder.Marshal(record)
	if err != nil || !bytes.Equal(data, canonical) {
		return false
	}
	if record.AUM == nil {
		return record.PurgedUnix > 0
	}
	a := record.AUM
	if a.MessageKind < tka.AUMAddKey || a.MessageKind > tka.AUMCheckpoint || a.StaticValidate() != nil || len(a.PrevAUMHash) != 0 && len(a.PrevAUMHash) != 32 || len(a.KeyID) != 0 && len(a.KeyID) != 32 {
		return false
	}
	if a.Key != nil && len(a.Key.Public) != 32 {
		return false
	}
	if a.State != nil {
		for _, key := range a.State.Keys {
			if len(key.Public) != 32 {
				return false
			}
		}
	}
	if a.MessageKind == tka.AUMNoOp && (a.Key != nil || a.KeyID != nil || a.State != nil || a.Votes != nil || a.Meta != nil) {
		return false
	}
	return a.Hash() == hash
}
