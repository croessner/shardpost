package main

import (
	"crypto/tls"
	"net"
	"shardpost/internal/config"
	"strings"
	"time"

	"github.com/pires/go-proxyproto"
)

// listen opens a TCP listener, optionally wrapped for the PROXY protocol
// (v1 and v2) that the nauthilus-director sends when haproxy.enabled is
// set for a backend. Only peers in PROXY_PROTOCOL_TRUSTED_CIDRS may send a
// header; others are served without one.
func listen(addr string, tlsCfg *tls.Config, settings *config.Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	if cidrs := strings.TrimSpace(settings.Server.ProxyProtocolTrustedCidrs); cidrs != "" {
		policy, err := proxyproto.PolicyFromRanges(strings.Split(cidrs, ","), proxyproto.USE, proxyproto.IGNORE)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}

		ln = &proxyproto.Listener{Listener: ln, ConnPolicy: policy, ReadHeaderTimeout: 5 * time.Second}
	}

	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	}

	return ln, nil
}
