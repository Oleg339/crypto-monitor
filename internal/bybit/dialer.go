package bybit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// directDNS resolves using public nameservers directly, bypassing the Docker
// DNS proxy (127.0.0.11) which drops requests when the host resolver lags.
//
// A UDP "dial" succeeds even when the server is unreachable (no packets are
// sent), so an in-order fallback never engages. Instead rotate the nameserver
// on every Dial call: the Go resolver re-dials on query timeout, so its own
// retries land on a different server.
var nameservers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

var nsCounter atomic.Uint32

var directDNS = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		ns := nameservers[int(nsCounter.Add(1))%len(nameservers)]
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, ns)
	},
}

// ResilientDialer dials with direct public DNS and remembers the IPs of
// successful connections, falling back to them when resolution fails — so a
// DNS outage doesn't take down connections to hosts we have reached before.
type ResilientDialer struct {
	d  *net.Dialer
	mu sync.RWMutex
	ip map[string]string // host -> last known good IP
}

// NewResilientDialer returns a dialer suitable for http.Transport.DialContext
// and websocket.Dialer.NetDialContext.
func NewResilientDialer() *ResilientDialer {
	return &ResilientDialer{
		d: &net.Dialer{
			Timeout:  5 * time.Second,
			Resolver: directDNS,
		},
		ip: make(map[string]string),
	}
}

// newResilientHTTPClient builds an HTTP client that dials through a
// ResilientDialer kept warm for the host of rawURL.
func newResilientHTTPClient(rawURL string) *http.Client {
	rd := NewResilientDialer()
	if u, err := url.Parse(rawURL); err == nil && u.Hostname() != "" {
		rd.KeepWarm(u.Hostname(), 5*time.Minute)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: rd.DialContext,
		},
	}
}

// KeepWarm resolves host in the background at startup and every interval, so
// the IP cache is armed before the first DNS outage and stays fresh as CDN
// IPs rotate. Failures are ignored — the next tick retries.
func (rd *ResilientDialer) KeepWarm(host string, interval time.Duration) {
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			addrs, err := rd.d.Resolver.LookupHost(ctx, host)
			cancel()
			if err == nil {
				for _, a := range addrs {
					if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
						rd.mu.Lock()
						rd.ip[host] = a
						rd.mu.Unlock()
						break
					}
				}
			}
			time.Sleep(interval)
		}
	}()
}

func (rd *ResilientDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := rd.d.DialContext(ctx, network, addr)
	if err == nil {
		if host, _, e := net.SplitHostPort(addr); e == nil {
			if remote, _, e := net.SplitHostPort(conn.RemoteAddr().String()); e == nil {
				rd.mu.Lock()
				rd.ip[host] = remote
				rd.mu.Unlock()
			}
		}
		return conn, nil
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return nil, err
	}
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		return nil, err
	}
	rd.mu.RLock()
	ip, ok := rd.ip[host]
	rd.mu.RUnlock()
	if !ok {
		return nil, err
	}
	conn, fallbackErr := rd.d.DialContext(ctx, network, net.JoinHostPort(ip, port))
	if fallbackErr != nil {
		return nil, fmt.Errorf("%w (cached-IP fallback %s also failed: %v)", err, ip, fallbackErr)
	}
	return conn, nil
}
