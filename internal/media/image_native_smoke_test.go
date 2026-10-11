//go:build media_image_native && sqlite_fts5 && darwin && arm64

package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
	"golang.org/x/sys/unix"
)

const imageSmokeFixtureSHA = "826f25d1e465b7ca493d902a506246eb35c485e412c83403b4ec03cd109178b5"
const imageSmokeAccount = "isolated-image-native-smoke"
const imageSmokeNativeManifest = "cca734091bfb996a5275c9e12f2e4ced749844fb28f82108baf1f2345be3770e"
const imageSmokeCaptionPatch = "llama CPU caption patch sha256=335fc4c0d9ad369a1d24e5f5efaeb8a58dbe033d2286c9209a22105b035dbf57"

type imageSmokeBinary struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func (b imageSmokeBinary) spec() RuntimeSpec {
	return RuntimeSpec{Path: b.Path, Bytes: b.Bytes, SHA256: b.SHA256}
}

// The root coordinator supplies this pinned private binding after separately
// admitting/building the real binaries and installing the exact catalog assets.
// This test never discovers executables, downloads/imports models, or uses the
// ordinary app root. WorkRoot must be a separate, precreated empty directory.
type imageSmokeBinding struct {
	Version         int              `json:"version"`
	GrantID         string           `json:"grant_id"`
	Scope           string           `json:"scope"`
	WorkRoot        string           `json:"work_root"`
	ModelsRoot      string           `json:"models_root"`
	OutputDirectory string           `json:"output_directory"`
	Fixture         string           `json:"fixture"`
	OCRProfile      string           `json:"ocr_profile"`
	OCRIdentity     string           `json:"ocr_identity"`
	CaptionIdentity string           `json:"caption_identity"`
	RSSMaximumBytes int64            `json:"rss_maximum_bytes"`
	OCR             imageSmokeBinary `json:"ocr"`
	Caption         imageSmokeBinary `json:"caption"`
	Holder          imageSmokeBinary `json:"holder"`
}

func imageSmokePath(path string) bool {
	return len(path) > 0 && len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func imageSmokeFile(path string, maximum int64) (*os.File, error) {
	if !imageSmokePath(path) {
		return nil, ErrInvalidRequest
	}
	// Reuse production ancestry/owner/mode/ACL/one-link checks, with no
	// pathname reopening after the fixture has become the held source handle.
	dir, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	f, err := dir.open(filepath.Base(path), unix.O_RDONLY)
	if err != nil {
		return nil, errors.Join(err, dir.file.Close())
	}
	info, statErr := f.Stat()
	identityErr := dir.same(filepath.Base(path), f)
	closeErr := dir.file.Close()
	if statErr != nil || identityErr != nil || closeErr != nil || info.Size() <= 0 || info.Size() > maximum {
		_ = f.Close()
		return nil, ErrUnsafeFile
	}
	return f, nil
}

func imageSmokeLoadBinding(t *testing.T) imageSmokeBinding {
	t.Helper()
	path, digest := os.Getenv("WA_INDEXER_IMAGE_SMOKE_BINDING"), os.Getenv("WA_INDEXER_IMAGE_SMOKE_BINDING_SHA256")
	if !validDigest(digest) {
		t.Fatal("enabled image smoke requires a pinned binding SHA256")
	}
	f, err := imageSmokeFile(path, 16<<10)
	if err != nil {
		t.Fatal("enabled image smoke requires a private bounded binding")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	closeErr := f.Close()
	sum := sha256.Sum256(data)
	if readErr != nil || closeErr != nil || len(data) > 16<<10 || hex.EncodeToString(sum[:]) != digest {
		t.Fatal("image smoke binding integrity failed")
	}
	var binding imageSmokeBinding
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("image smoke binding schema invalid")
	}
	if binding.Version != 1 || binding.Scope != "SYNTHETIC_CC0_IMAGE_CODE_SMOKE_ONLY" || len(binding.GrantID) == 0 || len(binding.GrantID) > 128 || strings.ContainsAny(binding.GrantID, "\x00\r\n") || !imageSmokePath(binding.WorkRoot) || !imageSmokePath(binding.ModelsRoot) || !imageSmokePath(binding.OutputDirectory) || !imageSmokePath(binding.Fixture) || (binding.OCRProfile != "tesseract-fast" && binding.OCRProfile != "tesseract-best") || !validDigest(binding.OCRIdentity) || !validDigest(binding.CaptionIdentity) || binding.RSSMaximumBytes < 1<<30 || binding.RSSMaximumBytes > 6<<30 {
		t.Fatal("image smoke binding prerequisites invalid")
	}
	for _, binary := range []imageSmokeBinary{binding.OCR, binding.Caption, binding.Holder} {
		if !imageSmokePath(binary.Path) || binary.Bytes <= 0 || binary.Bytes > 256<<20 || !validDigest(binary.SHA256) {
			t.Fatal("image smoke requires all three pinned real executables")
		}
	}
	for _, pair := range [][2]string{{binding.WorkRoot, binding.ModelsRoot}, {binding.ModelsRoot, binding.WorkRoot}, {binding.OutputDirectory, binding.WorkRoot}, {binding.WorkRoot, binding.OutputDirectory}, {binding.OutputDirectory, binding.ModelsRoot}, {binding.ModelsRoot, binding.OutputDirectory}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err != nil || relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			t.Fatal("image smoke work, model and output roots must be separate")
		}
	}
	return binding
}

type imageSmokeOwners struct {
	cancel         context.CancelFunc
	instance       *appdata.InstanceLock
	repo           *archive.Store
	space          *diskspace.Controller
	metadata       diskspace.Lease
	output         *directory
	report         *imageSmokeReport
	models         *modelstore.Store
	media          *Store
	namespace      *workerlife.ExecutableNamespace
	holder         *os.File
	fixture        *os.File
	root           *os.File
	scheduler      *compute.ThreadBudgetScheduler
	extractor      *Extractor
	download       *Download
	original       *Original
	diagnosticFile *os.File
	diagnostic     *nativeRuntimeDiagnostic
	diagnosticCode string
	uncertainClose error
}

// Stop/acknowledgement precedes upstream closure. On uncertain native cleanup
// this deliberately retains the namespace, quotas and original instance OFD.
// The explicit root is never removed automatically, including by t.TempDir.
func (o *imageSmokeOwners) close() error {
	// An uncertain first descriptor Close cannot be retried safely: its numeric
	// FD may already have been reused. Retain all upstream ownership until the
	// outer FAMILY runner proves process death and positive owner absence.
	if o.uncertainClose != nil {
		return ErrCleanup
	}
	if o.cancel != nil {
		o.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if o.extractor != nil {
		if err := o.extractor.Close(ctx); err != nil {
			return err
		}
		o.extractor = nil
	}
	// Only after the supervised native/holder cleanup is acknowledged may the
	// private diagnostic reach fsync/first-close and upstream release.
	if err := o.finishDiagnostic(); err != nil {
		return err
	}
	if o.original != nil {
		if err := o.original.Close(); err != nil {
			return err
		}
		o.original = nil
	}
	if o.repo != nil {
		page, err := o.repo.ListPinnedExtractions(ctx, archive.ExtractionRecoveryRequest{Account: imageSmokeAccount, Limit: 64})
		if err != nil || page.NextAfter != "" {
			return ErrCleanup
		}
		for _, pin := range page.Items {
			if err = o.repo.RetryExtraction(ctx, archive.JobRetry{Lease: pin.Lease, Code: "cancelled", Terminal: true}); err != nil {
				return err
			}
		}
	}
	if o.download != nil {
		if err := o.download.Close(); err != nil {
			return err
		}
		o.download = nil
	}
	if o.namespace != nil {
		if err := o.namespace.Close(); err != nil {
			return err
		}
		o.namespace = nil
	}
	if o.holder != nil {
		if err := o.holder.Close(); err != nil {
			return err
		}
		o.holder = nil
	}
	if o.models != nil {
		if err := o.models.Close(); err != nil {
			return err
		}
		o.models = nil
	}
	if o.media != nil {
		if err := o.media.Close(); err != nil {
			return err
		}
		o.media = nil
	}
	if o.repo != nil {
		if err := o.repo.Drain(ctx); err != nil {
			return err
		}
		if err := o.repo.Close(); err != nil {
			return err
		}
		o.repo = nil
	}
	if o.fixture != nil {
		if err := o.fixture.Close(); err != nil {
			return err
		}
		o.fixture = nil
	}
	if o.root != nil {
		if err := o.root.Close(); err != nil {
			return err
		}
		o.root = nil
	}
	if o.instance != nil {
		if err := o.instance.Close(); err != nil {
			return err
		}
		o.instance = nil
	}
	if o.scheduler != nil {
		o.scheduler.Close()
		o.scheduler = nil
	}
	// Native/input cleanup is already acknowledged. The remaining finite
	// metadata admission covers this private report on the same filesystem.
	// Its flag is accepted only with this test's final PASS and outer FAMILY
	// census, which also require the following fsync/close/release sequence.
	if o.report != nil {
		if o.output == nil || o.metadata == nil || o.space == nil {
			return ErrCleanup
		}
		if err := imageSmokeWriteReport(o.output, *o.report); err != nil {
			return err
		}
		o.report = nil
	}
	if o.output != nil {
		if err := o.output.file.Close(); err != nil {
			return err
		}
		o.output = nil
	}
	if o.metadata != nil {
		if err := o.metadata.Release(); err != nil {
			return err
		}
		o.metadata = nil
	}
	if o.space != nil {
		if err := o.space.Close(); err != nil {
			return err
		}
		o.space = nil
	}
	return nil
}

func imageSmokeEmptyRoot(path string) (*os.File, error) {
	dir, err := openDirectory(path)
	if err != nil {
		return nil, err
	}
	f := dir.file
	names, err := f.Readdirnames(1)
	if len(names) != 0 || err != io.EOF {
		_ = f.Close()
		return nil, ErrUnsafeFile
	}
	return f, nil
}

// The existing8MiB bootstrap/control admission also covers this fixed32KiB
// diagnostic and small numeric status record. It lives under the media Store's
// retained same-device working FD, never a new path-discovery authority.
func (o *imageSmokeOwners) finishDiagnostic() error {
	if o.diagnosticFile == nil {
		return nil
	}
	o.uncertainClose = ErrCleanup
	if o.media == nil || o.diagnostic == nil {
		return ErrCleanup
	}
	dir := o.media.working
	file := o.diagnosticFile
	status := o.diagnostic.snapshot()
	info, err := file.Stat()
	if err != nil || info.Size() != status.StderrBytes || info.Size() > nativeDiagnosticMaximumBytes ||
		dir.same("caption-native.stderr", file) != nil || checkFilesystem(file, dir.expectedDevice) != nil || file.Sync() != nil {
		return ErrCleanup
	}
	// Retain all upstream owners on an uncertain FIRSTclose; never retry the
	// consumed numeric descriptor or refund its admitted metadata allowance.
	o.uncertainClose = ErrCleanup
	o.diagnosticFile = nil
	if err := file.Close(); err != nil {
		return ErrCleanup
	}
	body, err := json.Marshal(struct {
		Scope                        string                 `json:"scope"`
		Kind                         string                 `json:"kind"`
		PrimaryCode                  string                 `json:"primary_code"`
		RuntimeSHA256                string                 `json:"runtime_sha256"`
		Status                       nativeDiagnosticStatus `json:"status"`
		ExtractorCleanupAcknowledged bool                   `json:"extractor_cleanup_acknowledged"`
		StderrCloseAcknowledged      bool                   `json:"stderr_close_acknowledged"`
	}{"SYNTHETIC_CC0_IMAGE_CODE_SMOKE_ONLY", "caption", o.diagnosticCode, o.diagnostic.runtime.SHA256, status, true, true})
	if err != nil || len(body) > 16<<10 {
		return ErrCleanup
	}
	metadata, err := dir.open("caption-native-status.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return ErrCleanup
	}
	n, writeErr := metadata.Write(body)
	if writeErr == nil && n != len(body) {
		writeErr = io.ErrShortWrite
	}
	syncErr := metadata.Sync()
	identityErr := dir.same("caption-native-status.json", metadata)
	o.uncertainClose = ErrCleanup
	closeErr := metadata.Close()
	if closeErr != nil {
		return ErrCleanup
	}
	if errors.Join(writeErr, syncErr, identityErr, dir.file.Sync()) != nil || status.WriteFailed {
		return ErrCleanup
	}
	o.diagnostic = nil
	o.uncertainClose = nil
	return nil
}

type imageSmokeReport struct {
	Scope                       string                      `json:"scope"`
	GrantID                     string                      `json:"grant_id"`
	FixtureSHA256               string                      `json:"fixture_sha256"`
	Quality                     string                      `json:"quality"`
	LatencyRSSRelease           string                      `json:"latency_rss_release"`
	RSSMaximumBytes             int64                       `json:"rss_maximum_bytes"`
	Versions                    []archive.ExtractionVersion `json:"versions"`
	Documents                   []archive.Document          `json:"documents"`
	CleanupAcknowledged         bool                        `json:"cleanup_acknowledged"`
	OriginalCleanupAcknowledged bool                        `json:"original_cleanup_acknowledged"`
	SourceRevoked               bool                        `json:"source_revoked"`
}

func imageSmokeWriteReport(dir *directory, report imageSmokeReport) error {
	names, readErr := dir.file.Readdirnames(1)
	if len(names) != 0 || readErr != io.EOF {
		return ErrUnsafeFile
	}
	data, err := json.Marshal(report)
	if err != nil || len(data) > 256<<10 {
		return ErrLimit
	}
	f, err := dir.open("image-native-smoke-generated.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Sync(), f.Close(), dir.file.Sync())
}

// Intended regressions: losing heb+eng or RGB/traineddata FD binding must lose
// the independently frozen logical OCR labels; losing caption model/projector,
// framing, Hebrew output or source/version provenance must fail real inference.
// This is one project-owned synthetic code smoke, never human/visual quality,
// latency/RSS qualification, release readiness, or ordinary app activation.
func TestImageNativeExtractPrimarySmoke(t *testing.T) {
	armed := os.Getenv("WA_INDEXER_IMAGE_SMOKE_ARMED")
	if armed == "" {
		t.Skip("NOT_RUN explicit isolated image native smoke required")
	}
	if armed != "1" {
		t.Fatal("invalid image native smoke opt-in")
	}
	binding := imageSmokeLoadBinding(t)
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		t.Fatal("pinned model catalog unavailable")
	}
	// A legacy profile identity must not label a v2 caption producer. This
	// intentionally rejects the old public catalog before any fixture body or
	// model acquisition; ROOT must separately promote and recapture catalog3.
	nativeCaption := false
	for _, profile := range catalog.Profiles {
		if profile.ID == "qwen3.5-0.8b-vision" && profile.Manifest == "packaging/media-native-manifest.json" && profile.ManifestSHA256 == imageSmokeNativeManifest && len(profile.RuntimeRequirements) == 2 && profile.RuntimeRequirements[0] == "llama.cpp revision=4f5406761517648c23dbd60ea5ade37f77a316c9; CPU vision worker" && profile.RuntimeRequirements[1] == imageSmokeCaptionPatch && profile.Selection == "candidate" && profile.GateStatus == "NOT_RUN" {
			nativeCaption = true
		}
	}
	if !nativeCaption {
		t.Fatal("UNARMED: legacy Qwen manifest/runtime provenance cannot authorize v2 image smoke")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	own := &imageSmokeOwners{cancel: cancel}
	t.Cleanup(func() {
		if err := own.close(); err != nil {
			t.Error("image smoke ownership cleanup is unacknowledged; retain isolated root")
		}
	})
	own.root, err = imageSmokeEmptyRoot(binding.WorkRoot)
	if err != nil {
		t.Fatal("image smoke requires an explicit existing empty private work root")
	}
	own.output, err = openDirectory(binding.OutputDirectory)
	if err != nil {
		t.Fatal("image smoke requires a separate empty private report directory")
	}
	if names, err := own.output.file.Readdirnames(1); len(names) != 0 || err != io.EOF {
		t.Fatal("private report directory must start empty")
	}
	own.fixture, err = imageSmokeFile(binding.Fixture, 10<<20)
	if err != nil {
		t.Fatal("CC0 printed image fixture unavailable")
	}
	fixtureInfo, err := own.fixture.Stat()
	if err != nil {
		t.Fatal("CC0 printed image fixture metadata unavailable")
	}
	fixtureHash := sha256.New()
	if _, err = io.Copy(fixtureHash, io.NewSectionReader(own.fixture, 0, fixtureInfo.Size())); err != nil || hex.EncodeToString(fixtureHash.Sum(nil)) != imageSmokeFixtureSHA {
		t.Fatal("CC0 printed image fixture integrity failed")
	}
	own.space, err = diskspace.New(binding.WorkRoot, diskspace.Options{})
	if err != nil {
		t.Fatal("shared physical admission unavailable")
	}
	physical, err := own.space.Snapshot()
	if err != nil {
		t.Fatal("shared physical admission identity unavailable")
	}
	if physical.ActiveLeases != 0 || physical.ReservedBytes != 0 {
		t.Fatal("isolated broker did not start with empty accounting")
	}
	if err = checkFilesystem(own.output.file, physical.Device); err != nil {
		t.Fatal("private report output differs from the shared broker filesystem")
	}
	// Admit finite bootstrap/SQLite/control/report growth before preparation.
	// This allowance is not an aggregate SQLite hard size-cap assertion.
	own.metadata, err = own.space.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 8 << 20})
	if err != nil {
		t.Fatal("isolated bootstrap metadata admission denied")
	}
	paths, err := appdata.Prepare(binding.WorkRoot)
	if err != nil {
		t.Fatal("isolated image smoke namespace preparation failed")
	}
	own.instance, err = appdata.Acquire(paths)
	if err != nil {
		t.Fatal("isolated image smoke instance ownership unavailable")
	}
	own.repo, err = archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal("isolated real SQLite archive unavailable")
	}
	if account, err := own.repo.BoundAccount(ctx); err != nil || account != "" {
		t.Fatal("isolated image archive was already account-bound")
	}
	if pins, err := own.repo.ListPinnedExtractions(ctx, archive.ExtractionRecoveryRequest{Account: imageSmokeAccount, Limit: 64}); err != nil || len(pins.Items) != 0 || pins.NextAfter != "" {
		t.Fatal("new isolated archive did not reach extraction recovery EOF")
	}
	own.media, err = Open(paths, own.repo, Options{Admission: own.space, ExpectedDevice: physical.Device, LeaseTTL: 10 * time.Minute})
	if err != nil {
		t.Fatal("source-bound same-device media store unavailable")
	}
	if reconciled, err := own.media.Reconcile(ctx, 16); err != nil || reconciled.Active != 0 || reconciled.Cleaned != 0 {
		t.Fatal("fresh media namespace did not reconcile to empty ownership")
	}
	// Borrow the Store's retained same-device working anchor. A pathname reopen
	// must not redirect namespace growth onto a replacement filesystem.
	working := own.media.working
	if err = checkPrivateFD(working.file, true); err != nil {
		t.Fatal("validated working-directory owner changed")
	}
	if err = checkFilesystem(working.file, physical.Device); err != nil {
		t.Fatal("executable working anchor differs from the shared broker filesystem")
	}
	if err = unix.Mkdirat(int(working.file.Fd()), "native-exec", 0700); err != nil {
		t.Fatal("anchored executable namespace creation failed")
	}
	if err = working.file.Sync(); err != nil {
		t.Fatal("executable namespace directory creation unacknowledged")
	}
	namespaceDir, err := working.subdirectory("native-exec")
	if err != nil {
		t.Fatal("same-device executable namespace child unavailable")
	}
	own.namespace, err = workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: namespaceDir.file, Path: namespaceDir.path, Admit: func(admitCtx context.Context, growth int64) (workerlife.GrowthLease, error) {
		return own.space.Reserve(admitCtx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: growth})
	}})
	// Close the temporary child exactly once even when construction failed.
	// Preserve an uncertain first close before cleanup can refund capacity or
	// acknowledge durable ownership; never retry this numeric descriptor.
	closeErr := namespaceDir.file.Close()
	if closeErr != nil {
		own.uncertainClose = closeErr
	}
	if err != nil || closeErr != nil {
		t.Fatal("namespace preparation/first descriptor close failed", errors.Join(err, closeErr))
	}
	if err = own.namespace.Recover(ctx); err != nil {
		t.Fatal("real executable namespace recovery unavailable")
	}
	own.holder, err = openRuntime(ctx, binding.Holder.spec())
	if err != nil {
		t.Fatal("pinned genuine workerlife holder unavailable")
	}
	own.models, err = modelstore.Open(binding.ModelsRoot, catalog, modelstore.Options{ExpectedDevice: physical.Device, ReserveSpace: func(admitCtx context.Context, growth int64) (modelstore.SpaceReservation, error) {
		return own.space.Reserve(admitCtx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: growth})
	}})
	if err != nil {
		t.Fatal("preinstalled same-device model store unavailable")
	}
	runtimes := RuntimePaths{OCR: binding.OCR.spec(), Caption: binding.Caption.spec()}
	var versions []archive.ExtractionVersion
	for _, route := range []struct {
		kind              archive.JobKind
		profile, identity string
	}{{archive.JobOCR, binding.OCRProfile, binding.OCRIdentity}, {archive.JobCaption, "qwen3.5-0.8b-vision", binding.CaptionIdentity}} {
		profile, err := own.models.Verify(ctx, route.profile)
		if err != nil || profile.Identity != route.identity {
			t.Fatal("actual whole-profile identity differs from trusted smoke binding")
		}
		version, err := NewExtractionVersion(route.kind, profile, runtimes, "")
		if err != nil {
			t.Fatal("actual primary extraction version unavailable")
		}
		versions = append(versions, version)
	}
	policy, err := own.repo.Policy(ctx)
	if err != nil {
		t.Fatal("fresh isolated policy unavailable")
	}
	policy, err = own.repo.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, policy.Generation)
	if err != nil {
		t.Fatal("isolated smoke policy unavailable")
	}
	key := archive.SourceKey{Account: imageSmokeAccount, Chat: "synthetic-image", Sender: "project-owned", Message: "printed-he-en"}
	event := archive.SourceEvent{Key: key, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Second), OriginalDateKnown: true, SourceRevision: 1, Caption: "CC0 synthetic image native code smoke", BodyKind: archive.MessageImage, Media: []archive.MediaReference{{Kind: "image", ProtocolReference: []byte("project-owned synthetic image smoke reference"), MimeType: "image/png", DeclaredBytes: fixtureInfo.Size(), Availability: "not_retained"}}}
	applied, err := own.repo.ApplySourceEvent(ctx, event, policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatal("isolated permitted image association unavailable")
	}
	digest, _ := hex.DecodeString(imageSmokeFixtureSHA)
	var protocolSHA [32]byte
	copy(protocolSHA[:], digest)
	request := DownloadRequest{Key: key, Version: applied.Version, MediaID: applied.MediaIDs[0], Kind: archive.MessageImage, DeclaredBytes: fixtureInfo.Size(), ProtocolSHA256: protocolSHA}
	own.download, err = own.media.AdmitDownload(ctx, request)
	if err != nil {
		t.Fatal("original pre-byte admission denied")
	}
	if n, err := io.Copy(own.download.File(), io.NewSectionReader(own.fixture, 0, fixtureInfo.Size())); err != nil || n != fixtureInfo.Size() {
		t.Fatal("bounded synthetic original write failed")
	}
	if _, err = own.download.File().Seek(0, io.SeekStart); err != nil {
		t.Fatal("admitted fixture receipt rewind failed")
	}
	if err = own.download.Complete(ctx, ingestion.VerifiedMedia{Key: key, Version: applied.Version, Kind: archive.MessageImage, SHA256: protocolSHA, Bytes: fixtureInfo.Size(), Reader: io.LimitReader(own.download.File(), fixtureInfo.Size())}); err != nil {
		t.Fatal("verified synthetic original publication failed")
	}
	if err = own.download.Close(); err != nil {
		t.Fatal("original stopped-writer acknowledgement failed")
	}
	own.download = nil
	scope := archive.ReadScope{Account: imageSmokeAccount, Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}
	own.scheduler, err = compute.NewThreadBudgetScheduler(2)
	if err != nil {
		t.Fatal("bounded media scheduler unavailable")
	}
	var acquired []*modelstore.Lease
	own.extractor, err = NewExtractor(ExtractorOptions{Store: own.media, Scheduler: own.scheduler, InstanceGuard: own.instance, OCRProfile: binding.OCRProfile, Runtimes: runtimes, ExtractionVersions: versions, Timeout: 2 * time.Minute,
		WorkerLifetime: workerlife.Config{Executable: binding.Holder.Path, ExecutableFile: own.holder, ExecutableProvider: own.namespace, ReadyTimeout: 10 * time.Second},
		Acquire: func(acquireCtx context.Context, id string) (MediaModelLease, error) {
			lease, err := own.models.Acquire(acquireCtx, id)
			if lease == nil {
				return nil, err
			}
			acquired = append(acquired, lease)
			return lease, err
		},
	})
	if err != nil {
		t.Fatal("trusted primary image extractor unavailable")
	}
	var ocrRoot archive.DocumentID
	var generated []archive.Document
	for _, version := range versions {
		work, err := own.repo.ClaimExtraction(ctx, archive.ExtractionClaim{Account: imageSmokeAccount, Version: version, TTL: 10 * time.Minute})
		if err != nil || work.Version != version || work.MediaID != request.MediaID || work.OriginalSHA256 != imageSmokeFixtureSHA {
			t.Fatal("claimed image work lost source/media/artifact/version identity")
		}
		loaded, err := own.repo.LoadExtractionInput(ctx, work.Lease)
		if err != nil || loaded != work {
			t.Fatal("exact current extraction input unavailable")
		}
		own.original, err = own.media.OpenOriginal(ctx, scope, loaded.MediaID)
		if err != nil {
			t.Fatal("exact authorized original unavailable")
		}
		meta := own.original.Metadata()
		if meta.MediaID != work.MediaID || meta.ArtifactID != work.ArtifactID || meta.SHA256 != work.OriginalSHA256 || !sameSource(meta.Source, work.Source) || meta.Bytes != fixtureInfo.Size() || meta.Format != "png" {
			t.Fatal("held PNG original differs from exact extraction source/artifact")
		}
		// The archive lease is deliberately much longer than the fixed native
		// two-minute timeout. No asynchronous renewal can race successful commit;
		// an expired or uncertain call still retains the durable recovery pin.
		if deadline, err := own.repo.RenewExtraction(ctx, work.Lease, 10*time.Minute); err != nil || !deadline.After(time.Now().Add(2*time.Minute)) {
			t.Fatal("exact image input renewal unavailable before bounded inference")
		}
		nativeCtx := ctx
		if version.Kind == archive.JobCaption {
			own.diagnosticFile, err = own.media.working.open("caption-native.stderr", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
			if err != nil {
				t.Fatal("bounded private caption diagnostic unavailable")
			}
			nativeCtx, own.diagnostic, err = withNativeDiagnostic(ctx, binding.Caption.spec(), own.diagnosticFile)
			if err != nil {
				t.Fatal("private exact-runtime diagnostic binding rejected")
			}
		}
		result, err := own.extractor.ExtractPrimary(nativeCtx, loaded, own.original)
		if version.Kind == archive.JobCaption {
			own.diagnosticCode = ExtractionCode(err)
		}
		if err != nil {
			t.Fatalf("actual image native code smoke failed: %s", ExtractionCode(err))
		}
		if result.Lease != work.Lease || result.ContentHash != work.ContentHash || result.Outcome != archive.ExtractionText || len(result.Documents) != 1 {
			t.Fatal("actual image inference did not produce one complete text root")
		}
		doc := result.Documents[0]
		kind, prompt, language := archive.DocumentOCR, "none-v1", "heb+eng"
		if version.Kind == archive.JobCaption {
			kind, prompt, language = archive.DocumentCaption, "he-grounded-canvas-v2", "he"
		}
		if version.Language != language || version.ExtractorVersion != "native-media-v1" || doc.Kind != kind || doc.PassageTokens != 0 || len(doc.Spans) != 0 || len(doc.Sources) != 1 || !sameSource(doc.Sources[0], work.Source) || doc.Provenance.Kind != kind || doc.Provenance.MediaID != work.MediaID || doc.Provenance.ProfileID != version.ProfileID || doc.Provenance.ExtractorVersion != "native-media-v1" || doc.Provenance.PromptVersion != prompt || doc.Provenance.OffsetKind != "none" || doc.Provenance.StartMS != 0 || doc.Provenance.EndMS != 0 {
			t.Fatal("actual image root gained false or incomplete provenance")
		}
		if doc.ID == "" || !utf8.ValidString(doc.Text) || len(doc.Text) == 0 || len(doc.Text) > 64<<10 || strings.ContainsRune(doc.Text, 0) || strings.Contains(doc.Text, "WA_MEDIA_CAPTION_") || strings.Contains(doc.Text, "<think>") {
			t.Fatal("actual image root escaped bounded generated-text framing")
		}
		if version.Kind == archive.JobOCR {
			// Frozen logical labels are independent of the OCR implementation.
			if !strings.Contains(doc.Text, "שלום עולם") || !strings.Contains(doc.Text, "123") || !strings.Contains(doc.Text, "Meeting Tuesday 10:30") {
				t.Fatal("actual heb+eng OCR lost frozen logical Hebrew/English/numeric labels")
			}
			ocrRoot = doc.ID
		} else {
			hebrew := false
			for _, r := range doc.Text {
				if unicode.In(r, unicode.Hebrew) {
					hebrew = true
					break
				}
			}
			if !hebrew || strings.TrimSpace(doc.Text) == "" {
				t.Fatal("actual caption produced no Hebrew text; quality remains untested")
			}
		}
		current, err := own.repo.CurrentSource(ctx, key)
		if err != nil || !sameSource(current, work.Source) {
			t.Fatal("source changed before primary image publication")
		}
		if err = own.original.Close(); err != nil {
			t.Fatal("physical original reader cleanup failed before exact commit")
		}
		own.original = nil
		if accepted, err := own.repo.CommitExtraction(ctx, result); err != nil || !accepted {
			t.Fatal("exact actual image root publication rejected")
		}
		generated = append(generated, doc)
		pins, err := own.repo.ListPinnedExtractions(ctx, archive.ExtractionRecoveryRequest{Account: imageSmokeAccount, Limit: 64})
		if err != nil || len(pins.Items) != 0 || pins.NextAfter != "" {
			t.Fatal("completed image publication did not acknowledge input ownership")
		}
		budget, err := own.repo.GetOriginalBudget(ctx)
		if err != nil || budget.ChargedBytes != fixtureInfo.Size() {
			t.Fatal("extraction completion forgave retained-original quota")
		}
		t.Logf("synthetic_code_smoke_only kind=%s generated_bytes=%d quality=NOT_RUN", version.Kind, len(doc.Text))
	}
	closing, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	if err = own.extractor.Close(closing); err != nil {
		t.Fatal("actual native/holder cleanup unacknowledged")
	}
	own.extractor = nil
	for _, lease := range acquired {
		guard, err := lease.WorkerGuard()
		if guard != nil {
			_ = guard.Close()
		}
		if !errors.Is(err, modelstore.ErrLeaseClosed) {
			t.Fatal("model profile ownership outlived acknowledged extraction")
		}
	}
	if snapshot := own.scheduler.Snapshot(); snapshot.Active != 0 || snapshot.Heavy != 0 || snapshot.Threads != 0 || snapshot.Queued != 0 {
		t.Fatal("native cleanup released no complete compute acknowledgement")
	}
	if snapshot, err := own.space.Snapshot(); err != nil || snapshot.ActiveLeases != 1 || snapshot.DecodedBytes != 0 || snapshot.OriginalBytes != 0 || snapshot.ReservedBytes != 8<<20 || snapshot.ModelInstallBytes != 8<<20 {
		t.Fatal("native/decoded/executable physical charges outlived acknowledged cleanup")
	}
	if err = own.media.Evict(ctx, request.MediaID); err != nil {
		t.Fatal("native original/decoded holder OFDs remain live after acknowledged cleanup")
	}
	if budget, err := own.repo.GetOriginalBudget(ctx); err != nil || budget.ChargedBytes != 0 {
		t.Fatal("verified original removal did not acknowledge durable quota cleanup")
	}
	page, err := own.repo.Search(ctx, scope, archive.SearchRequest{Query: "שלום", Limit: 10})
	found := false
	for _, hit := range page.Hits {
		if hit.DocumentID == ocrRoot {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatal("actual OCR keyword root was lost after original cache eviction")
	}
	if _, err = own.repo.ApplySourceEvent(ctx, archive.SourceEvent{Key: key, Kind: archive.EventDelete, SourceRevision: 2}, policy.Generation); err != nil {
		t.Fatal("isolated source invalidation failed")
	}
	if page, err = own.repo.Search(ctx, scope, archive.SearchRequest{Query: "שלום", Limit: 10}); err != nil || len(page.Hits) != 0 {
		t.Fatal("source revocation left actual OCR text readable")
	}
	if err = own.namespace.Close(); err != nil {
		t.Fatal("real executable snapshot cleanup unacknowledged")
	}
	own.namespace = nil
	entries, err := os.ReadDir(filepath.Join(paths.WorkingDir, "native-exec"))
	if err != nil || len(entries) != 0 {
		t.Fatal("actual executable namespace retained staged or published snapshots")
	}
	fixtureHash.Reset()
	if n, err := io.Copy(fixtureHash, io.NewSectionReader(own.fixture, 0, fixtureInfo.Size()+1)); err != nil || n != fixtureInfo.Size() || hex.EncodeToString(fixtureHash.Sum(nil)) != imageSmokeFixtureSHA {
		t.Fatal("held fixture changed during actual extraction")
	}
	// Generated text is a separate private artifact, never a public test log or
	// a quality label. The outer root-owned lane decides later artifact cleanup.
	// close writes it only after input/native owners ack, then releases the
	// remaining metadata lease after report fsync/close acknowledgement.
	own.report = &imageSmokeReport{Scope: binding.Scope, GrantID: binding.GrantID, FixtureSHA256: imageSmokeFixtureSHA, Quality: "NOT_RUN", LatencyRSSRelease: "NOT_RUN", RSSMaximumBytes: binding.RSSMaximumBytes, Versions: versions, Documents: generated, CleanupAcknowledged: true, OriginalCleanupAcknowledged: true, SourceRevoked: true}
	if err = own.close(); err != nil {
		t.Fatal("complete image smoke/report cleanup unacknowledged; retain isolated root")
	}
	// Prevent a second close of plain borrowed input descriptors in t.Cleanup.
	*own = imageSmokeOwners{}
	t.Log("synthetic_CC0_image_code_smoke_only actual_OCR_caption_and_cleanup=PASS quality_latency_RSS_release=NOT_RUN")
}
