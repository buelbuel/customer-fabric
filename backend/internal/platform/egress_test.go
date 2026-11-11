package platform

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPublicAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:2800:21f:cb07:6820:80da:af6b:8b2c": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "255.255.255.255": false, "224.0.0.1": false, "198.18.0.1": false,
		"192.0.2.1": false, "::1": false, "::": false, "fc00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
		"::ffff:10.0.0.1": false, "64:ff9b::a00:1": false, "2002:a00:1::": false, "2001:db8::1": false, "2001::1": false,
		"::7f00:1": false, "::a00:1": false, "::ffff:0:7f00:1": false, "fec0::1": false, "100::1": false, "2001:2::1": false,
		"192.88.99.1": false, "fe80::1%eth0": false,
	} {
		if got := publicAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

// Allowing private networks adds loopback, private and shared space only: metadata endpoints,
// link-local, multicast and reserved addresses stay refused.
func TestEgressAllowPrivateIsNarrow(t *testing.T) {
	dev := Egress{AllowPrivate: true}
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.0.0.1": true, "192.168.1.1": true, "100.64.0.1": true, "::1": true, "fd00::1": true, "93.184.215.14": true,
		"169.254.169.254": false, "fe80::1": false, "0.0.0.0": false, "224.0.0.1": false, "240.0.0.1": false, "::": false,
	} {
		if got := dev.permits(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if (Egress{}).permits(netip.MustParseAddr("10.0.0.1")) {
		t.Error("the default policy permits a private address")
	}
}

func TestEgressAllows(t *testing.T) {
	strict := Egress{Hosts: []string{"hooks.example:443", "sink:8090"}}
	dev := Egress{Hosts: strict.Hosts, AllowPlaintext: true}
	for raw, want := range map[string][2]bool{
		"https://hooks.example:443/a":          {true, true},
		"http://sink:8090/hook":                {false, true},
		"https://hooks.example/a":              {false, false},
		"https://other.example:443/a":          {false, false},
		"https://user@hooks.example:443/a":     {false, false},
		"https://hooks.example:443/a?x=1":      {false, false},
		"https://hooks.example:443/a#frag":     {false, false},
		"ftp://hooks.example:443/a":            {false, false},
		"https://" + strings.Repeat("a", 2050): {false, false},
	} {
		if got := [2]bool{strict.Allows(raw), dev.Allows(raw)}; got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

// A connection that resolves to a private address is refused at connect time under the default policy,
// whatever the URL named, and permitted only where private networks are explicitly allowed.
func TestEgressClientRefusesPrivateAddressesWhenConnecting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	_, err := (Egress{}).Client(2 * time.Second).Get(server.URL)
	if !errors.Is(err, errEgressRefused) {
		t.Fatalf("a loopback connection under the default policy: %v", err)
	}
	// A name resolving to loopback is refused the same way: the check is on the address connected to.
	if _, err = (Egress{}).Client(2 * time.Second).Get(strings.Replace(server.URL, "127.0.0.1", "localhost", 1)); !errors.Is(err, errEgressRefused) {
		t.Fatalf("a name resolving to loopback: %v", err)
	}
	res, err := (Egress{AllowPrivate: true}).Client(2 * time.Second).Get(server.URL)
	if err != nil || res.StatusCode != 204 {
		t.Fatalf("with private networks allowed: %v", err)
	}
	res.Body.Close()
	// Neither is IPv6 loopback.
	if v6 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })); true {
		if listener, err := net.Listen("tcp", "[::1]:0"); err == nil {
			v6.Listener = listener
			v6.Start()
			defer v6.Close()
			if _, err := (Egress{}).Client(2 * time.Second).Get(v6.URL); !errors.Is(err, errEgressRefused) {
				t.Fatalf("IPv6 loopback under the default policy: %v", err)
			}
		}
	}
	// Redirects are never followed.
	redirect := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/", http.StatusFound))
	defer redirect.Close()
	res, err = (Egress{AllowPrivate: true}).Client(2 * time.Second).Get(redirect.URL)
	if err != nil || res.StatusCode != http.StatusFound {
		t.Fatalf("a redirect was followed: %v", err)
	}
	res.Body.Close()
}
