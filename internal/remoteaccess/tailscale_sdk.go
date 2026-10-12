package remoteaccess

import (
	"context"
	"net"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/logtail"
	"tailscale.com/tsnet"
)

type sdkNode struct{ server *tsnet.Server }

func newSDKNode(o tailscaleOptions) tailscaleNode {
	// Official process-wide lifetime kill switch, before any tsnet Start. Release
	// builds additionally omit upload and buffering code with ts_omit_logtail.
	logtail.Disable()
	return &sdkNode{server: &tsnet.Server{Dir: o.dir, Hostname: o.hostname, Ephemeral: false, ControlURL: "https://controlplane.tailscale.com", RunWebClient: false, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}}
}
func (n *sdkNode) Start() error                 { return n.server.Start() }
func (n *sdkNode) Close() error                 { return n.server.Close() }
func (n *sdkNode) Up(ctx context.Context) error { _, err := n.server.Up(ctx); return err }
func (n *sdkNode) Status(ctx context.Context) (tailscaleStatus, error) {
	lc, err := n.server.LocalClient()
	if err != nil {
		return tailscaleStatus{}, err
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return tailscaleStatus{}, err
	}
	s := tailscaleStatus{state: st.BackendState, auth: st.AuthURL, hasTailnet: st.CurrentTailnet != nil}
	if st.CurrentTailnet != nil {
		s.magicDNS = st.CurrentTailnet.MagicDNSEnabled
	}
	for _, ip := range st.TailscaleIPs {
		if ip.IsValid() && !ip.IsUnspecified() {
			s.hasIP = true
		}
	}
	if st.Self != nil {
		s.dns = st.Self.DNSName
		s.funnelAllowed = ipn.CheckFunnelAccess(443, st.Self) == nil
		s.expired = st.Self.Expired || st.Self.KeyExpiry != nil && !st.Self.KeyExpiry.After(time.Now())
	}
	if len(st.CertDomains) > 0 {
		s.cert = st.CertDomains[0]
	}
	return s, nil
}
func (n *sdkNode) Listen(p Provider) (net.Listener, error) {
	if p == TailscaleFunnel {
		return n.server.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
	}
	return n.server.ListenTLS("tcp", ":443")
}
