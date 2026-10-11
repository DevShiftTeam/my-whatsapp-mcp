// Package modelstore installs only catalog-pinned assets. Verify is inspection;
// native consumers must hold an Acquire lease until every worker has exited.
package modelstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"runtime"
	"strings"
)

var (
	ErrInvalidCatalog     = errors.New("model catalog invalid")
	ErrUnknownProfile     = errors.New("model profile unknown")
	ErrPlatform           = errors.New("model profile incompatible with platform")
	ErrNotInstalled       = errors.New("model profile not installed")
	ErrHashMismatch       = errors.New("model asset hash mismatch")
	ErrSizeMismatch       = errors.New("model asset size mismatch")
	ErrUnsafeFile         = errors.New("model file unsafe")
	ErrURLPolicy          = errors.New("model URL policy rejected")
	ErrActiveLease        = errors.New("model profile has active leases")
	ErrLeaseClosed        = errors.New("model lease closed")
	ErrClosed             = errors.New("model store closed")
	ErrOfflineOnly        = errors.New("model asset requires offline import")
	ErrRange              = errors.New("model resume response invalid")
	ErrFilesystemMismatch = errors.New("model filesystem mismatch")
)

type Catalog struct {
	SchemaVersion int       `json:"schema_version"`
	AllowedHosts  []string  `json:"allowed_hosts"`
	Profiles      []Profile `json:"profiles"`
}

type Profile struct {
	ID                  string      `json:"id"`
	Revision            string      `json:"revision"`
	Manifest            string      `json:"manifest"`
	ManifestSHA256      string      `json:"manifest_sha256"`
	Selection           string      `json:"selection"`
	GateStatus          string      `json:"gate_status"`
	Platform            string      `json:"platform"`
	License             string      `json:"license"`
	Notices             string      `json:"notices"`
	RuntimeRequirements []string    `json:"runtime_requirements"`
	Components          []Component `json:"components"`
}

type Component struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	License  string `json:"license"`
}

type InstalledComponent struct {
	// Path is diagnostic only. Open under an active Lease supplies the verified
	// descriptor; reopening this path is vulnerable to local path replacement.
	ID, Role, SHA256, Path string
	Bytes                  int64
}

type InstalledProfile struct {
	ID, Identity, Revision, Platform, Selection, GateStatus string
	RuntimeRequirements                                     []string
	Components                                              []InstalledComponent
}

type Progress struct {
	ProfileID, ComponentID, Phase                                 string
	CompletedBytes, DownloadBytes, InstalledBytes, TemporaryBytes int64
}

type SpacePlan struct {
	DownloadBytes, InstalledBytes, TemporaryBytes, ExistingBytes, ExistingAllocatedBytes, ReusableBytes, GrowthBytes, FreeBytes, HeadroomBytes, RequiredBytes, AdditionalBytes int64
}

type SpaceError struct {
	Plan            SpacePlan
	AdditionalBytes int64
}

func (e *SpaceError) Error() string {
	return fmt.Sprintf("model installation requires %d additional bytes", e.AdditionalBytes)
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func safeName(s string) bool { return safeID.MatchString(s) && s != "." && s != ".." }

func ParseCatalog(data []byte) (Catalog, error) {
	var c Catalog
	if len(data) > 1<<20 {
		return c, ErrInvalidCatalog
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, ErrInvalidCatalog
	}
	return c, c.validate()
}

func (c Catalog) validate() error {
	if c.SchemaVersion != 1 || len(c.Profiles) == 0 || len(c.Profiles) > 64 || len(c.AllowedHosts) > 16 {
		return ErrInvalidCatalog
	}
	hosts := make(map[string]bool)
	for _, host := range c.AllowedHosts {
		if host == "" || strings.ToLower(host) != host || strings.ContainsAny(host, "/:@* \\?#") || hosts[host] {
			return ErrInvalidCatalog
		}
		hosts[host] = true
	}
	ids := make(map[string]bool)
	for _, p := range c.Profiles {
		if !safeName(p.ID) || len(p.ID) > 100 || ids[p.ID] || p.Revision == "" || len(p.Revision) > 128 || p.License == "" || p.Notices == "" || len(p.Components) == 0 || len(p.Components) > 16 {
			return ErrInvalidCatalog
		}
		ids[p.ID] = true
		if p.Selection != "candidate" && p.Selection != "failed" && p.Selection != "accepted" {
			return ErrInvalidCatalog
		}
		if p.Platform != "any" && p.Platform != "darwin-arm64" && p.Platform != "darwin-amd64" && p.Platform != "linux-arm64" && p.Platform != "linux-amd64" {
			return ErrInvalidCatalog
		}
		if p.ManifestSHA256 != "" && !shaPattern.MatchString(p.ManifestSHA256) {
			return ErrInvalidCatalog
		}
		components, names, reserved := make(map[string]bool), make(map[string]bool), make(map[string]bool)
		var total int64
		reserved["profile.json"], reserved["profile.json.tmp"] = true, true
		for _, a := range p.Components {
			reserved[a.ID+".part"], reserved[a.ID+".meta"], reserved[a.ID+".meta.tmp"] = true, true, true
		}
		for _, a := range p.Components {
			if !safeName(a.ID) || len(a.ID) > 100 || !safeName(a.Filename) || reserved[a.Filename] || components[a.ID] || names[a.Filename] || a.Role == "" || a.License == "" || !shaPattern.MatchString(a.SHA256) || a.Bytes <= 0 || a.Bytes > 8<<30 {
				return ErrInvalidCatalog
			}
			components[a.ID], names[a.Filename] = true, true
			total += a.Bytes
			if a.URL != "" {
				if err := c.checkURL(a.URL); err != nil {
					return fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
				}
			}
		}
		if total > 16<<30 {
			return ErrInvalidCatalog
		}
	}
	return nil
}

func (c Catalog) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Host == "" || u.Opaque != "" {
		return ErrURLPolicy
	}
	host := u.Hostname()
	// The provider documents these exact CDN edges on HTTPS443 only:
	// https://huggingface.co/docs/hub/en/models-downloading
	if (host == "us.aws.cdn.hf.co" || host == "us.gcp.cdn.hf.co") && u.Port() != "" && u.Port() != "443" {
		return ErrURLPolicy
	}
	for _, h := range c.AllowedHosts {
		if host == h {
			return nil
		}
	}
	return ErrURLPolicy
}

func (p Profile) identity() string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p Profile) totalBytes() int64 {
	var n int64
	for _, a := range p.Components {
		n += a.Bytes
	}
	return n
}
func (p Profile) compatible() bool {
	return p.Platform == "any" || p.Platform == runtime.GOOS+"-"+runtime.GOARCH
}
