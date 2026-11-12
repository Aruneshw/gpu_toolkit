package cli

import (
	"errors"
	"fmt"
	"meshcompute/internal/network"
	"net"
	"strings"
)

// meshAddress is one mesh-VPN address of this computer (see network.MeshAddresses).
type meshAddress = network.MeshAddress

func meshAddresses() []meshAddress { return network.MeshAddresses() }

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

func meshLabel(m meshAddress) string {
	if m.Interface == "" {
		return "VPN address " + m.IP
	}
	return m.Kind + " " + m.Interface
}
