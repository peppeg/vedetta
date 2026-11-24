package reader

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrPrivateAddress: the address is of this machine or of its network. No reader of the
// chain is tried after it: one that runs elsewhere and follows redirects by itself would
// go there (external audit 24/11/2026).
var ErrPrivateAddress = errors.New("SSRF guard")

var privateIPBlocks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"0.0.0.0/8",      // "this network": 0.0.0.0 reaches the machine itself
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // RFC3927 link-local
		"100.64.0.0/10",  // RFC6598 shared address space: carrier NAT, and the Tailscale network
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local
		"fec0::/10",      // IPv6 site-local, deprecated
		"2001::/32",      // Teredo
		"240.0.0.0/4",    // reserved
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

func ValidateSafeURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("unsupported scheme %q, only http/https allowed", scheme)
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("missing hostname")
	}

	// Direct IP check
	if ip := net.ParseIP(hostname); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("access to private IP %s is blocked (%w)", ip, ErrPrivateAddress)
		}
		return nil
	}

	// DNS resolution check
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	if err != nil {
		return fmt.Errorf("failed to resolve host %s: %w", hostname, err)
	}

	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("host %s resolves to private IP %s (%w)", hostname, ip, ErrPrivateAddress)
		}
	}

	return nil
}

const lookupTimeout = 10 * time.Second

// innerIPv4 is the IPv4 address written inside an IPv6 one that leads to it (NAT64,
// 6to4, the old "::a.b.c.d"), nil for the others. A network with NAT64 reaches every
// IPv4 site that way: it is the address inside that counts, not the prefix.
func innerIPv4(ip net.IP) net.IP {
	if ip.To4() != nil || len(ip) != net.IPv6len {
		return nil
	}
	switch {
	case ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b, // 64:ff9b::/96, 64:ff9b:1::/48
		allZero(ip[:12]): // ::a.b.c.d
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	case ip[0] == 0x20 && ip[1] == 0x02: // 2002::/16
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func isPrivateIP(ip net.IP) bool {
	if v4 := innerIPv4(ip); v4 != nil && isPrivateIP(v4) {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// safeDialControl runs when a reader connects, with the address actually dialled:
// the name was resolved again after ValidateSafeURL, and a redirect may have led
// elsewhere. A private address is refused here too (Piano dopo audit 1.1 / S1).
func safeDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("connection to %q refused (SSRF guard): %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("connection to %q refused (SSRF guard): not an IP address", address)
	}
	if isPrivateIP(ip) {
		return fmt.Errorf("connection to private IP %s refused (%w)", ip, ErrPrivateAddress)
	}
	return nil
}

// safeTransport is the transport of the readers that open pages themselves (http,
// pdf): the default one, with safeDialControl on every connection.
func safeTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   safeDialControl,
	}).DialContext
	return t
}
