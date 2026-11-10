//go:build linux

package cli

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

// Kernel IPv6 address flags from /proc/net/if_inet6.
const (
	ifaDADFailed  = 0x08
	ifaDeprecated = 0x20
	ifaTentative  = 0x40
)

// unusableIPv6 lists addresses the kernel still holds but that must not be
// advertised: deprecated ones (the network moved to a new prefix; mobile
// hotspots do this every few minutes), tentative and failed duplicates.
func unusableIPv6() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 6 || len(fields[0]) != 32 {
			continue
		}
		flags, err := strconv.ParseUint(fields[4], 16, 32)
		if err != nil || flags&(ifaDeprecated|ifaTentative|ifaDADFailed) == 0 {
			continue
		}
		b, err := hex.DecodeString(fields[0])
		if err != nil {
			continue
		}
		out[net.IP(b).String()] = true
	}
	return out
}
