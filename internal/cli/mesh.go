package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// meshAddress is one address on a mesh-VPN interface (Tailscale, WireGuard,
// ZeroTier, Nebula, NetBird). Computers on the same mesh reach it directly,
// whatever networks they are physically on.
type meshAddress struct {
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

// meshKind classifies an interface by its kernel device type, its name and
// its addresses.
func meshKind(iface net.Interface, ips []net.IP) string {
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

// meshAddresses lists usable mesh addresses, IPv4 first.
func meshAddresses() []meshAddress {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []meshAddress
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
		kind := meshKind(iface, ips)
		if kind == "" {
			continue
		}
		for _, ip := range ips {
			m := meshAddress{Interface: iface.Name, Kind: kind, IP: ip.String()}
			if ip.To4() != nil {
				v4 = append(v4, m)
			} else {
				v6 = append(v6, m)
			}
		}
	}
	return append(v4, v6...)
}

// pickMesh resolves the --mesh argument: "auto", a kind ("tailscale",
// "wireguard", ...), an interface name, or a literal IP of this computer.
func pickMesh(want string) (meshAddress, error) {
	want = strings.TrimSpace(want)
	if ip := net.ParseIP(want); ip != nil {
		if !localAddress(ip.String()) {
			return meshAddress{}, fmt.Errorf("%s is not an address of this computer; is the VPN connected?", want)
		}
		m := meshAddress{Interface: "", Kind: "vpn", IP: ip.String()}
		for _, known := range meshAddresses() {
			if known.IP == m.IP {
				return known, nil
			}
		}
		return m, nil
	}
	all := meshAddresses()
	for _, m := range all {
		if want == "" || strings.EqualFold(want, "auto") || strings.EqualFold(want, m.Kind) || strings.EqualFold(want, m.Interface) {
			return m, nil
		}
	}
	if len(all) == 0 {
		return meshAddress{}, errors.New("no Tailscale, WireGuard or other mesh VPN interface is up on this computer\n  connect the VPN first (tailscale up / wg-quick up wg0), or name its address: host --mesh <VPN-IP>")
	}
	var names []string
	for _, m := range all {
		names = append(names, m.Interface+" ("+m.Kind+" "+m.IP+")")
	}
	return meshAddress{}, fmt.Errorf("no mesh interface matches %q; found: %s", want, strings.Join(names, ", "))
}

func (m meshAddress) label() string {
	if m.Interface == "" {
		return "VPN address " + m.IP
	}
	return m.Kind + " " + m.Interface
}
