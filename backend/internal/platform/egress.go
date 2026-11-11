package platform

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"syscall"
	"time"
)

// Egress is the policy for every webhook Customer Fabric sends: activation deliveries and change
// notifications. A destination's host:port must be on Hosts. Unless explicitly allowed, it must use
// https, and the address each connection actually reaches must be a public unicast address: a private,
// loopback, link-local, shared, documentation or otherwise special address is refused when the socket
// is opened, after name resolution, so a name that later resolves elsewhere (DNS rebinding) cannot reach
// an internal service. Both allowances exist for local development only.
type Egress struct {
	Hosts []string
	// AllowPrivate permits private and loopback addresses, as local test receivers use.
	AllowPrivate bool
	// AllowPlaintext permits http.
	AllowPlaintext bool
}

// errEgressRefused is a connection the egress policy refused; retrying cannot help.
var errEgressRefused = errors.New("the webhook address is not permitted")

// refusedPrefixes are special-purpose ranges netip's predicates do not cover.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (carrier-grade NAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, including broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, which embeds IPv4 addresses
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4, which embeds IPv4 addresses
	netip.MustParsePrefix("2001::/32"),       // Teredo, which embeds IPv4 addresses
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking
}

// globalUnicast is the only IPv6 range that may be public; everything outside it is refused.
var globalUnicast = netip.MustParsePrefix("2000::/3")

// sharedSpace is the shared address space local development may reach besides loopback and private
// addresses (see permits).
var sharedSpace = netip.MustParsePrefix("100.64.0.0/10")

// publicAddress reports whether a connection may reach addr under the default policy. IPv6 is allowed
// only within global unicast, so deprecated, compatible and translated forms are refused as a class.
func publicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || addr.Is6() && !globalUnicast.Contains(addr) {
		return false
	}
	for _, prefix := range refusedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// permits reports whether a connection may reach addr: a public address, or, where private networks are
// allowed, a loopback, private or shared one.
func (e Egress) permits(addr netip.Addr) bool {
	addr = addr.WithZone("").Unmap()
	return publicAddress(addr) || e.AllowPrivate && (addr.IsLoopback() || addr.IsPrivate() || sharedSpace.Contains(addr))
}

// Allows reports whether a destination URL may be registered or sent to: http(s) under the policy, an
// explicit port on a configured host, no credentials, query or fragment.
func (e Egress) Allows(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && (u.Scheme == "https" || u.Scheme == "http" && e.AllowPlaintext) && u.User == nil &&
		u.Fragment == "" && u.RawQuery == "" && u.Port() != "" && slices.Contains(e.Hosts, u.Host)
}

// Client returns an HTTP client that enforces the policy on every connection. It uses no proxy, which
// would connect to targets the policy never saw, and follows no redirect.
func (e Egress) Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errEgressRefused
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !e.permits(addr) {
			return errEgressRefused
		}
		return nil
	}}
	return &http.Client{Timeout: timeout,
		Transport: &http.Transport{Proxy: nil, DialContext: dialer.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 5 * time.Second,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
