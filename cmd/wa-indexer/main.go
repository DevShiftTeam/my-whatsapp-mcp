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

	"github.com/Bodokh/wa-indexer/internal/app"
	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/localui"
	"github.com/Bodokh/wa-indexer/internal/mcpserver"
)

var errStartup = errors.New("application_unavailable")
var errRestartRequired = errors.New("restart_required")

const usage = `Usage: wa-indexer [serve] [--listen 127.0.0.1:64165] [--data-dir DIRECTORY] [--no-open]
       wa-indexer demo [--data-dir DIRECTORY] [--no-open]
       wa-indexer doctor [--data-dir DIRECTORY]
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
Use --open-existing to open an authenticated running owner without starting an
app or changing its configuration. Missing owner metadata fails without startup.
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
		fmt.Fprintln(stderr, "WA Indexer could not read the launch options. Run wa-indexer --help.")
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err := execute(ctx, opts, stdout, stderr); err != nil {
		if errors.Is(err, errRestartRequired) {
			fmt.Fprintln(stderr, "WA Indexer must restart to apply the supplied remote route settings.")
			return 1
		}
		fmt.Fprintln(stderr, startupErrorMessage(err))
		return 1
	}
	return 0
}

func startupErrorMessage(err error) string {
	// Inspect only fixed sentinels. Error text can contain paths, credentials or
	// source identifiers and must never become terminal output.
	switch app.StartupErrorCode(err) {
	case "native_namespace_unavailable":
		return "WA Indexer could not run (native_namespace_unavailable). Local runtime files could not be prepared. Restart the app and keep the existing data folder."
	case "extraction_recovery_pending":
		return "WA Indexer could not run (extraction_recovery_pending). Local media recovery is incomplete. Keep the data folder unchanged and retry after recovery."
	case "local_ai_bundle_unavailable":
		return "WA Indexer could not run (local_ai_bundle_unavailable). Local AI resources are unavailable. Start the app from its complete app bundle."
	}
	switch {
	case errors.Is(err, errAdminConfig):
		return "WA Indexer could not run (admin_auth_unavailable). Configure the private admin password hash and HTTPS admin route, then restart."
	case errors.Is(err, appdata.ErrUnsafeDataPath), errors.Is(err, archive.ErrUnsafeDataPath):
		return "WA Indexer could not run (unsafe_data_path). The data folder failed an ownership or privacy check. Check its permissions or use the original app data folder."
	case errors.Is(err, appdata.ErrDataUnavailable):
		return "WA Indexer could not run (data_unavailable). Local app data could not be opened. Check that the data folder is accessible, then try again."
	case errors.Is(err, appdata.ErrLockRequired):
		return "WA Indexer could not run (instance_lock_required). Ownership of the data folder could not be established. Restart the app and try again."
	case errors.Is(err, appdata.ErrInstanceAlreadyRunning), errors.Is(err, archive.ErrInstanceAlreadyRunning):
		return "WA Indexer could not run (instance_already_running). Another WA Indexer instance owns this data folder. Close that instance and try again."
	case errors.Is(err, archive.ErrArchiveUnavailable):
		return "WA Indexer could not run (archive_unavailable). The local archive could not be opened. Keep the data folder unchanged and retry."
	case errors.Is(err, archive.ErrSourceUnavailable):
		return "WA Indexer could not run (source_unavailable). WhatsApp data is unavailable. Restart the app and try again."
	case errors.Is(err, archive.ErrReconciliationRequired):
		return "WA Indexer could not run (reconciliation_required). An interrupted import requires archive recovery. Keep the data folder unchanged."
	case errors.Is(err, archive.ErrSchemaTooNew):
		return "WA Indexer could not run (schema_too_new). This archive needs a newer app version. Use the version that created it or a newer compatible version."
	case errors.Is(err, archive.ErrSchemaChanged):
		return "WA Indexer could not run (schema_changed). The archive schema changed. Restart with a compatible app version and keep the data folder unchanged."
	case errors.Is(err, archive.ErrFTS5Unavailable):
		return "WA Indexer could not run (fts5_unavailable). This build lacks keyword search support. Use the complete WA Indexer app build."
	case errors.Is(err, archive.ErrUnauthorized):
		return "WA Indexer could not run (unauthorized). The archive and linked WhatsApp account do not match. Use the matching account and data folder."
	case errors.Is(err, appdata.ErrUnsupportedPlatform):
		return "WA Indexer could not run (unsupported_platform). This platform is not supported by the local app. Use a supported app build."
	case errors.Is(err, context.Canceled):
		return "WA Indexer could not run (canceled). Startup was canceled. Start the app again when ready."
	case errors.Is(err, context.DeadlineExceeded):
		return "WA Indexer could not run (deadline). Startup timed out. Restart the app and try again."
	default:
		return "WA Indexer could not run (application_unavailable). Restart the app and try again."
	}
}

func serve(ctx context.Context, opts options, stdout, stderr io.Writer) (result error) {
	if opts.openExisting {
		if opts.adminListen != "" || opts.adminPublicURL != "" || opts.adminEnvFile != "" || opts.mcpListen != "" || opts.mcpPublicURL != "" || opts.mcpHeaderAuth || opts.originalBudget != nil || opts.demoAcceptance {
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
	// Bind before opening any local data, including a WhatsApp session store.
	listener, err := bindLoopback(ctx, opts.listen)
	if errors.Is(err, errListenOccupied) {
		if opts.mcpListen != "" || adminConfig != nil {
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
	cfg := app.Config{DataDir: opts.dataDir, Demo: opts.demo, DemoAcceptanceFixtures: opts.demoAcceptance, ExecutablePath: executable, OriginalBudgetBytes: opts.originalBudget}
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
		if opts.mcpListen != "" || adminConfig != nil {
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
	fmt.Fprintln(stdout, "WA Indexer is running. Open the private owner link on this computer.")
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
	fmt.Fprintln(stdout, "WA Indexer is already running. Open the private owner link on this computer.")
	return nil
}
