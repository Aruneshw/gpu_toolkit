package network

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// MeshAddress is one address on a mesh-VPN interface (Tailscale, WireGuard,
// ZeroTier, Nebula, NetBird). Computers on the same mesh reach it directly,
// whatever networks they are physically on.
type MeshAddress struct {
	Interface string `json:"interface"`
	Kind      string `json:"kind"`
	IP        string `json:"ip"`
}

var (
	cgnat       = mustCIDR("100.64.0.0/10")       // Tailscale IPv4
	tailscaleV6 = mustCIDR("fd7a:115c:a1e0::/48") // Tailscale IPv6
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// MeshKind classifies an interface by its kernel device type, its name and
// its addresses.
func MeshKind(iface net.Interface, ips []net.IP) string {
	name := strings.ToLower(iface.Name)
	if b, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "uevent")); err == nil && strings.Contains(string(b), "DEVTYPE=wireguard") {
		// Tailscale can also run on kernel WireGuard; its addresses decide below.
		for _, ip := range ips {
			if cgnat.Contains(ip) || tailscaleV6.Contains(ip) {
				return "tailscale"
			}
		}
		return "wireguard"
	}
	switch {
	case strings.Contains(name, "tailscale"):
		return "tailscale"
	case strings.HasPrefix(name, "wg"), strings.Contains(name, "wireguard"):
		return "wireguard"
	case strings.HasPrefix(name, "zt"), strings.Contains(name, "zerotier"):
		return "zerotier"
	case strings.Contains(name, "nebula"):
		return "nebula"
	case strings.HasPrefix(name, "wt"), strings.Contains(name, "netbird"):
		return "netbird"
	}
	// Tailscale's tunnel is point-to-point and uses its reserved ranges. A
	// carrier-NAT Wi-Fi address in 100.64/10 is not point-to-point.
	if iface.Flags&net.FlagPointToPoint != 0 {
		for _, ip := range ips {
			if cgnat.Contains(ip) || tailscaleV6.Contains(ip) {
				return "tailscale"
			}
		}
	}
	return ""
}

// MeshAddresses lists usable mesh addresses, IPv4 first.
func MeshAddresses() []MeshAddress {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []MeshAddress
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() && !n.IP.IsUnspecified() {
				ips = append(ips, n.IP)
			}
		}
		kind := MeshKind(iface, ips)
		if kind == "" {
			continue
		}
		for _, ip := range ips {
			m := MeshAddress{Interface: iface.Name, Kind: kind, IP: ip.String()}
			if ip.To4() != nil {
				v4 = append(v4, m)
			} else {
				v6 = append(v6, m)
			}
		}
	}
	return append(v4, v6...)
}

// IsMeshIP reports whether ip is an address of this computer's mesh VPN
// interfaces, or in Tailscale's reserved ranges.
func IsMeshIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if tailscaleV6.Contains(ip) {
		return true
	}
	for _, m := range MeshAddresses() {
		if net.ParseIP(m.IP).Equal(ip) {
			return true
		}
	}
	return false
}
