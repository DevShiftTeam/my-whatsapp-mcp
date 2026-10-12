package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
)

type ownerListenerConfig struct {
	Listen, Origin string
	Container      bool
}

func ownerLaunchConfig(opts options) (ownerListenerConfig, error) {
	cfg := ownerListenerConfig{Listen: opts.listen, Container: opts.container}
	if !opts.container {
		host, port, err := net.SplitHostPort(opts.listen)
		ip, ipErr := netip.ParseAddr(host)
		portNumber, portErr := strconv.Atoi(port)
		if err != nil || ipErr != nil || !ip.IsLoopback() || ip.Is4In6() || ip.Zone() != "" || port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) != -1 || portErr != nil || portNumber < 0 || portNumber > 65535 || opts.browserOrigin != "" || opts.dockerContainerName != "" {
			return ownerListenerConfig{}, errArguments
		}
		return cfg, nil
	}
	if opts.command != "serve" || !validContainerListen(opts.listen) || !validBrowserOrigin(opts.browserOrigin) {
		return ownerListenerConfig{}, errArguments
	}
	cfg.Origin = opts.browserOrigin
	return cfg, nil
}

func bindOwner(ctx context.Context, cfg ownerListenerConfig) (net.Listener, error) {
	if !cfg.Container {
		return bindLoopback(ctx, cfg.Listen)
	}
	if !validContainerListen(cfg.Listen) {
		return nil, errListen
	}
	address, _ := netip.ParseAddrPort(cfg.Listen)
	network := "tcp4"
	if address.Addr().Is6() {
		network = "tcp6"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, network, cfg.Listen)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errListenOccupied
		}
		return nil, errListen
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || bound.Zone != "" || bound.AddrPort() != address {
		listener.Close()
		return nil, errListen
	}
	return listener, nil
}

func validContainerListen(value string) bool {
	address, err := netip.ParseAddrPort(value)
	return err == nil && address.Port() != 0 && address.String() == value && !address.Addr().Is4In6() && address.Addr().Zone() == "" && (address.Addr().IsLoopback() || address.Addr().IsUnspecified())
}

func validBrowserOrigin(value string) bool {
	if !strings.HasPrefix(value, "http://") {
		return false
	}
	address, err := netip.ParseAddrPort(strings.TrimPrefix(value, "http://"))
	return err == nil && address.Port() != 0 && address.Addr().IsLoopback() && !address.Addr().Is4In6() && address.Addr().Zone() == "" && value == "http://"+address.String()
}

// Prevent repeated authority flags from silently overriding earlier values.
type ownerFlagValue struct {
	flag.Value
	seen    bool
	boolean bool
}

func (value *ownerFlagValue) Set(input string) error {
	if value.seen {
		return errArguments
	}
	value.seen = true
	return value.Value.Set(input)
}
func (value *ownerFlagValue) IsBoolFlag() bool { return value.boolean }

func addContainerFlags(flags *flag.FlagSet, opts *options) {
	flags.BoolVar(&opts.container, "container", false, "")
	flags.StringVar(&opts.browserOrigin, "browser-origin", "", "")
	flags.StringVar(&opts.dockerContainerName, "docker-container-name", "", "")
	for _, name := range []string{"container", "browser-origin", "docker-container-name", "listen"} {
		value := flags.Lookup(name)
		value.Value = &ownerFlagValue{Value: value.Value, boolean: name == "container"}
	}
}

func configureContainerOptions(flags *flag.FlagSet, opts *options) error {
	explicit := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { explicit[value.Name] = true })
	if !opts.container {
		if explicit["browser-origin"] || explicit["docker-container-name"] {
			return errArguments
		}
		return nil
	}
	if !explicit["listen"] {
		opts.listen = "0.0.0.0:8123"
	}
	if !explicit["browser-origin"] {
		opts.browserOrigin = "http://127.0.0.1:8123"
	}
	opts.noOpen = true
	if explicit["docker-container-name"] && !validProfileArgument(opts.dockerContainerName) {
		return errArguments
	}
	_, err := ownerLaunchConfig(*opts)
	return err
}
