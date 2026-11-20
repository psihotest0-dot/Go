package webfetch

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// errNotPublic is what a connection to a local or private address fails with.
var errNotPublic = errors.New("the address is not a public one")

// blockedPrefixes are the ranges that are not "private" to net/netip but are not the
// internet either: shared address space of carriers, protocol assignments, benchmarking,
// the reserved block, and the ways to write an IPv4 address inside an IPv6 one.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// isPublic tells whether ip is an address of the public internet. The address is the one
// the connection is really made to, after the name was resolved (see publicOnly).
func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	if ip.Is6() {
		b := ip.As16()
		// 64:ff9b::/96 (NAT64) carries an IPv4 address in its last four bytes, 2002::/16
		// (6to4) in bytes 2 to 5: the address behind them is the one that counts.
		if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]) {
			return isPublic(netip.AddrFrom4([4]byte(b[12:16])))
		}
		if b[0] == 0x20 && b[1] == 0x02 {
			return isPublic(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	return true
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// publicOnly is the Control function of the dialer: it runs for every connection, with the
// address the name was resolved to, and refuses one that is not public. Checking the name
// of the host is not enough - a name can point inside, and can point somewhere else the
// next time it is asked (rebinding) - and a redirect is a new connection, so it comes here
// too.
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errNotPublic
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !isPublic(ip) {
		return errNotPublic
	}
	return nil
}
