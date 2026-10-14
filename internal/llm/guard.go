package llm

import (
	"net"
	"net/netip"
	"syscall"
)

// privateError is the guard's refusal of an address.
type privateError struct {
	ip string // the address refused, or what the dialer passed
}

func (e *privateError) Error() string { return "refused private address " + e.ip }

// cgnat is the shared address space carrier-grade NAT uses (RFC 6598).
// netip does not count it as private, but it is not on the internet
// either, and some clouds put internal services there.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// thisNetwork is 0.0.0.0/8. Linux connects 0.0.0.0 to the machine
// itself, and the rest of the block is no public address.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// nat64 and sixToFour are IPv6 ranges that carry an IPv4 address
// inside. A gateway on the server's network may turn such an address
// into the IPv4 one, so isPrivate checks the IPv4 address too.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052, IPv4 in the last 4 bytes
	sixToFour = netip.MustParsePrefix("2002::/16")    // RFC 3056, IPv4 in bytes 2 to 5
)

// embeddedIPv4 returns the IPv4 address inside a NAT64 or 6to4 address.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// isPrivate reports whether ip is an address the server must not call
// unless the operator allows it: loopback, private (RFC 1918 and IPv6
// unique local), link-local (this includes the cloud metadata address
// 169.254.169.254), unspecified, multicast and carrier-grade NAT. An
// IPv4 address written as IPv6 (::ffff:127.0.0.1) counts as IPv4, and
// so does the IPv4 address inside a NAT64 or 6to4 address.
func isPrivate(ip netip.Addr) bool {
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok && isPrivate(v4) {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnat.Contains(ip) ||
		thisNetwork.Contains(ip)
}

// refusePrivate is a net.Dialer Control function. It runs after DNS,
// with the IP address about to be dialed, so a name that resolves to a
// private address is refused, even when it resolved to a public one
// when the model was saved.
func refusePrivate(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return &privateError{ip: address}
	}
	// The dialer always passes an IP address. Anything else is refused,
	// so a surprise cannot get through.
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return &privateError{ip: host}
	}
	if isPrivate(ip) {
		return &privateError{ip: ip.Unmap().String()}
	}
	return nil
}
