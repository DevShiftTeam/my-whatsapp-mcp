// Command wa-indexer launches the private local owner interface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

var errStartup = errors.New("application_unavailable")
var errRestartRequired = errors.New("restart_required")

const usage = `Usage: wa-indexer [serve] [--listen 127.0.0.1:64165] [--data-dir DIRECTORY] [--no-open]
       wa-indexer demo [--data-dir DIRECTORY] [--no-open]
       wa-indexer doctor [--data-dir DIRECTORY]
       wa-indexer recover-snapshot --data-dir ABSOLUTE_PRIVATE_PROFILE
       wa-indexer recover-original-cache --data-dir ABSOLUTE_PRIVATE_PROFILE --backup-dir ABSOLUTE_STOPPED_BACKUP
       wa-indexer models install|verify PROFILE [--data-dir DIRECTORY]
       wa-indexer mcp stdio --config ABSOLUTE_PRIVATE_CONFIG

The listener must use a literal loopback address. The app opens a private
owner link on this computer. Demo uses a separate synthetic-data archive.
Use --originals-budget INTEGERMiB or INTEGERGiB to explicitly change the
persisted originals budget (64MiB..1024GiB); omission preserves it.
Remote bearer setup requires --mcp-listen LITERAL_LOOPBACK:FIXED_PORT together
with --mcp-public-url https://PUBLIC_HOST/mcp. Configure your HTTPS tunnel
separately. --mcp-header-auth-available confirms hosted header-auth support.
Password-protected admin access requires --admin-listen 127.0.0.1:FIXED_PORT
with --admin-public-url https://PUBLIC_HOST and WA_INDEXER_ADMIN_PASSWORD_HASH.
Use --admin-env-file ABSOLUTE_PRIVATE_ENV to load only that hash from a private
local file. Admin credentials and scoped MCP client credentials are separate.
Voice transcription uses WA_INDEXER_TRANSCRIPTION_URL and TOKEN with optional
WA_INDEXER_TRANSCRIPTION_LANGUAGE (auto) and TIMEOUT_SECONDS (600). Use
--transcription-env-file ABSOLUTE_PRIVATE_ENV to load the private service setup.
Use --open-existing to open an authenticated running owner without starting an
app or changing its configuration. Missing owner metadata fails without startup.
After stopping the app, recover-snapshot explicitly restores the committed
archive after an eligible interrupted drain. It preserves saved messages and
clients, records a coverage gap, and does not verify the phone's current state.
recover-original-cache verifies already-present originals against a stopped
private backup and current source authority. It restores only eligible cache
metadata, copies no recordings, and starts no connections or processing workers.
`

func main() {
	if isWorkerHolder(os.Args[1:]) {
		os.Exit(runWorkerHolder())
	}
	os.Exit(signalMain())
}

func signalMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "My WhatsApp MCP could not read the launch options. Run wa-indexer --help.")
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err := execute(ctx, opts, stdout, stderr); err != nil {
		if opts.command == "recover_snapshot" {
			fmt.Fprintln(stderr, snapshotRecoveryErrorMessage(err))
			return 1
		}
		if opts.command == "recover_original_cache" {
			fmt.Fprintln(stderr, originalCacheRecoveryErrorMessage(err))
			fmt.Fprintln(stderr, "Earlier eligible cache repairs may have committed. Recovery is idempotent; preserve both profiles before retrying.")
			return 1
		}
		if errors.Is(err, errRestartRequired) {
			fmt.Fprintln(stderr, "My WhatsApp MCP must restart to apply the supplied service settings.")
			return 1
		}
		fmt.Fprintln(stderr, startupErrorMessage(err))
		return 1
	}
	return 0
}

func startupErrorMessage(err error) string {
	if message, ok := voiceConfigErrorMessage(err); ok {
		return message
	}
	// Inspect only fixed sentinels. Error text can contain paths, credentials or
	// source identifiers and must never become terminal output.
	switch app.StartupErrorCode(err) {
	case "transcription_config_invalid":
		return "My WhatsApp MCP could not run (transcription_config_invalid). Check the private voice service configuration, then restart."
	case "native_namespace_unavailable":
		return "My WhatsApp MCP could not run (native_namespace_unavailable). Local runtime files could not be prepared. Restart the app and keep the existing data folder."
	case "extraction_recovery_pending":
		return "My WhatsApp MCP could not run (extraction_recovery_pending). Local media recovery is incomplete. Keep the data folder unchanged and retry after recovery."
	case "local_ai_bundle_unavailable":
		return "My WhatsApp MCP could not run (local_ai_bundle_unavailable). Local AI resources are unavailable. Start the app from its complete app bundle."
	}
	switch {
	case errors.Is(err, errTranscriptionConfig):
		return "My WhatsApp MCP could not run (transcription_config_invalid). Check the private voice service configuration, then restart."
	case errors.Is(err, errAdminConfig):
		return "My WhatsApp MCP could not run (admin_auth_unavailable). Configure the private admin password hash and HTTPS admin route, then restart."
	case errors.Is(err, appdata.ErrUnsafeDataPath), errors.Is(err, archive.ErrUnsafeDataPath):
		return "My WhatsApp MCP could not run (unsafe_data_path). The data folder failed an ownership or privacy check. Check its permissions or use the original app data folder."
	case errors.Is(err, appdata.ErrDataUnavailable):
		return "My WhatsApp MCP could not run (data_unavailable). Local app data could not be opened. Check that the data folder is accessible, then try again."
	case errors.Is(err, appdata.ErrLockRequired):
		return "My WhatsApp MCP could not run (instance_lock_required). Ownership of the data folder could not be established. Restart the app and try again."
	case errors.Is(err, appdata.ErrInstanceAlreadyRunning), errors.Is(err, archive.ErrInstanceAlreadyRunning):
		return "My WhatsApp MCP could not run (instance_already_running). Another My WhatsApp MCP instance owns this data folder. Close that instance and try again."
	case errors.Is(err, archive.ErrArchiveUnavailable):
		return "My WhatsApp MCP could not run (archive_unavailable). The local archive could not be opened. Keep the data folder unchanged and retry."
	case errors.Is(err, archive.ErrSourceUnavailable):
		return "My WhatsApp MCP could not run (source_unavailable). WhatsApp data is unavailable. Restart the app and try again."
	case errors.Is(err, archive.ErrReconciliationRequired):
		return "My WhatsApp MCP could not run (reconciliation_required). An interrupted import requires archive recovery. Keep the data folder unchanged."
	case errors.Is(err, archive.ErrSchemaTooNew):
		return "My WhatsApp MCP could not run (schema_too_new). This archive needs a newer app version. Use the version that created it or a newer compatible version."
	case errors.Is(err, archive.ErrSchemaChanged):
		return "My WhatsApp MCP could not run (schema_changed). The archive schema changed. Restart with a compatible app version and keep the data folder unchanged."
	case errors.Is(err, archive.ErrFTS5Unavailable):
		return "My WhatsApp MCP could not run (fts5_unavailable). This build lacks keyword search support. Use the complete My WhatsApp MCP app build."
	case errors.Is(err, archive.ErrUnauthorized):
		return "My WhatsApp MCP could not run (unauthorized). The archive and linked WhatsApp account do not match. Use the matching account and data folder."
	case errors.Is(err, appdata.ErrUnsupportedPlatform):
		return "My WhatsApp MCP could not run (unsupported_platform). This platform is not supported by the local app. Use a supported app build."
	case errors.Is(err, context.Canceled):
		return "My WhatsApp MCP could not run (canceled). Startup was canceled. Start the app again when ready."
	case errors.Is(err, context.DeadlineExceeded):
		return "My WhatsApp MCP could not run (deadline). Startup timed out. Restart the app and try again."
	default:
		return "My WhatsApp MCP could not run (application_unavailable). Restart the app and try again."
	}
}

func serve(ctx context.Context, opts options, stdout, stderr io.Writer) (result error) {
	if opts.openExisting {
		if opts.adminListen != "" || opts.adminPublicURL != "" || opts.adminEnvFile != "" || opts.mcpListen != "" || opts.mcpPublicURL != "" || opts.mcpHeaderAuth || opts.originalBudget != nil || opts.demoAcceptance || opts.transcriptionEnvFile != "" {
			return errStartup
		}
		candidate, err := app.OwnerAttachURL(app.Config{DataDir: opts.dataDir, Demo: opts.demo})
		if err != nil {
			return errStartup
		}
		return attachExistingOnListener(ctx, candidate, opts, stdout)
	}
	adminConfig, err := remoteAdminLaunchConfig(opts)
	if err != nil {
		return errAdminConfig
	}
	voiceConfig, err := transcriptionLaunchConfig(opts)
	if err != nil {
		return err
	}
	// Bind before opening any local data, including a WhatsApp session store.
	listener, err := bindLoopback(ctx, opts.listen)
	if errors.Is(err, errListenOccupied) {
		if opts.mcpListen != "" || adminConfig != nil || voiceConfig != nil {
			return errRestartRequired
		}
		if opts.originalBudget != nil {
			return errStartup
		}
		candidate, readErr := app.OwnerAttachURL(app.Config{DataDir: opts.dataDir, Demo: opts.demo})
		if readErr != nil {
			return errStartup
		}
		return attachExistingOnListener(ctx, candidate, opts, stdout)
	}
	if err != nil {
		return errStartup
	}
	defer listener.Close()
	var adminListener net.Listener
	if adminConfig != nil {
		adminListener, err = bindLoopback(ctx, adminConfig.ListenAddress)
		if err != nil {
			return errStartup
		}
		defer adminListener.Close()
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return errStartup
	}
	cfg := app.Config{DataDir: opts.dataDir, Demo: opts.demo, DemoAcceptanceFixtures: opts.demoAcceptance, ExecutablePath: executable, OriginalBudgetBytes: opts.originalBudget, Transcription: voiceConfig}
	if opts.mcpListen != "" {
		cfg.MCP, err = remoteLaunchConfig(opts)
		if err != nil {
			return errStartup
		}
	}
	cfg.Native, cfg.Media, _ = app.LoadBundledAI(executable)
	application, err := app.New(serveCtx, cfg)
	if err != nil {
		if closeErr := app.CloseBundledAI(cfg.Native, cfg.Media); closeErr != nil {
			return errStartup
		}
	}
	if errors.Is(err, appdata.ErrInstanceAlreadyRunning) {
		if opts.mcpListen != "" || adminConfig != nil || voiceConfig != nil {
			return errRestartRequired
		}
		if opts.originalBudget != nil {
			return errStartup
		}
		// The candidate remains private and is used only after a fresh signed mode
		// handshake. Never retry a stale bootstrap or trust a PID/health response.
		listener.Close()
		candidate, readErr := app.OwnerAttachURL(app.Config{DataDir: opts.dataDir, Demo: opts.demo})
		if readErr != nil {
			return errStartup
		}
		return attachExistingOnListener(serveCtx, candidate, opts, stdout)
	}
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		// Serve has joined its HTTP shutdown before this deferred cleanup runs.
		// Remove the capability while the application still holds its lease.
		if err := application.RemoveOwnerURL(); err != nil {
			result = errStartup
		}
		if err := drainApplication(application); err != nil {
			result = errStartup
		}
	}()
	server, err := localui.New(localui.Config{Origin: "http://" + listener.Addr().String(), Demo: opts.demo}, application)
	if err != nil {
		return errStartup
	}
	defer server.RevokeSessions()
	bindings := []ownerInterfaceBinding{{server: server, listener: listener}}
	if adminConfig != nil {
		if opts.mcpListen != "" {
			adminConfig.MCPHandler = application.RemoteMCPHandler()
		}
		remote, err := localui.NewRemote(*adminConfig, application)
		if err != nil {
			return errAdminConfig
		}
		defer remote.RevokeSessions()
		if localui.LinkOwnerRevocations(server, remote) != nil {
			return errAdminConfig
		}
		bindings = append(bindings, ownerInterfaceBinding{server: remote, listener: adminListener})
	}
	if err := application.WriteOwnerURL(server.AttachURL()); err != nil {
		return errStartup
	}
	fmt.Fprintln(stdout, "My WhatsApp MCP is running. Open the private owner link on this computer.")
	if !opts.noOpen {
		openCtx, openCancel := context.WithTimeout(serveCtx, 5*time.Second)
		err := openBrowser(openCtx, server.BootstrapURL())
		openCancel()
		if err != nil && serveCtx.Err() == nil {
			fmt.Fprintln(stderr, "The browser could not open. Open the private owner link on this computer.")
		}
	}
	if err := serveOwnerInterfaces(serveCtx, bindings); err != nil {
		return errStartup
	}
	return nil
}

// The stdio helper reads only its protected configuration; it never constructs
// an Application, opens WhatsApp, or starts native model ownership.
func execute(ctx context.Context, opts options, stdout, stderr io.Writer) (result error) {
	switch opts.command {
	case "recover_original_cache":
		receipt, err := app.RecoverOriginalCache(ctx, opts.dataDir, opts.backupDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(receipt)
	case "recover_snapshot":
		receipt, err := app.RecoverCommittedSnapshot(ctx, opts.dataDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(struct {
			RecoveryEpoch             uint64                  `json:"recovery_epoch"`
			ArchiveRevision           archive.ArchiveRevision `json:"archive_revision"`
			Mode                      string                  `json:"mode"`
			Code                      string                  `json:"code"`
			Coverage                  string                  `json:"coverage"`
			CurrentPhoneStateVerified bool                    `json:"current_phone_state_verified"`
		}{receipt.RecoveryEpoch, receipt.ArchiveRevision, receipt.Mode, receipt.Code, "partial", false})
	case "mcp_stdio":
		cfg, err := mcpserver.ReadCredentialFile(opts.config)
		if err != nil {
			return err
		}
		return mcpserver.RunStdio(ctx, cfg)
	case "doctor", "models_install", "models_verify":
		application, err := app.New(ctx, app.Config{DataDir: opts.dataDir, Offline: true, OriginalBudgetBytes: opts.originalBudget})
		if err != nil {
			return err
		}
		defer func() {
			if err := drainApplication(application); err != nil {
				result = errStartup
			}
		}()
		switch opts.command {
		case "models_install":
			return application.InstallModel(ctx, opts.profile)
		case "models_verify":
			return application.VerifyModel(ctx, opts.profile)
		default:
			report, err := application.Doctor(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(report)
		}
	default:
		return serve(ctx, opts, stdout, stderr)
	}
}

func originalCacheRecoveryErrorMessage(err error) string {
	switch {
	case errors.Is(err, appdata.ErrInstanceAlreadyRunning), errors.Is(err, archive.ErrInstanceAlreadyRunning):
		return "Original cache recovery was refused (instance_already_running). Stop both the current archive and the backup before trying again."
	case errors.Is(err, appdata.ErrUnsafeDataPath), errors.Is(err, archive.ErrUnsafeDataPath):
		return "Original cache recovery was refused (unsafe_data_path). Use distinct existing private profiles with their original permissions."
	case errors.Is(err, appdata.ErrDataUnavailable):
		return "Original cache recovery was refused (data_unavailable). Both existing profiles require their archive, session, and app state."
	case errors.Is(err, archive.ErrUnauthorized):
		return "Original cache recovery was refused (account_mismatch). Use the stopped backup belonging to this WhatsApp archive."
	case errors.Is(err, archive.ErrReconciliationRequired):
		return "Original cache recovery was refused (reconciliation_required). Resolve the archive's source-safety state first and keep both profiles unchanged."
	case errors.Is(err, archive.ErrLimitExceeded):
		return "Original cache recovery was refused (limit_exceeded). The backup exceeds the bounded recovery inventory. Keep both profiles unchanged."
	case errors.Is(err, context.Canceled):
		return "Original cache recovery was canceled (canceled). Keep both profiles unchanged and retry."
	case errors.Is(err, context.DeadlineExceeded):
		return "Original cache recovery timed out (deadline). Keep both profiles unchanged and retry."
	default:
		return "Original cache recovery could not finish (archive_unavailable). Keep both profiles unchanged and report this diagnostic."
	}
}

func snapshotRecoveryErrorMessage(err error) string {
	switch {
	case errors.Is(err, appdata.ErrInstanceAlreadyRunning), errors.Is(err, archive.ErrInstanceAlreadyRunning):
		return "Snapshot recovery was refused (instance_already_running). Close the archive app before trying again."
	case errors.Is(err, appdata.ErrUnsafeDataPath), errors.Is(err, archive.ErrUnsafeDataPath):
		return "Snapshot recovery was refused (unsafe_data_path). Use the existing private app data folder and check its permissions."
	case errors.Is(err, appdata.ErrDataUnavailable):
		return "Snapshot recovery was refused (data_unavailable). The existing archive, session, and app state are required. Keep the data folder unchanged."
	case errors.Is(err, archive.ErrReconciliationRequired):
		return "Snapshot recovery was refused (reconciliation_required). This archive is not eligible for interrupted-drain recovery. Keep the data folder unchanged."
	case errors.Is(err, archive.ErrPolicyChanged):
		return "Snapshot recovery was refused (policy_changed). Archive safety changed during recovery. Keep the data folder unchanged and retry."
	case app.StartupErrorCode(err) == "extraction_recovery_pending":
		return "Snapshot recovery could not finish (extraction_recovery_pending). Prior media ownership could not be settled. Keep the data folder unchanged and retry."
	case errors.Is(err, context.Canceled):
		return "Snapshot recovery was canceled (canceled). Keep the data folder unchanged and retry."
	case errors.Is(err, context.DeadlineExceeded):
		return "Snapshot recovery timed out (deadline). Keep the data folder unchanged and retry."
	default:
		return "Snapshot recovery could not finish (archive_unavailable). Keep the data folder unchanged and retry."
	}
}

// A fixed launch address must match the running owner's origin. An explicit
// port zero keeps the existing authenticated ephemeral-listener behavior.
func attachExistingOnListener(ctx context.Context, candidate string, opts options, stdout io.Writer) error {
	host, portText, err := net.SplitHostPort(opts.listen)
	if err != nil {
		return errStartup
	}
	requested, err := netip.ParseAddrPort(net.JoinHostPort(host, portText))
	if err != nil {
		return errStartup
	}
	if requested.Port() != 0 {
		owner, err := url.Parse(candidate)
		if err != nil || owner.Scheme != "http" || owner.Host != requested.String() {
			return errStartup
		}
	}
	return attachExisting(ctx, candidate, opts.demo, opts.noOpen, stdout)
}

// Only the authenticated fresh bootstrap result reaches the browser opener.
func attachExisting(ctx context.Context, candidate string, demo, noOpen bool, stdout io.Writer) error {
	bootstrap, err := localui.Attach(ctx, candidate, demo)
	if err != nil {
		return errStartup
	}
	if !noOpen {
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = openBrowser(openCtx, bootstrap)
		cancel()
		if err != nil {
			return errStartup
		}
	}
	fmt.Fprintln(stdout, "My WhatsApp MCP is already running. Open the private owner link on this computer.")
	return nil
}
