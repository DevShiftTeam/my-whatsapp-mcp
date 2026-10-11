//go:build native_media_smoke && sqlite_fts5 && darwin && arm64

package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// These separately gated opt-in tests have no discovery/default paths,
// synthetic native producer, encoder, acceptance threshold, or default-profile
// authority. Bootstrap uses only the existing pinned public installer; it never
// arms smoke. ROOT must approve each exact binding and actual execution. Each
// unarmed gate precedes binding-file, fixture, model and executable reads.
const hebrewSmokeArm = "ROOT_APPROVED_HEBREW_SMOKE3_ONCE"
const hebrewSmokeProfile = "ivrit-whisper-large-v3-turbo"
const hebrewSmokeManifest = "2f9ef04d7321635f1cbb6182c7163445949f4a6d37c02649e9eae1c173693a80"
const hebrewBootstrapArm = "ROOT_APPROVED_IVRIT_BOOTSTRAP_ONCE"

// Each exact phase is selected once without Parallel. These two fixed slots
// retain uncertain binding-directory owners until process/FAMILY termination;
// a GC finalizer cannot substitute for an explicit first-close acknowledgement.
var hebrewBootstrapUncertainBindingDirectory, hebrewSmokeUncertainBindingDirectory *os.File

type hebrewBootstrapBinding struct {
	Schema, GrantID, Scope, InstallRoot, OutputDirectory string
	RSSMaximumBytes                                      int64
}

type hebrewBootstrapReport struct {
	Scope, GrantID, InstallRoot, ModelDirectory, WorkingDirectory string
	Installed                                                     modelstore.InstalledProfile
	Space                                                         modelstore.SpacePlan
	Device                                                        uint64
	InstallCalls                                                  int
	StartedUTC, FinishedUTC                                       time.Time
	InstallSeconds                                                float64
	CleanupAcknowledged                                           bool
	Interpretation                                                string
}

// Separate explicit acquisition gate. This calls the existing catalog installer
// once; it cannot invoke native workers or automatically arm the later smoke.
func hebrewBootstrapRead(t *testing.T) hebrewBootstrapBinding {
	t.Helper()
	if os.Getenv("WA_INDEXER_HEBREW_MODEL_ARMED") != hebrewBootstrapArm {
		t.Fatal("UNARMED: separate ROOT pinned-model acquisition grant required; no model payload or native child accessed")
	}
	name, digest := os.Getenv("WA_INDEXER_HEBREW_MODEL_BINDING"), os.Getenv("WA_INDEXER_HEBREW_MODEL_BINDING_SHA256")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || !validDigest(digest) {
		t.Fatal("invalid explicit bootstrap binding")
	}
	data, err := hebrewSmokeReadBinding(name, digest, &hebrewBootstrapUncertainBindingDirectory)
	if err != nil {
		t.Fatal("bootstrap binding integrity/first-close acknowledgement failed", err)
	}
	var b hebrewBootstrapBinding
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF || b.Schema != "wa-indexer-ivrit-bootstrap-v1" || b.Scope != "INSTALLATION_ONLY_NOT_HEBREW_QUALIFIED" || len(b.GrantID) < 1 || len(b.GrantID) > 128 || strings.ContainsAny(b.GrantID, "\x00\r\n") || b.RSSMaximumBytes != 4<<30 {
		t.Fatal("UNARMED: incomplete exact bootstrap authority")
	}
	for _, path := range []string{b.InstallRoot, b.OutputDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			t.Fatal("UNARMED: canonical ROOT-approved bootstrap paths required")
		}
	}
	return b
}

func TestHebrewNativeModelBootstrap(t *testing.T) {
	b := hebrewBootstrapRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	o := &hebrewSmokeOwner{}
	report := hebrewBootstrapReport{Scope: b.Scope, GrantID: b.GrantID, InstallRoot: b.InstallRoot, StartedUTC: time.Now().UTC(), Interpretation: "Installation receipt only; candidate/NOT_RUN acceptance unchanged; no native, encoder, fixture PCM read or smoke automatically invoked; installed component Paths are diagnostic and never loading authority"}
	t.Cleanup(func() {
		if err := o.close(); err != nil {
			t.Error("bootstrap cleanup uncertain; upstream owners/reservations retained", err)
			return
		}
		report.CleanupAcknowledged = true
		report.FinishedUTC = time.Now().UTC()
		if err := hebrewSmokeWriteJSON(b.OutputDirectory, "hebrew-model-install.json", report, 16<<10, o.device); err != nil {
			t.Error("bootstrap receipt durability failed", err)
			return
		}
		if err := o.closeFinal(); err != nil {
			t.Error("final bootstrap owner close uncertain", err)
		}
	})
	root, err := openDirectory(b.InstallRoot)
	if err != nil {
		t.Fatal("unsafe bootstrap root")
	}
	emptyErr := hebrewSmokeEmpty(root)
	closeErr := root.file.Close()
	if closeErr != nil {
		o.uncertainClose = closeErr
	}
	if emptyErr != nil || closeErr != nil {
		t.Fatal("bootstrap requires genuinely fresh private root", errors.Join(emptyErr, closeErr))
	}
	o.broker, err = diskspace.New(b.InstallRoot, diskspace.Options{HeadroomBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := o.broker.Snapshot()
	if err != nil || snapshot.ReservedBytes != 0 {
		t.Fatal("bootstrap broker starts with no outstanding reservations")
	}
	report.Device = snapshot.Device
	o.device = snapshot.Device
	if err := hebrewSmokeCheckOutput(o, b.OutputDirectory); err != nil {
		t.Fatal("bootstrap output must be private on the broker filesystem", err)
	}
	// One controller owns all constructor/report metadata and the install's own
	// exact new-growth reservation. Existing bytes later remain in fstatfs free.
	o.metadata, err = o.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(b.InstallRoot)
	if err != nil {
		t.Fatal(err)
	}
	o.instance, err = appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	report.ModelDirectory, report.WorkingDirectory = paths.ModelDir, paths.WorkingDir
	working, err := openDirectory(paths.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	deviceErr := checkFilesystem(working.file, snapshot.Device)
	workingCloseErr := working.file.Close()
	if workingCloseErr != nil {
		o.uncertainClose = workingCloseErr
	}
	if deviceErr != nil || workingCloseErr != nil {
		t.Fatal("bootstrap working directory/device/first-close verification failed", errors.Join(deviceErr, workingCloseErr))
	}
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, p := range catalog.Profiles {
		if p.ID != hebrewSmokeProfile {
			continue
		}
		if p.Revision != "2130c78e4a9cb4914cc4df91a1c3031407789705" || p.Platform != "any" || p.Selection != "candidate" || p.GateStatus != "NOT_RUN" || p.License != "Apache-2.0" || len(p.Components) != 1 {
			t.Fatal("frozen ivrit candidate profile differs")
		}
		component := p.Components[0]
		if component.ID != "model" || component.Role != "model" || component.Filename != "ggml-model.bin" || component.Bytes != 1624555275 || component.SHA256 != "c8090411113357097bfafc2b8e228ec1639fa7f5fe4ecb5d054ac0ccef8641b1" || component.URL != "https://huggingface.co/ivrit-ai/whisper-large-v3-turbo-ggml/resolve/2130c78e4a9cb4914cc4df91a1c3031407789705/ggml-model.bin" || component.License != "Apache-2.0" {
			t.Fatal("frozen pinned ivrit artifact differs")
		}
		matched = true
	}
	if !matched {
		t.Fatal("pinned ivrit catalog profile missing")
	}
	o.models, err = modelstore.Open(paths.ModelDir, catalog, modelstore.Options{ExpectedDevice: snapshot.Device, HeadroomBytes: 1 << 30, MaxDownloadTime: 30 * time.Minute, ReserveSpace: func(ctx context.Context, maximum int64) (modelstore.SpaceReservation, error) {
		return o.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: maximum})
	}})
	if err != nil {
		t.Fatal(err)
	}
	report.Space, err = o.models.Space(ctx, hebrewSmokeProfile)
	if err != nil {
		t.Fatal(err)
	}
	if report.Space.DownloadBytes != 1624555275 || report.Space.InstalledBytes != 1624555275 || report.Space.GrowthBytes != 1624555275 || report.Space.ExistingBytes != 0 || report.Space.ReusableBytes != 0 || report.Space.TemporaryBytes != 1<<20 || report.Space.HeadroomBytes != 1<<30 || report.Space.RequiredBytes != 2699345675 || report.Space.AdditionalBytes != 0 {
		t.Fatal("fresh pinned install growth/headroom admission unavailable")
	}
	if !t.Run("install_ivrit_catalog_once", func(t *testing.T) {
		report.InstallCalls++
		started := time.Now()
		// The ONLY payload acquisition is public Store.Install using its existing
		// HTTPS allowlist/resume/caps/hash/fsync/atomic-activation implementation.
		if err := o.models.Install(ctx, hebrewSmokeProfile, nil); err != nil {
			t.Fatal("single public pinned installation failed; no automatic retry", err)
		}
		report.InstallSeconds = time.Since(started).Seconds()
		o.model, err = o.models.Acquire(ctx, hebrewSmokeProfile)
		if err != nil {
			t.Fatal("installed profile full-current verification failed", err)
		}
		report.Installed = o.model.Profile()
		if !knownInstalledMediaProfile(report.Installed) || report.Installed.ID != hebrewSmokeProfile || !validDigest(report.Installed.Identity) || report.Installed.Selection != "candidate" || report.Installed.GateStatus != "NOT_RUN" {
			t.Fatal("genuine installed identity/candidate metadata differs")
		}
	}) {
		return
	}
	if report.InstallCalls != 1 {
		t.Fatal("installation must execute exactly once")
	}
}

type hebrewSmokeBinding struct {
	Schema, GrantID, Scope                                         string
	SandboxRoot, ModelDirectory, FixtureDirectory, OutputDirectory string
	FixtureManifestSHA256, ExpectedModelIdentity                   string
	RSSMaximumBytes                                                int64
	Helper                                                         RuntimeSpec
	Decoder, Speech                                                RuntimeSpec
}

type hebrewSmokeFixture struct {
	File, SHA256, HumanLabel string
	Bytes, EndMS             int64
}

var hebrewSmokeFixtures = []hebrewSmokeFixture{
	{"3481283228131691837.wav", "cc966cb5beb88994c0eab7610ec889d2c0125c0280ac2dfde7e9c0f541fa5218", "הם עדיין מנסים לקבוע את ממדי ההתרסקות וכיצד יושפע ממנה כדור הארץ.", 272684, 8520},
	{"4190590012937821325.wav", "a82b848522701dcfe0cf9a9655a637fd235cbbcfb15c9f0226e6feab3ba760de", "הוא בנה פעמון WiFi  לדלת, כך אמר.", 134444, 4200},
	{"12376353851396300425.wav", "c7f7212aa931478910fbf7ed78fa01793c91c28182a63e98042fb469d0f97bea", "מתוך 1,400 איש שהשיבו לסקר לפינ הבחירות הפדרליות ב-2010, מספר המתנגדים לכך שאוסטרליה תהפוך לרפובליקה עלה ב-8 אחוזים מאז 2008.", 549164, 17160},
}

type hebrewSmokeRow struct {
	Fixture, SHA256, HumanLabel, Transcript, NormalizedLabel, NormalizedTranscript string
	Bytes, StartMS, EndMS                                                          int64
	CharacterEdits, ReferenceCharacters, WordEdits, ReferenceWords                 int
	CER, WER, ExtractSeconds                                                       float64
}

type hebrewSmokeReport struct {
	Scope, GrantID, FixtureManifestSHA256, ProfileID, ProfileIdentity, ExtractionVersion string
	RuntimePaths                                                                         RuntimePaths
	StartedUTC, FinishedUTC                                                              time.Time
	Rows                                                                                 []hebrewSmokeRow
	Diagnostics                                                                          []hebrewSmokeDiagnostic
	DiagnosticsOverflow                                                                  bool
	LaunchFailure                                                                        *hebrewSmokeLaunchFailure
	CleanupAcknowledged                                                                  bool
	Limits                                                                               string
	Interpretation                                                                       string
}

type hebrewSmokeLaunchFailure struct {
	Stage      string
	ErrnoKnown bool
	Errno      uint32
}

// Test-only observations of the REAL provider. No paths, argv, stderr, payload,
// replacement leases, or error wrapping. Holding the verified preflight FDs
// prevents their inode identities being reused while these fixed roles run.
type hebrewSmokeDiagnostic struct {
	Role, Stage, Outcome, Cause string
	LeaseReturned               bool
	ErrnoKnown                  bool
	Errno                       uint32
	Ordinal                     uint8
}

type hebrewSmokeFDIdentity struct{ device, inode uint64 }

type hebrewSmokeProvider struct {
	real                                                                  workerlife.ExecutableProvider
	mu                                                                    sync.Mutex
	roles                                                                 map[hebrewSmokeFDIdentity]string
	events                                                                []hebrewSmokeDiagnostic
	overflow                                                              bool
	decodedAdmissions, physicalDecoded, instanceGuards, modelOpens, paths uint8
}

// First-clip phase counters observe real authority without recording request
// identifiers, source bytes, paths, or arbitrary error strings.
func (p *hebrewSmokeProvider) ordinal(counter *uint8) uint8 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if *counter < 3 {
		*counter++
	}
	return *counter
}

type hebrewSmokeMediaStore struct {
	*archive.Store
	diagnostics *hebrewSmokeProvider
}

func (s *hebrewSmokeMediaStore) AdmitMedia(ctx context.Context, request archive.MediaAdmissionRequest) (archive.MediaReservation, error) {
	ordinal := uint8(0)
	if request.Budget == archive.BudgetDecoded {
		ordinal = s.diagnostics.ordinal(&s.diagnostics.decodedAdmissions)
	}
	reservation, err := s.Store.AdmitMedia(ctx, request)
	if ordinal > 0 && ordinal <= 2 {
		cause := hebrewSmokeCause(err)
		outcome := "completed"
		if err != nil {
			outcome = "failed"
		} else if reservation.ID == "" || reservation.ArtifactID == "" || reservation.LeaseID == "" || reservation.MediaID != request.MediaID || reservation.Budget != request.Budget || reservation.MaximumBytes != request.MaximumBytes || !sameSource(reservation.Source, request.Source) {
			cause, outcome = "reservation_invalid", "invalid"
		}
		s.diagnostics.record(hebrewSmokeDiagnostic{Role: "decoded", Stage: "repo_admit", Outcome: outcome, Cause: cause, Ordinal: ordinal})
	}
	return reservation, err
}

type hebrewSmokeAdmission struct {
	diskspace.Admission
	diagnostics *hebrewSmokeProvider
}

func (a *hebrewSmokeAdmission) Reserve(ctx context.Context, request diskspace.Request) (diskspace.Lease, error) {
	ordinal := uint8(0)
	if request.Class == diskspace.Decoded {
		ordinal = a.diagnostics.ordinal(&a.diagnostics.physicalDecoded)
	}
	lease, err := a.Admission.Reserve(ctx, request)
	if ordinal > 0 && ordinal <= 2 {
		outcome := "completed"
		if err != nil || lease == nil {
			outcome = "failed"
		}
		a.diagnostics.record(hebrewSmokeDiagnostic{Role: "decoded", Stage: "physical_admit", Outcome: outcome, Cause: hebrewSmokeCause(err), LeaseReturned: lease != nil, Ordinal: ordinal})
	}
	return lease, err
}

type hebrewSmokeInstanceGuard struct {
	InstanceGuard
	diagnostics *hebrewSmokeProvider
}

func (g *hebrewSmokeInstanceGuard) WorkerGuard() (*os.File, error) {
	ordinal := g.diagnostics.ordinal(&g.diagnostics.instanceGuards)
	file, err := g.InstanceGuard.WorkerGuard()
	if ordinal <= 2 {
		outcome := "completed"
		if err != nil || file == nil {
			outcome = "failed"
		}
		g.diagnostics.record(hebrewSmokeDiagnostic{Role: "instance", Stage: "instance_guard", Outcome: outcome, Cause: hebrewSmokeCause(err), LeaseReturned: file != nil, Ordinal: ordinal})
	}
	return file, err
}

type hebrewSmokeExecutableLease struct {
	workerlife.ExecutableLease
	diagnostics *hebrewSmokeProvider
	role        string
}

func (l *hebrewSmokeExecutableLease) Path() string {
	path := l.ExecutableLease.Path()
	ordinal := l.diagnostics.ordinal(&l.diagnostics.paths)
	if ordinal <= 2 {
		outcome, cause := "completed", "none"
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			outcome, cause = "invalid", "executable_path_invalid"
		}
		l.diagnostics.record(hebrewSmokeDiagnostic{Role: l.role, Stage: "executable_path", Outcome: outcome, Cause: cause, Ordinal: ordinal})
	}
	return path
}

func hebrewSmokeCause(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, workerlife.ErrCleanup):
		return "worker_cleanup"
	case errors.Is(err, workerlife.ErrProtocol):
		return "worker_protocol"
	case errors.Is(err, workerlife.ErrLimit):
		return "worker_limit"
	case errors.Is(err, workerlife.ErrUnavailable):
		return "worker_unavailable"
	case errors.Is(err, diskspace.ErrInsufficientSpace):
		return "disk_budget"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case errors.Is(err, ErrUnavailable):
		return "media_unavailable"
	default:
		return "other"
	}
}

func (p *hebrewSmokeProvider) record(event hebrewSmokeDiagnostic) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) == 64 {
		p.overflow = true
		return
	}
	p.events = append(p.events, event)
}

func (p *hebrewSmokeProvider) snapshot() ([]hebrewSmokeDiagnostic, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]hebrewSmokeDiagnostic(nil), p.events...), p.overflow
}

func (p *hebrewSmokeProvider) bind(role string, f *os.File) error {
	if f == nil {
		return ErrUnavailable
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	p.roles[hebrewSmokeFDIdentity{device: uint64(uint32(st.Dev)), inode: st.Ino}] = role
	return nil
}

func (p *hebrewSmokeProvider) Prepare(ctx context.Context, f *os.File, maximum int64) (workerlife.ExecutableLease, error) {
	role := "unknown"
	var st unix.Stat_t
	if f != nil && unix.Fstat(int(f.Fd()), &st) == nil {
		if value := p.roles[hebrewSmokeFDIdentity{device: uint64(uint32(st.Dev)), inode: st.Ino}]; value != "" {
			role = value
		}
	}
	p.record(hebrewSmokeDiagnostic{Role: role, Stage: "provider_prepare", Outcome: "started", Cause: "none"})
	lease, err := p.real.Prepare(ctx, f, maximum)
	outcome := "completed"
	if err != nil {
		outcome = "failed"
	}
	p.record(hebrewSmokeDiagnostic{Role: role, Stage: "provider_prepare", Outcome: outcome, Cause: hebrewSmokeCause(err), LeaseReturned: !nilExecutableAuthority(lease)})
	// Partial leases and the original error identity belong to existing native
	// ownership. This observer neither closes them nor changes loading authority.
	if nilExecutableAuthority(lease) {
		return lease, err
	}
	return &hebrewSmokeExecutableLease{ExecutableLease: lease, diagnostics: p, role: role}, err
}

// Transparent test-only model authority observer. Every method delegates to
// the genuine public Acquire lease; returned descriptors/errors/ownership are
// unchanged, and no descriptor is closed by this observer.
type hebrewSmokeModelLease struct {
	MediaModelLease
	diagnostics *hebrewSmokeProvider
}

func (l *hebrewSmokeModelLease) Open(component string) (*os.File, error) {
	ordinal := l.diagnostics.ordinal(&l.diagnostics.modelOpens)
	file, err := l.MediaModelLease.Open(component)
	if ordinal == 1 {
		outcome := "completed"
		if err != nil || file == nil {
			outcome = "failed"
		}
		l.diagnostics.record(hebrewSmokeDiagnostic{Role: "model", Stage: "model_open", Outcome: outcome, Cause: hebrewSmokeCause(err), LeaseReturned: file != nil, Ordinal: ordinal})
	}
	return file, err
}

func (l *hebrewSmokeModelLease) WorkerGuard() (*os.File, error) {
	l.diagnostics.record(hebrewSmokeDiagnostic{Role: "model", Stage: "model_worker_guard", Outcome: "started", Cause: "none"})
	file, err := l.MediaModelLease.WorkerGuard()
	cause := hebrewSmokeCause(err)
	if errors.Is(err, modelstore.ErrLeaseClosed) {
		cause = "model_lease_closed"
	}
	if errors.Is(err, modelstore.ErrUnsafeFile) {
		cause = "model_lease_unsafe"
	}
	outcome := "completed"
	if err != nil || file == nil {
		outcome = "failed"
	}
	event := hebrewSmokeDiagnostic{Role: "model", Stage: "model_worker_guard", Outcome: outcome, Cause: cause, LeaseReturned: file != nil}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 && uint64(errno) <= uint64(^uint32(0)) {
		event.ErrnoKnown, event.Errno = true, uint32(errno)
	}
	l.diagnostics.record(event)
	return file, err
}

// Same reviewed private-directory and runtime verification used by production;
// the fixed fixture handles remain anchored until synchronous receipt completes.
func hebrewSmokeBindingRead(t *testing.T) hebrewSmokeBinding {
	t.Helper()
	if os.Getenv("WA_INDEXER_HEBREW_SMOKE_ARMED") != hebrewSmokeArm {
		t.Fatal("UNARMED: ROOT-approved binding and separate actual-runtime grant required; no payload or child accessed")
	}
	name, digest := os.Getenv("WA_INDEXER_HEBREW_SMOKE_BINDING"), os.Getenv("WA_INDEXER_HEBREW_SMOKE_BINDING_SHA256")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || !validDigest(digest) {
		t.Fatal("invalid explicit smoke binding")
	}
	data, err := hebrewSmokeReadBinding(name, digest, &hebrewSmokeUncertainBindingDirectory)
	if err != nil {
		t.Fatal("binding integrity/first-close acknowledgement failed", err)
	}
	var b hebrewSmokeBinding
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("invalid bounded smoke binding")
	}
	if b.Schema != "wa-indexer-hebrew-smoke3-v1" || b.Scope != "SMOKE3_ONLY_NOT_HEBREW_QUALIFIED" || len(b.GrantID) < 1 || len(b.GrantID) > 128 || strings.ContainsAny(b.GrantID, "\x00\r\n") || b.FixtureManifestSHA256 != hebrewSmokeManifest || !validDigest(b.ExpectedModelIdentity) || b.RSSMaximumBytes != 4<<30 {
		t.Fatal("UNARMED: incomplete exact smoke authority")
	}
	for _, path := range []string{b.SandboxRoot, b.ModelDirectory, b.FixtureDirectory, b.OutputDirectory, b.Helper.Path, b.Decoder.Path, b.Speech.Path} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			t.Fatal("UNARMED: missing canonical trusted path")
		}
	}
	if b.Helper.Bytes < 1 || b.Helper.Bytes > 64<<20 || !validDigest(b.Helper.SHA256) || b.Decoder.Bytes != 1850040 || b.Decoder.SHA256 != "9131343e4b34ba6ee74a4c8ce0b3450d4ec1718ddbfdc3831f2aa98a9530e0d0" || b.Speech.Bytes != 2539288 || b.Speech.SHA256 != "fed03904a463cf43482ab5ee143192f133b93e67329e5403a668a3eb49d8f3bd" {
		t.Fatal("UNARMED: sealed V8 runtimes and genuine helper required")
	}
	return b
}

// No binding can authorize later writes/children until both owned reader first
// closes acknowledge. A failed first Close is never retried numerically; the
// test aborts before it acquires any installation/native ownership.
func hebrewSmokeReadBinding(name, digest string, retained **os.File) ([]byte, error) {
	if *retained != nil {
		return nil, errors.New("prior binding close remains uncertain")
	}
	dir, err := openDirectory(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	f, err := dir.open(filepath.Base(name), unix.O_RDONLY)
	if err != nil {
		return nil, errors.Join(err, dir.file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 16385))
	sum := sha256.Sum256(data)
	identityErr := dir.same(filepath.Base(name), f)
	if len(data) > 16384 || hex.EncodeToString(sum[:]) != digest {
		readErr = errors.Join(readErr, errors.New("bounded binding hash/extent mismatch"))
	}
	if closeErr := f.Close(); closeErr != nil {
		// Keep this directory strongly rooted instead of advancing its close
		// or letting GC close it. Abort before installation/native ownership.
		*retained = dir.file
		return nil, errors.Join(readErr, identityErr, closeErr)
	}
	if closeErr := dir.file.Close(); closeErr != nil {
		return nil, errors.Join(readErr, identityErr, closeErr)
	}
	return data, errors.Join(readErr, identityErr)
}

func hebrewSmokeEmpty(dir *directory) error {
	entries, err := dir.file.ReadDir(1)
	if err != io.EOF || len(entries) != 0 {
		return errors.New("smoke sandbox must be a new empty owned directory")
	}
	_, err = dir.file.Seek(0, io.SeekStart)
	return err
}

// Renewal outlives job cancellation, through physical reader cleanup and exact
// durable input-pin acknowledgement. It is joined before archive close.
type hebrewSmokeRenewal struct {
	stop context.CancelFunc
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func hebrewSmokeRenew(repo *archive.Store, lease archive.JobLease, cancelJob context.CancelFunc) *hebrewSmokeRenewal {
	ctx, stop := context.WithCancel(context.Background())
	r := &hebrewSmokeRenewal{stop: stop, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				call, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, err := repo.RenewExtraction(call, lease, 2*time.Minute)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					r.mu.Lock()
					r.err = err
					r.mu.Unlock()
					cancelJob()
					return
				}
			}
		}
	}()
	return r
}

func (r *hebrewSmokeRenewal) close() error {
	r.stop()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *hebrewSmokeRenewal) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Retains all upstream owners after uncertain native, physical, or durable
// cleanup. No deadline, failed response, t.TempDir deletion, or process signal
// invents an acknowledgement. The outer FAMILY runner owns terminal proof.
type hebrewSmokeOwner struct {
	repo           *archive.Store
	store          *Store
	models         *modelstore.Store
	model          *modelstore.Lease
	namespace      *workerlife.ExecutableNamespace
	extractor      *Extractor
	scheduler      *compute.ThreadBudgetScheduler
	instance       *appdata.InstanceLock
	broker         *diskspace.Controller
	metadata       diskspace.Lease
	helper         *os.File
	runtimeChecks  []*os.File
	download       *Download
	original       *Original
	claim          *archive.ExtractionWork
	renewal        *hebrewSmokeRenewal
	cancelJob      context.CancelFunc
	uncertainClose error
	device         uint64
}

func (o *hebrewSmokeOwner) close() error {
	// A failed first Close cannot be retried safely: the integer descriptor may
	// already have been reused. Keep all upstream authority until outer process
	// death/positive FAMILY absence proves every descriptor owner is stopped.
	if o.uncertainClose != nil {
		return ErrCleanup
	}
	if o.cancelJob != nil {
		o.cancelJob()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if o.extractor != nil {
		if err := o.extractor.Close(ctx); err != nil {
			return err
		}
		o.extractor = nil
	}
	if o.original != nil {
		if err := o.original.Close(); err != nil {
			return err
		}
		o.original = nil
	}
	if o.claim != nil {
		if err := o.repo.RetryExtraction(ctx, archive.JobRetry{Lease: o.claim.Lease, Code: "cancelled", After: time.Second}); err != nil {
			return err
		}
		o.claim = nil
	}
	if o.renewal != nil {
		_ = o.renewal.close()
		o.renewal = nil
	}
	if o.download != nil {
		if err := o.download.Close(); err != nil {
			return err
		}
		o.download = nil
	}
	if o.store != nil {
		if err := o.store.Close(); err != nil {
			return err
		}
		o.store = nil
	}
	if o.namespace != nil {
		if err := o.namespace.Close(); err != nil {
			return err
		}
		o.namespace = nil
	}
	for len(o.runtimeChecks) > 0 {
		if err := o.runtimeChecks[0].Close(); err != nil {
			o.uncertainClose = err
			return err
		}
		o.runtimeChecks = o.runtimeChecks[1:]
	}
	if o.model != nil {
		if err := o.model.Close(); err != nil {
			return err
		}
		o.model = nil
	}
	if o.models != nil {
		if err := o.models.Close(); err != nil {
			return err
		}
		o.models = nil
	}
	if o.helper != nil {
		if err := o.helper.Close(); err != nil {
			return err
		}
		o.helper = nil
	}
	if o.scheduler != nil {
		o.scheduler.Close()
		o.scheduler = nil
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
	return nil
}

// Called only after the bounded report writer has fsynced. A failed report
// leaves this allowance and instance authority retained for explicit recovery.
func (o *hebrewSmokeOwner) closeFinal() error {
	if o.metadata != nil {
		if err := o.metadata.Release(); err != nil {
			return err
		}
		o.metadata = nil
	}
	if o.broker != nil {
		if err := o.broker.Close(); err != nil {
			return err
		}
		o.broker = nil
	}
	if o.instance != nil {
		if err := o.instance.Close(); err != nil {
			return err
		}
		o.instance = nil
	}
	return nil
}

func TestHebrewNativeExtractPrimarySmoke(t *testing.T) {
	b := hebrewSmokeBindingRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	owner := &hebrewSmokeOwner{}
	diagnostics := &hebrewSmokeProvider{roles: make(map[hebrewSmokeFDIdentity]string)}
	report := hebrewSmokeReport{Scope: b.Scope, GrantID: b.GrantID, FixtureManifestSHA256: hebrewSmokeManifest, ProfileID: hebrewSmokeProfile, StartedUTC: time.Now().UTC(), Limits: "3 serial clips; CPU2; batch8m; child2m; cleanup30s; generated UTF-8 <=64KiB/clip; no encoder", Interpretation: "SMOKE3_ONLY: three male read-speech clips; no Hebrew quality/default acceptance, no macOS14 execution claim, no model selection change; offsets are slices, not words; RSS is sampled FAMILY RSS in outer runner; CleanupAcknowledged covers native/input/media/model/archive cleanup, and final metadata/broker/instance closure additionally requires test PASS"}
	t.Cleanup(func() {
		report.Diagnostics, report.DiagnosticsOverflow = diagnostics.snapshot()
		if data, err := json.Marshal(report.Diagnostics); err == nil {
			t.Log("body-free real-provider diagnostics", string(data), "overflow", report.DiagnosticsOverflow)
		}
		if err := owner.close(); err != nil {
			t.Error("cleanup uncertain; upstream owners/files/charges retained", err)
			return
		}
		report.CleanupAcknowledged = true
		report.FinishedUTC = time.Now().UTC()
		if err := hebrewSmokeWriteReport(b.OutputDirectory, report, owner.device); err != nil {
			t.Error("bounded smoke report persistence failed", err)
			return
		}
		if err := owner.closeFinal(); err != nil {
			t.Error("final smoke owner close uncertain", err)
		}
	})
	// A fresh empty sandbox is mandatory. The separately installed model tree is
	// not copied into it and is verified on the very same broker filesystem.
	sandbox, err := openDirectory(b.SandboxRoot)
	if err != nil {
		t.Fatal("unsafe smoke sandbox")
	}
	if err := hebrewSmokeEmpty(sandbox); err != nil {
		sandbox.file.Close()
		t.Fatal(err)
	}
	if err := sandbox.file.Close(); err != nil {
		t.Fatal(err)
	}
	owner.broker, err = diskspace.New(b.SandboxRoot, diskspace.Options{HeadroomBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	device, err := owner.broker.Snapshot()
	if err != nil || device.ReservedBytes != 0 {
		t.Fatal("shared broker must start with no outstanding reservations")
	}
	owner.device = device.Device
	if err := hebrewSmokeCheckOutput(owner, b.OutputDirectory); err != nil {
		t.Fatal("smoke output must be private on the broker filesystem", err)
	}
	// Covers finite sandbox/SQLite/control metadata growth; runtime copies and
	// media bytes take their own exact leases on this same controller. This is
	// an admission allowance, not a claim of a SQLite aggregate hard size cap.
	owner.metadata, err = owner.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(b.SandboxRoot)
	if err != nil {
		t.Fatal(err)
	}
	owner.instance, err = appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	owner.repo, err = archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	account, err := owner.repo.BoundAccount(ctx)
	if err != nil || account != "" {
		t.Fatal("smoke archive must have no account")
	}
	policy, err := owner.repo.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy, err = owner.repo.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	owner.store, err = Open(paths, &hebrewSmokeMediaStore{Store: owner.repo, diagnostics: diagnostics}, Options{Admission: &hebrewSmokeAdmission{Admission: owner.broker, diagnostics: diagnostics}, ExpectedDevice: device.Device, VoiceMaximumBytes: 1 << 20, OnTransition: func(event Transition) {
		if event.Phase == "decoded_created" {
			diagnostics.mu.Lock()
			ordinal := diagnostics.decodedAdmissions
			diagnostics.mu.Unlock()
			if ordinal > 0 && ordinal <= 2 {
				diagnostics.record(hebrewSmokeDiagnostic{Role: "decoded", Stage: "file_created", Outcome: "completed", Cause: "none", Ordinal: ordinal})
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := owner.store.Reconcile(ctx, 16)
	if err != nil || reconciled.Active != 0 || reconciled.Cleaned != 0 {
		t.Fatal("fresh media ownership reconciliation failed")
	}
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(ctx context.Context, maximum int64) (modelstore.SpaceReservation, error) {
		return owner.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: maximum})
	}
	owner.models, err = modelstore.Open(b.ModelDirectory, catalog, modelstore.Options{ExpectedDevice: device.Device, ReserveSpace: reserve, HeadroomBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	owner.model, err = owner.models.Acquire(ctx, hebrewSmokeProfile)
	if err != nil {
		t.Fatal("genuine separately installed ivrit profile required", err)
	}
	installed := owner.model.Profile()
	if installed.Identity != b.ExpectedModelIdentity {
		t.Fatal("verified installed profile differs from ROOT binding")
	}
	runtimes := RuntimePaths{Decoder: b.Decoder, Speech: b.Speech}
	version, err := NewExtractionVersion(archive.JobTranscription, installed, runtimes, SpeechHebrew)
	if err != nil {
		t.Fatal(err)
	}
	report.ProfileIdentity, report.ExtractionVersion, report.RuntimePaths = installed.Identity, version.ID, runtimes
	owner.helper, err = openRuntime(ctx, b.Helper)
	if err != nil {
		t.Fatal("genuine pinned thin-ARM64 system-only helper required", err)
	}
	if err := diagnostics.bind("holder", owner.helper); err != nil {
		t.Fatal("holder diagnostic FD identity unavailable", err)
	}
	for _, runtime := range []struct {
		role string
		spec RuntimeSpec
	}{{"decoder", b.Decoder}, {"speech", b.Speech}} {
		diagnostics.record(hebrewSmokeDiagnostic{Role: runtime.role, Stage: "runtime_preflight", Outcome: "started", Cause: "none"})
		f, runtimeErr := openRuntime(ctx, runtime.spec)
		if f != nil {
			owner.runtimeChecks = append(owner.runtimeChecks, f)
		}
		if runtimeErr == nil {
			runtimeErr = diagnostics.bind(runtime.role, f)
		}
		outcome := "completed"
		if runtimeErr != nil {
			outcome = "failed"
		}
		diagnostics.record(hebrewSmokeDiagnostic{Role: runtime.role, Stage: "runtime_preflight", Outcome: outcome, Cause: hebrewSmokeCause(runtimeErr)})
		if runtimeErr != nil {
			t.Fatal("fixed runtime preflight failed before real provider", runtime.role, hebrewSmokeCause(runtimeErr))
		}
	}
	// Borrow the Store's already validated same-device working-directory anchor;
	// a later pathname reopen must not choose a replacement mount for growth.
	working := owner.store.working
	if err := checkPrivateFD(working.file, true); err != nil {
		t.Fatal(err)
	}
	if err := checkFilesystem(working.file, device.Device); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkdirat(int(working.file.Fd()), "hebrew-smoke-exec", 0700); err != nil {
		t.Fatal(err)
	}
	if err := working.file.Sync(); err != nil {
		t.Fatal(err)
	}
	namespaceDir, err := working.subdirectory("hebrew-smoke-exec")
	if err != nil {
		t.Fatal(err)
	}
	owner.namespace, err = workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: namespaceDir.file, Path: namespaceDir.path, Admit: func(ctx context.Context, maximum int64) (workerlife.GrowthLease, error) {
		return owner.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: maximum})
	}})
	closeErr := namespaceDir.file.Close()
	if closeErr != nil {
		owner.uncertainClose = closeErr
	}
	if err != nil || closeErr != nil {
		t.Fatal("namespace preparation/first descriptor close failed", errors.Join(err, closeErr))
	}
	if err := owner.namespace.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	diagnostics.real = owner.namespace
	owner.scheduler, err = compute.NewThreadBudgetScheduler(2)
	if err != nil {
		t.Fatal(err)
	}
	owner.extractor, err = NewExtractor(ExtractorOptions{Store: owner.store, Scheduler: owner.scheduler, InstanceGuard: &hebrewSmokeInstanceGuard{InstanceGuard: owner.instance, diagnostics: diagnostics}, WorkerLifetime: workerlife.Config{ExecutableFile: owner.helper, ExecutableProvider: diagnostics, ReadyTimeout: 5 * time.Second}, Acquire: func(ctx context.Context, id string) (MediaModelLease, error) {
		if id != hebrewSmokeProfile {
			return nil, ErrModelMissing
		}
		diagnostics.record(hebrewSmokeDiagnostic{Role: "model", Stage: "model_acquire", Outcome: "started", Cause: "none"})
		lease, acquireErr := owner.models.Acquire(ctx, id)
		outcome := "completed"
		if acquireErr != nil || lease == nil {
			outcome = "failed"
		}
		diagnostics.record(hebrewSmokeDiagnostic{Role: "model", Stage: "model_acquire", Outcome: outcome, Cause: hebrewSmokeCause(acquireErr), LeaseReturned: lease != nil})
		if lease == nil {
			return nil, acquireErr
		}
		return &hebrewSmokeModelLease{MediaModelLease: lease, diagnostics: diagnostics}, acquireErr
	}, Runtimes: runtimes, Language: SpeechHebrew, ExtractionVersions: []archive.ExtractionVersion{version}, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := openDirectory(b.FixtureDirectory)
	if err != nil {
		t.Fatal("unsafe fixed fixture directory")
	}
	defer fixtures.file.Close()
	manifest, err := fixtures.open("manifest.json", unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(manifest, 4786))
	if err != nil || n != 4785 || hex.EncodeToString(hash.Sum(nil)) != hebrewSmokeManifest || fixtures.same("manifest.json", manifest) != nil {
		manifest.Close()
		t.Fatal("fixed fixture manifest differs")
	}
	if err := manifest.Close(); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range hebrewSmokeFixtures {
		if !t.Run(strings.TrimSuffix(fixture.File, ".wav"), func(t *testing.T) {
			report.Rows = append(report.Rows, hebrewSmokeRunClip(t, ctx, owner, fixtures, fixture, policy.Generation, version, &report))
		}) {
			break
		}
	}
	if len(report.Rows) != 3 {
		t.Fatal("all three genuine fixed clips must complete")
	}
}

func hebrewSmokeRunClip(t *testing.T, ctx context.Context, o *hebrewSmokeOwner, fixtures *directory, fixture hebrewSmokeFixture, generation archive.Generation, version archive.ExtractionVersion, report *hebrewSmokeReport) hebrewSmokeRow {
	t.Helper()
	f, err := fixtures.open(fixture.File, unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != fixture.Bytes {
		t.Fatal("fixed fixture extent differs")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.NewSectionReader(f, 0, fixture.Bytes+1))
	if err != nil || n != fixture.Bytes || hex.EncodeToString(hash.Sum(nil)) != fixture.SHA256 || fixtures.same(fixture.File, f) != nil {
		t.Fatal("fixed PCM fixture integrity failed")
	}
	sha, err := hex.DecodeString(fixture.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	var protocolSHA [32]byte
	copy(protocolSHA[:], sha)
	event := archive.SourceEvent{Key: archive.SourceKey{Account: "public-fleurs-hebrew-smoke3-v1", Chat: "fixed-public-fixtures", Sender: "public-human-label", Message: fixture.File}, Kind: archive.EventOriginal, OriginalSentAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), OriginalDateKnown: true, BodyKind: archive.MessageVoice, Media: []archive.MediaReference{{Kind: "voice", ProtocolReference: []byte("wa-indexer/public-fleurs-smoke3/v1/" + fixture.File), MimeType: "audio/wav", DeclaredBytes: fixture.Bytes, Availability: "unavailable"}}}
	applied, err := o.repo.ApplySourceEvent(ctx, event, generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatal("public fixture source publication failed", err)
	}
	o.download, err = o.store.AdmitDownload(ctx, DownloadRequest{Key: event.Key, Version: applied.Version, MediaID: applied.MediaIDs[0], Kind: archive.MessageVoice, DeclaredBytes: fixture.Bytes, CiphertextMaximum: 16*(fixture.Bytes/16+1) + 10, ProtocolSHA256: protocolSHA})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(o.download.File(), io.NewSectionReader(f, 0, fixture.Bytes)); err != nil || n != fixture.Bytes {
		t.Fatal("bounded same-FD fixture receipt write failed", err)
	}
	if _, err := o.download.File().Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// Reader is the admitted anchored file just written, not a second fixture
	// read or an unrelated bytes.Reader. Production Complete verifies it again.
	if err := o.download.Complete(ctx, ingestion.VerifiedMedia{Key: event.Key, Version: applied.Version, Kind: archive.MessageVoice, SHA256: protocolSHA, Bytes: fixture.Bytes, Reader: io.LimitReader(o.download.File(), fixture.Bytes)}); err != nil {
		t.Fatal(err)
	}
	if err := o.download.Close(); err != nil {
		t.Fatal(err)
	}
	o.download = nil
	work, err := o.repo.ClaimExtraction(ctx, archive.ExtractionClaim{Account: event.Key.Account, Version: version, TTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	o.claim = &work
	if work.MediaID != applied.MediaIDs[0] {
		t.Fatal("claimed fixture association differs")
	}
	current, err := o.repo.LoadExtractionInput(ctx, work.Lease)
	if err != nil || current.Source != work.Source || current.MediaID != work.MediaID || current.ArtifactID != work.ArtifactID || current.OriginalSHA256 != fixture.SHA256 || current.ContentHash != work.ContentHash {
		t.Fatal("current public fixture extraction input differs", err)
	}
	o.original, err = o.store.OpenOriginal(ctx, archive.ReadScope{Account: event.Key.Account, Principal: "isolated-smoke-owner", AllChats: true, PolicyGeneration: generation}, current.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	meta := o.original.Metadata()
	if meta.Source != current.Source || meta.MediaID != current.MediaID || meta.ArtifactID != current.ArtifactID || meta.SHA256 != current.OriginalSHA256 || meta.Bytes != fixture.Bytes || meta.Format != "wav" {
		t.Fatal("retained original is not the claimed fixed PCM")
	}
	jobCtx, cancelJob := context.WithCancel(ctx)
	o.cancelJob = cancelJob
	o.renewal = hebrewSmokeRenew(o.repo, current.Lease, cancelJob)
	started := time.Now()
	result, err := o.extractor.ExtractPrimary(jobCtx, current, o.original)
	elapsed := time.Since(started).Seconds()
	cancelJob()
	if err != nil {
		var launch workerlife.LaunchFailure
		if errors.As(err, &launch) {
			report.LaunchFailure = &hebrewSmokeLaunchFailure{Stage: launch.Stage(), ErrnoKnown: launch.ErrnoKnown(), Errno: launch.Errno()}
		}
		t.Fatal("actual fixed-he ExtractPrimary failed; no transcript manufactured", err)
	}
	if result.Lease != current.Lease || result.ContentHash != current.ContentHash || result.Outcome != archive.ExtractionText || len(result.Documents) != 1 {
		t.Fatal("human-speech clip requires one genuine transcript root")
	}
	doc := result.Documents[0]
	p := doc.Provenance
	if doc.Kind != archive.DocumentTranscript || len(doc.Sources) != 1 || doc.Sources[0] != current.Source || len(doc.Spans) != 0 || doc.PassageTokens != 0 || !utf8.ValidString(doc.Text) || len(doc.Text) > 64<<10 || strings.TrimSpace(doc.Text) == "" || strings.ContainsRune(doc.Text, 0) || p.Kind != doc.Kind || p.MediaID != current.MediaID || p.ProfileID != current.Version.ProfileID || p.ExtractorVersion != current.Version.ExtractorVersion || p.PromptVersion != current.Version.PromptVersion || p.OffsetKind != "slice" || p.StartMS != 0 || p.EndMS != fixture.EndMS {
		t.Fatal("actual transcript root provenance/output/slice bounds differ")
	}
	if err := o.original.Close(); err != nil {
		t.Fatal(err)
	}
	o.original = nil
	if err := o.renewal.failure(); err != nil {
		t.Fatal("extraction lease renewal failed", err)
	}
	accepted, err := o.repo.CommitExtraction(ctx, result)
	if err != nil || !accepted {
		t.Fatal("exact real transcript commit uncertain; capability retained", err)
	}
	o.claim = nil
	// A concurrent renew may observe the acknowledged completed job as no-work.
	// The successful exact commit is the durable pin-release authority; stop
	// and join after that acknowledgement, not before it.
	_ = o.renewal.close()
	o.renewal = nil
	o.cancelJob = nil
	if err := fixtures.same(fixture.File, f); err != nil {
		t.Fatal("fixed fixture replaced during extraction")
	}
	hash.Reset()
	n, err = io.Copy(hash, io.NewSectionReader(f, 0, fixture.Bytes+1))
	if err != nil || n != fixture.Bytes || hex.EncodeToString(hash.Sum(nil)) != fixture.SHA256 || fixtures.same(fixture.File, f) != nil {
		t.Fatal("fixed PCM fixture changed during extraction")
	}
	label, transcript := hebrewSmokeNormalize(fixture.HumanLabel), hebrewSmokeNormalize(doc.Text)
	cEdits := hebrewSmokeDistance([]rune(label), []rune(transcript))
	lw, tw := strings.Fields(label), strings.Fields(transcript)
	wEdits := hebrewSmokeDistance(lw, tw)
	return hebrewSmokeRow{Fixture: fixture.File, SHA256: fixture.SHA256, HumanLabel: fixture.HumanLabel, Transcript: doc.Text, NormalizedLabel: label, NormalizedTranscript: transcript, Bytes: fixture.Bytes, StartMS: p.StartMS, EndMS: p.EndMS, CharacterEdits: cEdits, ReferenceCharacters: utf8.RuneCountInString(label), WordEdits: wEdits, ReferenceWords: len(lw), CER: float64(cEdits) / float64(utf8.RuneCountInString(label)), WER: float64(wEdits) / float64(len(lw)), ExtractSeconds: elapsed}
}

// Scoring is explicit and bounded: Unicode lowercase; remove punctuation;
// collapse whitespace; keep letters/digits/marks. It preserves the raw human
// typo and label separately, does not correct transcript text, and imposes no
// quality pass threshold. Levenshtein uses O(reference length) memory.
func hebrewSmokeNormalize(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || unicode.IsSpace(r) {
			out.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func hebrewSmokeDistance[T comparable](reference, actual []T) int {
	row := make([]int, len(reference)+1)
	for i := range row {
		row[i] = i
	}
	for i, token := range actual {
		previous := row[0]
		row[0] = i + 1
		for j, wanted := range reference {
			old, cost := row[j+1], 1
			if wanted == token {
				cost = 0
			}
			row[j+1] = min(row[j+1]+1, row[j]+1, previous+cost)
			previous = old
		}
	}
	return row[len(reference)]
}

func hebrewSmokeCheckOutput(owner *hebrewSmokeOwner, path string) error {
	dir, err := openDirectory(path)
	if err != nil {
		return err
	}
	deviceErr := checkFilesystem(dir.file, owner.device)
	closeErr := dir.file.Close()
	if closeErr != nil {
		owner.uncertainClose = closeErr
	}
	return errors.Join(deviceErr, closeErr)
}

func hebrewSmokeWriteReport(path string, report hebrewSmokeReport, device uint64) error {
	return hebrewSmokeWriteJSON(path, "hebrew-smoke3.json", report, 512<<10, device)
}

func hebrewSmokeWriteJSON(path, name string, report any, maximum int, device uint64) (err error) {
	dir, err := openDirectory(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.file.Close()) }()
	if device == 0 {
		return errors.New("missing report filesystem authority")
	}
	if err := checkFilesystem(dir.file, device); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	data = append(data, '\n')
	if err != nil || len(data) > maximum {
		return fmt.Errorf("smoke report exceeds bound")
	}
	f, err := dir.open(name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return dir.file.Sync()
}

const hebrewHolderDecoderProbeArm = "ROOT_APPROVED_HEBREW_HOLDER_DECODER_PROBE_ONCE"

type hebrewHolderDecoderProbeBinding struct {
	Schema, GrantID, Scope, SandboxRoot, OutputDirectory string
	RSSMaximumBytes                                      int64
	Helper, Decoder                                      RuntimeSpec
}

type hebrewHolderDecoderProbeStep struct {
	Stage, StartCause, WaitCause, CleanupCause  string
	Attempted, NativeStarted, WaitAcknowledged  bool
	LaunchFailure                               *hebrewSmokeLaunchFailure
	ExitKnown, SignalKnown, CleanupAcknowledged bool
	NativeDeadlineFailed                        bool
	ExitCode, Signal                            int
	Seconds                                     float64
}

type hebrewHolderDecoderProbeReport struct {
	Scope, GrantID, Interpretation           string
	StartedUTC, FinishedUTC                  time.Time
	MissingNative, DecoderVersion            hebrewHolderDecoderProbeStep
	Diagnostics                              []hebrewSmokeDiagnostic
	DiagnosticsOverflow, CleanupAcknowledged bool
}

// Failed cleanup retains these owners until the outer controlled process and
// its full FAMILY terminate. No finalizer, pathname deletion, or retry of a
// failed first Close substitutes for that terminal proof.
var hebrewHolderDecoderProbeUncertainBindingDirectory *os.File
var hebrewHolderDecoderProbeUncertainOwner *hebrewSmokeOwner
var hebrewHolderDecoderProbeUncertainLifetime *workerlife.Lease
var hebrewHolderDecoderProbeUncertainExecutable workerlife.ExecutableLease
var hebrewHolderDecoderProbeUncertainCommand *exec.Cmd

func TestHebrewNativeHolderDecoderProbe(t *testing.T) {
	// This separate unarmed gate precedes all binding/runtime reads and children.
	if os.Getenv("WA_INDEXER_HEBREW_PROBE_ARMED") != hebrewHolderDecoderProbeArm {
		t.Fatal("UNARMED: separate ROOT holder/decoder probe grant required; no model, PCM, runtime payload or child accessed")
	}
	if hebrewHolderDecoderProbeUncertainOwner != nil {
		t.Fatal("prior probe cleanup remains uncertain")
	}
	name, digest := os.Getenv("WA_INDEXER_HEBREW_PROBE_BINDING"), os.Getenv("WA_INDEXER_HEBREW_PROBE_BINDING_SHA256")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || !validDigest(digest) {
		t.Fatal("invalid explicit holder/decoder probe binding")
	}
	data, err := hebrewSmokeReadBinding(name, digest, &hebrewHolderDecoderProbeUncertainBindingDirectory)
	if err != nil {
		t.Fatal("probe binding integrity/first-close acknowledgement failed")
	}
	var b hebrewHolderDecoderProbeBinding
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF || b.Schema != "wa-indexer-hebrew-holder-decoder-probe-v1" || b.Scope != "HOLDER_DECODER_PROBE_ONLY_NOT_HEBREW_QUALIFIED" || len(b.GrantID) < 1 || len(b.GrantID) > 128 || strings.ContainsAny(b.GrantID, "\x00\r\n") || b.RSSMaximumBytes != 2<<30 {
		t.Fatal("UNARMED: incomplete exact holder/decoder probe authority")
	}
	for _, path := range []string{b.SandboxRoot, b.OutputDirectory, b.Helper.Path, b.Decoder.Path} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			t.Fatal("UNARMED: canonical ROOT-approved probe paths required")
		}
	}
	if b.Helper.Bytes != 32816370 || b.Helper.SHA256 != "aaf3423407f528f9569fd7409d5fb37a1b04667bbb18dda84b6d347df0699945" || b.Decoder.Bytes != 1850040 || b.Decoder.SHA256 != "9131343e4b34ba6ee74a4c8ce0b3450d4ec1718ddbfdc3831f2aa98a9530e0d0" {
		t.Fatal("UNARMED: sealed actual app and V8 decoder required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	owner := &hebrewSmokeOwner{}
	diagnostics := &hebrewSmokeProvider{roles: make(map[hebrewSmokeFDIdentity]string)}
	report := hebrewHolderDecoderProbeReport{Scope: b.Scope, GrantID: b.GrantID, StartedUTC: time.Now().UTC(), Interpretation: "Fixed real-app holder READY/NOOP probe, then sealed decoder -version only if acknowledged NOOP passes; no model, PCM, speech, encoder or Hebrew qualification; diagnostics contain no output bodies; final owner closure additionally requires test PASS"}
	var lifetime *workerlife.Lease
	var executable workerlife.ExecutableLease
	var command *exec.Cmd
	var cleanupUncertain bool
	t.Cleanup(func() {
		if !cleanupUncertain {
			if err := owner.close(); err != nil {
				cleanupUncertain = true
				t.Error("probe owner cleanup uncertain", hebrewSmokeCause(err))
			}
		}
		if cleanupUncertain {
			hebrewHolderDecoderProbeUncertainOwner = owner
			hebrewHolderDecoderProbeUncertainLifetime = lifetime
			hebrewHolderDecoderProbeUncertainExecutable = executable
			hebrewHolderDecoderProbeUncertainCommand = command
		} else {
			report.CleanupAcknowledged = true
		}
		report.Diagnostics, report.DiagnosticsOverflow = diagnostics.snapshot()
		report.FinishedUTC = time.Now().UTC()
		if encoded, err := json.Marshal(report); err == nil {
			t.Log("body-free holder/decoder probe", string(encoded))
		}
		if owner.device == 0 || owner.metadata == nil {
			return
		}
		if err := hebrewSmokeWriteJSON(b.OutputDirectory, "hebrew-holder-decoder-probe.json", report, 16<<10, owner.device); err != nil {
			hebrewHolderDecoderProbeUncertainOwner = owner
			t.Error("bounded probe report persistence failed")
			return
		}
		if !cleanupUncertain {
			if err := owner.closeFinal(); err != nil {
				hebrewHolderDecoderProbeUncertainOwner = owner
				t.Error("final probe owner close uncertain")
			}
		}
	})
	sandbox, err := openDirectory(b.SandboxRoot)
	if err != nil {
		t.Fatal("unsafe probe sandbox")
	}
	emptyErr := hebrewSmokeEmpty(sandbox)
	closeErr := sandbox.file.Close()
	if closeErr != nil {
		owner.uncertainClose = closeErr
	}
	if emptyErr != nil || closeErr != nil {
		t.Fatal("probe requires fresh private sandbox and first-close acknowledgement")
	}
	owner.broker, err = diskspace.New(b.SandboxRoot, diskspace.Options{HeadroomBytes: 1 << 30})
	if err != nil {
		t.Fatal("probe broker unavailable")
	}
	snapshot, err := owner.broker.Snapshot()
	if err != nil || snapshot.ReservedBytes != 0 {
		t.Fatal("probe broker must start without reservations")
	}
	owner.device = snapshot.Device
	if err := hebrewSmokeCheckOutput(owner, b.OutputDirectory); err != nil {
		t.Fatal("probe output filesystem/private-directory validation failed")
	}
	owner.metadata, err = owner.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 8 << 20})
	if err != nil {
		t.Fatal("probe metadata admission failed")
	}
	paths, err := appdata.Prepare(b.SandboxRoot)
	if err != nil {
		t.Fatal("probe appdata preparation failed")
	}
	owner.instance, err = appdata.Acquire(paths)
	if err != nil {
		t.Fatal("probe instance authority unavailable")
	}
	working, err := openDirectory(paths.WorkingDir)
	if err != nil {
		t.Fatal("probe working anchor unavailable")
	}
	owner.runtimeChecks = append(owner.runtimeChecks, working.file)
	if checkFilesystem(working.file, owner.device) != nil || checkPrivateFD(working.file, true) != nil {
		t.Fatal("probe working filesystem/private anchor differs")
	}
	working.expectedDevice = owner.device
	guard, err := owner.instance.WorkerGuard()
	if guard != nil {
		owner.runtimeChecks = append(owner.runtimeChecks, guard)
	}
	if err != nil || guard == nil {
		t.Fatal("probe instance borrowed guard unavailable")
	}
	guards := []*os.File{guard}
	for _, name := range []string{"probe-original-guard", "probe-pin-guard"} {
		guard, err := working.open(name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
		if guard != nil {
			owner.runtimeChecks = append(owner.runtimeChecks, guard)
		}
		if err != nil || guard == nil || unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil || guard.Sync() != nil {
			t.Fatal("probe empty borrowed guard acquisition failed")
		}
		guards = append(guards, guard)
	}
	if unix.Mkdirat(int(working.file.Fd()), "hebrew-probe-exec", 0700) != nil || working.file.Sync() != nil {
		t.Fatal("probe namespace creation/durability failed")
	}
	namespaceDir, err := working.subdirectory("hebrew-probe-exec")
	if err != nil {
		t.Fatal("probe namespace anchor unavailable")
	}
	owner.namespace, err = workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: namespaceDir.file, Path: namespaceDir.path, Admit: func(ctx context.Context, maximum int64) (workerlife.GrowthLease, error) {
		return owner.broker.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: maximum})
	}})
	closeErr = namespaceDir.file.Close()
	if closeErr != nil {
		owner.uncertainClose = closeErr
	}
	if err != nil || closeErr != nil || owner.namespace.Recover(ctx) != nil {
		t.Fatal("probe namespace preparation/first-close/recovery failed")
	}
	diagnostics.real = owner.namespace
	owner.helper, err = openRuntime(ctx, b.Helper)
	if err != nil || diagnostics.bind("holder", owner.helper) != nil {
		t.Fatal("sealed actual app helper verification failed")
	}
	config := workerlife.Config{ExecutableFile: owner.helper, ExecutableProvider: diagnostics, ReadyTimeout: 5 * time.Second}
	run := func(step *hebrewHolderDecoderProbeStep, cmd *exec.Cmd, expectMissing bool) bool {
		step.Attempted = true
		started := time.Now()
		command = cmd
		cmd.Dir = "/"
		cmd.Stdin = nil
		cmd.Stdout = &limitedOutput{dst: io.Discard, remaining: 64 << 10}
		cmd.Stderr = &limitedOutput{dst: io.Discard, remaining: 64 << 10}
		cmd.Env = mediaWorkerEnvironment()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		call, stop := context.WithTimeout(ctx, 10*time.Second)
		var startErr error
		lifetime, startErr = workerlife.Start(call, config, cmd, guards)
		step.StartCause = hebrewSmokeCause(startErr)
		step.NativeDeadlineFailed = call.Err() != nil
		var launch workerlife.LaunchFailure
		launchKnown := errors.As(startErr, &launch)
		if launchKnown {
			step.LaunchFailure = &hebrewSmokeLaunchFailure{Stage: launch.Stage(), ErrnoKnown: launch.ErrnoKnown(), Errno: launch.Errno()}
		}
		step.NativeStarted = cmd.Process != nil
		var waitErr error
		if cmd.Process != nil {
			done := make(chan struct{})
			go func() { waitErr = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-call.Done():
				// os.Process retains launch identity across a racing Wait. Any
				// surviving group member leaves AwaitCleanup fail-closed below.
				_ = cmd.Process.Kill()
			}
			step.NativeDeadlineFailed = step.NativeDeadlineFailed || call.Err() != nil
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			select {
			case <-done:
				step.WaitAcknowledged = true
				step.WaitCause = hebrewSmokeCause(waitErr)
				if cmd.ProcessState != nil {
					status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
					if ok {
						step.ExitKnown, step.SignalKnown = status.Exited(), status.Signaled()
						if step.ExitKnown {
							step.ExitCode = status.ExitStatus()
						}
						if step.SignalKnown {
							step.Signal = int(status.Signal())
						}
					}
				}
			case <-cleanup.Done():
				cleanupUncertain = true
				step.CleanupCause = "deadline"
			}
			if !cleanupUncertain && lifetime != nil {
				cleanupErr := lifetime.AwaitCleanup(cleanup)
				step.CleanupCause = hebrewSmokeCause(cleanupErr)
				cleanupUncertain = cleanupErr != nil
			}
			cleanupCancel()
		} else if lifetime != nil {
			cleanup, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			cleanupErr := lifetime.AwaitCleanup(cleanup)
			cleanupCancel()
			step.CleanupCause = hebrewSmokeCause(cleanupErr)
			cleanupUncertain = cleanupErr != nil
		} else {
			step.CleanupCause = "lease_absent"
		}
		stop()
		step.CleanupAcknowledged = lifetime != nil && !cleanupUncertain
		if step.CleanupAcknowledged {
			lifetime = nil
			if executable != nil {
				if err := executable.Close(); err != nil {
					cleanupUncertain = true
					step.CleanupAcknowledged = false
					step.CleanupCause = "executable_cleanup"
				} else {
					executable = nil
				}
			}
		}
		step.Seconds = time.Since(started).Seconds()
		if expectMissing {
			return !step.NativeDeadlineFailed && launchKnown && launch.Stage() == "native_start" && launch.ErrnoKnown() && launch.Errno() == uint32(syscall.ENOENT) && !step.NativeStarted && step.CleanupAcknowledged
		}
		return !step.NativeDeadlineFailed && startErr == nil && step.NativeStarted && step.WaitAcknowledged && waitErr == nil && step.ExitKnown && step.ExitCode == 0 && !step.SignalKnown && step.CleanupAcknowledged
	}
	var absent unix.Stat_t
	if err := unix.Fstatat(int(working.file.Fd()), "probe-missing-native", &absent, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, syscall.ENOENT) {
		t.Fatal("fixed missing-native probe target is not absent")
	}
	report.MissingNative.Stage = "missing_native_noop"
	if !run(&report.MissingNative, exec.Command(filepath.Join(working.path, "probe-missing-native")), true) {
		t.Fatal("real-app holder READY/NOOP probe failed; decoder version NOT_RUN")
	}
	decoderFile, err := openRuntime(ctx, b.Decoder)
	if decoderFile != nil {
		owner.runtimeChecks = append(owner.runtimeChecks, decoderFile)
	}
	if err != nil || diagnostics.bind("decoder", decoderFile) != nil {
		t.Fatal("sealed decoder verification failed after acknowledged NOOP")
	}
	executable, err = diagnostics.Prepare(ctx, decoderFile, 256<<20)
	if err != nil || nilExecutableAuthority(executable) {
		if !nilExecutableAuthority(executable) {
			if closeErr := executable.Close(); closeErr != nil {
				cleanupUncertain = true
			} else {
				executable = nil
			}
		}
		t.Fatal("sealed decoder preparation failed")
	}
	path := executable.Path()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		cleanupUncertain = true
		t.Fatal("sealed decoder prepared path unavailable")
	}
	cmd := exec.Command(path, "-version")
	// Empty FD4 preserves the production decoder descriptor layout without any
	// model or PCM authority; borrowed guards then occupy FD5, FD6 and FD7.
	cmd.ExtraFiles = []*os.File{decoderFile, guards[1]}
	report.DecoderVersion.Stage = "decoder_version"
	if !run(&report.DecoderVersion, cmd, false) {
		t.Fatal("sealed decoder version holder journey failed")
	}
}
