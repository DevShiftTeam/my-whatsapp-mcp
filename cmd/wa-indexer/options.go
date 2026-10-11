package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
)

var (
	errArguments      = errors.New("invalid_launch_options")
	errListen         = errors.New("local_listener_unavailable")
	errListenOccupied = errors.New("local_listener_occupied")
)

type options struct {
	command              string
	profile              string
	config               string
	listen               string
	dataDir              string
	demo                 bool
	demoAcceptance       bool
	noOpen               bool
	openExisting         bool
	help                 bool
	originalBudget       *int64
	mcpListen            string
	mcpPublicURL         string
	mcpHeaderAuth        bool
	adminListen          string
	adminPublicURL       string
	adminEnvFile         string
	transcriptionEnvFile string
}

// Supplied values never form part of an error or the flag parser's output.
func parseArgs(args []string) (options, error) {
	var opts options
	opts.command = "serve"
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		opts.help = true
		return opts, nil
	}
	if len(args) == 0 {
		args = []string{"serve"}
	} else if strings.HasPrefix(args[0], "-") {
		args = append([]string{"serve"}, args...)
	}
	tail := args[1:]
	explicitDemo := false
	switch args[0] {
	case "serve":
	case "demo":
		explicitDemo = true
		opts.demo = true
	case "doctor":
		opts.command = "doctor"
	case "models":
		if len(args) < 3 || (args[1] != "install" && args[1] != "verify") || !validProfileArgument(args[2]) {
			return options{}, errArguments
		}
		opts.command = "models_" + args[1]
		opts.profile = args[2]
		tail = args[3:]
	case "mcp":
		if len(args) < 2 || args[1] != "stdio" {
			return options{}, errArguments
		}
		opts.command = "mcp_stdio"
		tail = args[2:]
	default:
		return options{}, errArguments
	}
	flags := flag.NewFlagSet(opts.command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var originalBudget string
	if opts.command == "mcp_stdio" {
		flags.StringVar(&opts.config, "config", "", "")
	} else {
		flags.StringVar(&opts.dataDir, "data-dir", "", "")
		flags.StringVar(&originalBudget, "originals-budget", "", "")
	}
	if opts.command == "serve" {
		flags.BoolVar(&opts.demoAcceptance, "demo-acceptance-fixtures", false, "")
		flags.StringVar(&opts.mcpListen, "mcp-listen", "", "")
		flags.StringVar(&opts.adminListen, "admin-listen", "", "")
		flags.StringVar(&opts.adminPublicURL, "admin-public-url", "", "")
		flags.StringVar(&opts.adminEnvFile, "admin-env-file", "", "")
		flags.StringVar(&opts.transcriptionEnvFile, "transcription-env-file", "", "")
		flags.StringVar(&opts.mcpPublicURL, "mcp-public-url", "", "")
		flags.BoolVar(&opts.mcpHeaderAuth, "mcp-header-auth-available", false, "")
		flags.StringVar(&opts.listen, "listen", "127.0.0.1:64165", "")
		if !explicitDemo {
			flags.BoolVar(&opts.demo, "demo", false, "")
		}
		flags.BoolVar(&opts.noOpen, "no-open", false, "")
		flags.BoolVar(&opts.openExisting, "open-existing", false, "")
	}
	if err := flags.Parse(tail); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.help = true
			return opts, nil
		}
		return options{}, errArguments
	}
	if flags.NArg() != 0 {
		return options{}, errArguments
	}
	if opts.demoAcceptance && !opts.demo {
		return options{}, errArguments
	}
	if opts.command == "mcp_stdio" && (!filepath.IsAbs(opts.config) || filepath.Clean(opts.config) != opts.config) {
		return options{}, errArguments
	}
	var emptyDataDir bool
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "data-dir" && opts.dataDir == "" {
			emptyDataDir = true
		}
	})
	budgetExplicit := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "originals-budget" {
			budgetExplicit = true
		}
	})
	if budgetExplicit {
		value, err := parseOriginalBudget(originalBudget)
		if err != nil {
			return options{}, errArguments
		}
		opts.originalBudget = &value
	}
	if emptyDataDir {
		return options{}, errArguments
	}
	remoteExplicit := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "mcp-listen" || value.Name == "mcp-public-url" || value.Name == "mcp-header-auth-available" {
			remoteExplicit = true
		}
	})
	if remoteExplicit {
		address, err := netip.ParseAddrPort(opts.mcpListen)
		if err != nil || !address.Addr().IsLoopback() || address.Addr().Is4In6() || address.Addr().Zone() != "" || address.Port() == 0 || address.String() != opts.mcpListen {
			return options{}, errArguments
		}
		canonical, err := clientsetup.NormalizeRemoteURL(opts.mcpPublicURL)
		if err != nil || canonical != opts.mcpPublicURL {
			return options{}, errArguments
		}
	}
	adminExplicit := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "admin-listen" || value.Name == "admin-public-url" || value.Name == "admin-env-file" {
			adminExplicit = true
		}
	})
	if adminExplicit && !validAdminRoute(opts) {
		return options{}, errArguments
	}
	transcriptionExplicit := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "transcription-env-file" {
			transcriptionExplicit = true
		}
	})
	if transcriptionExplicit && (!filepath.IsAbs(opts.transcriptionEnvFile) || filepath.Clean(opts.transcriptionEnvFile) != opts.transcriptionEnvFile) {
		return options{}, errArguments
	}
	if opts.openExisting && (adminExplicit || remoteExplicit || opts.originalBudget != nil || opts.demoAcceptance || transcriptionExplicit) {
		return options{}, errArguments
	}
	return opts, nil
}

func parseOriginalBudget(value string) (int64, error) {
	multiplier := uint64(0)
	switch {
	case strings.HasSuffix(value, "MiB"):
		multiplier = 1 << 20
	case strings.HasSuffix(value, "GiB"):
		multiplier = 1 << 30
	default:
		return 0, errArguments
	}
	number := value[:len(value)-3]
	if number == "" || strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return 0, errArguments
	}
	parsed, err := strconv.ParseUint(number, 10, 64)
	if err != nil || parsed > uint64(archive.MaximumOriginalBudgetBytes)/multiplier {
		return 0, errArguments
	}
	bytes := int64(parsed * multiplier)
	if bytes < archive.MinimumOriginalBudgetBytes {
		return 0, errArguments
	}
	return bytes, nil
}

func validProfileArgument(value string) bool {
	if len(value) < 1 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for i, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '-' || r == '_' || r == '.')) {
			return false
		}
	}
	return true
}

// DNS names are intentionally absent: binding a literal loopback address is an
// invariant, even when hostname resolution or network configuration changes.
func bindLoopback(ctx context.Context, value string) (net.Listener, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return nil, errListen
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.IsLoopback() || address.Is4In6() || address.Zone() != "" || portText == "" {
		return nil, errListen
	}
	if strings.IndexFunc(portText, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return nil, errListen
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, errListen
	}
	network := "tcp4"
	if address.Is6() {
		network = "tcp6"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, network, net.JoinHostPort(address.String(), strconv.Itoa(port)))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errListenOccupied
		}
		return nil, errListen
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !bound.IP.IsLoopback() {
		listener.Close()
		return nil, errListen
	}
	return listener, nil
}
