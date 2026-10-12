package localui

import (
	"net"
	"net/netip"
	"strconv"
)

// The browser origin and internal listener are independent trusted composition
// inputs. Neither is inferred from forwarded headers or request addresses.
func validContainerListen(value string) bool {
	address, err := netip.ParseAddrPort(value)
	return err == nil && address.Port() != 0 && address.String() == value && !address.Addr().Is4In6() && address.Addr().Zone() == "" && (address.Addr().IsLoopback() || address.Addr().IsUnspecified())
}

func (s *Server) acceptsListener(listener net.Listener) bool {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Zone != "" {
		return false
	}
	actual := net.JoinHostPort(address.IP.String(), strconv.Itoa(address.Port))
	if s.containerListen != "" {
		return validContainerListen(actual) && actual == s.containerListen
	}
	return address.IP.IsLoopback() && actual == s.host
}
