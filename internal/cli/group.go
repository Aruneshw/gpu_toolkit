package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/security"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// localAddress reports whether ip is currently assigned to an up interface.
func localAddress(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// autoAddress picks the address a group host should advertise. A globally
// routable IPv6 address is preferred because it needs no router port mapping.
func autoAddress() (address string, public bool, reason string, err error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false, "", err
	}
	var v6, v4 string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			switch {
			case ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast():
				if v6 == "" {
					v6 = ip.String()
				}
			case ip.To4() != nil && ip.IsPrivate() && !ip.IsUnspecified():
				if v4 == "" {
					v4 = ip.String()
				}
			}
		}
	}
	if v6 != "" {
		return v6, true, "global IPv6; friends connect directly without router port forwarding", nil
	}
	if v4 != "" {
		return v4, false, "private IPv4; remote friends need TCP port forwarding on the router", nil
	}
	return "", false, "", errors.New("no usable network address found; pass --listen and --advertise")
}

func unspecified(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func hostOf(endpoint string) string {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return ""
	}
	return host
}

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(b)
}

func hostGroup(ctx context.Context, dir string, c config.Config, args []string, out io.Writer) error {
	listen, advertise, public := c.Listen, c.Advertise, c.Public
	ui := "127.0.0.1:8081"
	ttl := 5 * time.Minute
	auto := false
	open := false
	port := 7443
	if err := flags("host", args, func(f *flag.FlagSet) {
		f.StringVar(&listen, "listen", listen, "literal local address and port")
		f.StringVar(&advertise, "advertise", "", "reachable HTTPS IP:port (defaults to listen)")
		f.BoolVar(&public, "public", public, "allow public IP group hosting")
		f.BoolVar(&auto, "auto", false, "pick the best local address automatically")
		f.IntVar(&port, "port", port, "port used by --auto")
		f.StringVar(&ui, "ui", ui, "loopback dashboard address")
		f.DurationVar(&ttl, "ttl", ttl, "invitation expiry")
		f.BoolVar(&open, "open", false, "open the dashboard in the default browser")
	}); err != nil {
		return err
	}
	explicitListen := listen != c.Listen
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	// Keep an existing group's address while this computer still owns it, so a
	// restart does not invalidate invitations or enrolled workers.
	oldIP := net.ParseIP(hostOf(c.Listen))
	keep := (auto || !explicitListen) && c.GroupHost && localAddress(hostOf(c.Listen)) && advertise == "" &&
		!(auto && oldIP != nil && oldIP.IsLoopback())
	if _, p, err := net.SplitHostPort(c.Listen); keep && err == nil && p != strconv.Itoa(port) && auto {
		keep = false
	}
	if keep {
		listen, advertise, public = c.Listen, c.Advertise, c.Public
		fmt.Fprintf(out, "Resuming group on %s.\n", listen)
	} else if auto || !explicitListen || unspecified(hostOf(listen)) {
		address, isPublic, reason, err := autoAddress()
		if err != nil {
			return err
		}
		if auto || !explicitListen {
			listen = net.JoinHostPort(address, strconv.Itoa(port))
			public = isPublic
			fmt.Fprintf(out, "Selected %s (%s).\n", listen, reason)
			if advertise == "" {
				advertise = "https://" + listen
			}
		} else {
			// Keep the user's port but replace an unroutable wildcard address.
			_, chosenPort, splitErr := net.SplitHostPort(listen)
			if splitErr != nil {
				chosenPort = strconv.Itoa(port)
			}
			listen = net.JoinHostPort(address, chosenPort)
			public = isPublic
			fmt.Fprintf(out, "Replaced wildcard bind with %s (%s).\n", listen, reason)
		}
	}
	if advertise == "" {
		if listen == c.Listen {
			advertise = c.Advertise
		} else {
			advertise = "https://" + listen
		}
	}
	next := c
	next.Listen, next.Advertise, next.Public, next.GroupHost = listen, advertise, public, true
	if err := next.Validate(); err != nil {
		return err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return errors.New("invitation expiry must be positive and at most 24h")
	}
	// Bind before generating a new identity so an occupied address fails cleanly.
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	if _, err := os.Lstat(filepath.Join(dir, "identity.json")); os.IsNotExist(err) {
		if err := security.InitGroup(dir, listen, advertise, public); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		host, reverse, _, err := security.Group(dir)
		if err != nil {
			return err
		}
		if !host || !reverse {
			return errors.New("this directory contains an existing non-group identity; use --home with a new directory for the group")
		}
		if c.Listen != listen || c.Advertise != advertise || c.Public != public {
			// The address is part of the certificate. Re-issue it with the same
			// group CA instead of stranding the group (mobile/home IPv6 prefixes
			// change often).
			if err := security.RetargetGroup(dir, listen, advertise, public); err != nil {
				return fmt.Errorf("could not move the group to %s: %w", advertise, err)
			}
			fmt.Fprintf(out, "NOTICE: this computer's address changed from %s to %s.\n", c.Advertise, advertise)
			fmt.Fprintln(out, "        The group certificate was re-issued for the new address. Old invitations no longer work,")
			fmt.Fprintln(out, "        and friends who joined before must run join again with a NEW invitation.")
		}
	}
	if err := config.Save(dir, next); err != nil {
		return err
	}
	code, err := security.Pair(dir, advertise, ttl)
	if err != nil {
		return err
	}
	panel, err := dashboard.New(dashboard.NewRemote(dir, next), ui, "")
	if err != nil {
		return err
	}
	panel.EnableTerminal(dir)
	output := &synchronizedWriter{w: out}
	fmt.Fprintf(output, "Group host: %s\nPrivate single-use invitation (expires in %s):\n%s\n", advertise, ttl, code)
	fmt.Fprintf(output, "Dashboard: http://%s/?token=%s\n", panel.Listen(), panel.Token())
	fmt.Fprintln(output, "  Open it for live GPU/VRAM totals, one-click invitations for friends, and a built-in terminal.")
	fmt.Fprintln(output, "Friend on Windows: extract the release ZIP, double-click win.bat, choose 2 and paste the invitation above.")
	fmt.Fprintln(output, "Friend on Linux: ./linux.sh join <invitation>")
	hostIP := hostOf(listen)
	ip := net.ParseIP(hostIP)
	if ip != nil && ip.IsLoopback() {
		fmt.Fprintln(output, "Loopback host: only this computer can connect. Use --auto to select a reachable address.")
	} else if ip != nil && ip.To4() != nil && ip.IsPrivate() {
		_, port, _ := net.SplitHostPort(listen)
		fmt.Fprintf(output, "Reachability: %s is a private address. Friends on other networks need TCP %s forwarded on the router to %s.\n", hostIP, port, listen)
	} else if ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() {
		_, port, _ := net.SplitHostPort(listen)
		fmt.Fprintf(output, "Reachability: %s is a global IPv6 address. No port forwarding is needed; allow TCP %s through the firewall if one is enabled.\n", hostIP, port)
	}
	fmt.Fprintf(output, "Keep this process running (Ctrl-C stops it); the friend only needs the invitation above.\n")
	if open {
		if openBrowser(fmt.Sprintf("http://%s/?token=%s", panel.Listen(), panel.Token())) {
			fmt.Fprintln(output, "Opened the dashboard in your browser.")
		} else {
			fmt.Fprintln(output, "Could not open a browser automatically; open the Dashboard link above.")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- agent.New(dir, next, output).ServeListener(ctx, listener) }()
	go func() { results <- panel.Serve(ctx) }()
	first := <-results
	cancel()
	second := <-results
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, context.Canceled) {
		return second
	}
	return nil
}

func readInvitation(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if len(line) > 32768 {
		return "", errors.New("invitation too large")
	}
	// A pasted whole command (".\join.cmd CODE") also works: the invitation is
	// always the last word.
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	return strings.Trim(fields[len(fields)-1], `"'`), nil
}

// joinGroup enrolls this computer as a worker and, for creator-hosted groups,
// keeps it connected. An existing worker identity is replaced only after the
// new enrollment succeeds and is kept as a backup file.
func joinGroup(ctx context.Context, dir string, c config.Config, args []string, in io.Reader, out io.Writer) error {
	code, stdin, noStart := "", false, false
	for _, a := range args {
		switch {
		case a == "--stdin":
			stdin = true
		case a == "--no-start":
			noStart = true
		case strings.HasPrefix(a, "-") || code != "":
			return errors.New("usage: join [CODE|--stdin] [--no-start]")
		default:
			code = a
		}
	}
	if stdin && code != "" {
		return errors.New("usage: join [CODE|--stdin] [--no-start]")
	}
	if in == nil {
		in = strings.NewReader("")
	}
	reader := bufio.NewReader(io.LimitReader(in, 32769))
	if stdin {
		b, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		if len(b) > 32768 {
			return errors.New("invitation too large")
		}
		code = strings.TrimSpace(string(b))
	} else if code == "" {
		if isHost, reverse, _, err := security.Group(dir); err == nil {
			if isHost {
				return errors.New("this --home is a group host; join from a different --home directory")
			}
			if reverse {
				endpoint, _ := security.HostEndpoint(dir)
				fmt.Fprintf(out, "This computer already belongs to the group at %s.\n", endpoint)
				fmt.Fprintln(out, "Press Enter to reconnect to it, or paste a NEW invitation to join a new or moved group:")
				line, err := readInvitation(reader)
				if err != nil {
					return err
				}
				if line == "" {
					fmt.Fprintln(out, "Reconnecting. Press Ctrl-C to stop.")
					return agent.New(dir, c, out).Serve(ctx)
				}
				code = line
			}
		}
		if code == "" {
			fmt.Fprintln(out, "Paste the private invitation and press Enter:")
			line, err := readInvitation(reader)
			if err != nil {
				return err
			}
			code = line
		}
	}
	if code == "" {
		return errors.New("no invitation provided; ask the group host for one")
	}
	host, _, _ := net.SplitHostPort(c.Listen)
	backup, err := security.RejoinHost(ctx, dir, code, host)
	if err != nil {
		return joinFailure(code, err)
	}
	if backup != "" {
		fmt.Fprintf(out, "Replaced the previous group membership (backup: %s).\n", backup)
	}
	_, reverse, _, err := security.Group(dir)
	if err != nil {
		return err
	}
	id, _ := security.NodeID(dir)
	if !reverse {
		_, err = fmt.Fprintf(out, "Enrolled as %s. Start this worker and add its VPN endpoint on the coordinator with peer add.\n", id)
		return err
	}
	fmt.Fprintf(out, "Joined the group as %s. Connections are outbound and reconnect automatically.\n", id)
	if noStart {
		_, err = fmt.Fprintln(out, "Start the worker later with: meshcompute start")
		return err
	}
	fmt.Fprintln(out, "Starting the worker. Keep this window open; press Ctrl-C to stop.")
	return agent.New(dir, c, out).Serve(ctx)
}
