package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/dashboard"
	"meshcompute/internal/relay"
	"meshcompute/internal/security"
	"net"
	"net/url"
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
	if unusableIPv6()[ip.String()] {
		return false // deprecated: the network no longer routes it
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
	stale := unusableIPv6()
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
			if stale[ip.String()] {
				continue
			}
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
	direct := false
	mesh := ""
	poolModel := ""
	relayServer := c.Relay
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
		f.StringVar(&relayServer, "relay", relayServer, "public TCP relay to host through, e.g. bore.pub (works from any network)")
		f.BoolVar(&direct, "direct", false, "host directly on this computer's IP instead of a relay")
		f.StringVar(&mesh, "mesh", "", "host on a mesh VPN: auto, tailscale, wireguard, an interface name or a VPN IP")
		f.StringVar(&poolModel, "pool", "", "registered model to serve across every shared GPU")
	}); err != nil {
		return err
	}
	if direct {
		relayServer = ""
	}
	meshName := ""
	if mesh == "" && !direct && !auto && advertise == "" && listen == c.Listen && c.Mesh != "" && c.GroupHost && localAddress(hostOf(c.Listen)) {
		// The group already lives on a mesh address that is still up: stay there.
		mesh = hostOf(c.Listen)
	}
	if mesh == "" && !direct && !auto && advertise == "" && listen == c.Listen && c.Mesh != "" && c.GroupHost && relayServer == "" {
		return fmt.Errorf("this group is hosted on %s, but that VPN address (%s) is not up on this computer\n  connect the VPN and start again, or move the group with --mesh auto, --relay bore.pub or --direct --auto (friends then need a new invitation)", c.Mesh, hostOf(c.Listen))
	}
	if mesh != "" {
		// Mesh mode: listen on the VPN address. Members of the same Tailscale
		// tailnet or WireGuard network connect directly; no relay is involved.
		m, err := pickMesh(mesh)
		if err != nil {
			return err
		}
		if port < 1 || port > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
		if hostOf(c.Listen) == m.IP && c.GroupHost {
			listen = c.Listen // keep the group's port
		} else {
			listen = net.JoinHostPort(m.IP, strconv.Itoa(port))
		}
		advertise = "https://" + listen
		if ip := net.ParseIP(m.IP); config.RemoteEndpoint(advertise, false) != nil && ip.IsGlobalUnicast() {
			public = true // a VPN that hands out public-range addresses
		} else {
			public = false
		}
		relayServer, meshName = "", m.label()
		fmt.Fprintf(out, "Mesh: hosting on %s (%s). Computers on the same VPN connect directly.\n", m.IP, meshName)
	}
	explicitListen := listen != c.Listen
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	var listener net.Listener
	var tunnel *relay.Tunnel
	var session *relay.Session
	if relayServer != "" {
		// Relay mode: listen on loopback only and let the relay carry visitors.
		// Address changes of this computer no longer matter.
		// The local port is private to this computer; if it is busy, reuse the
		// previous one or take any free port (the certificate covers IPs only).
		var l net.Listener
		var err error
		for _, candidate := range []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), c.Listen, "127.0.0.1:0"} {
			if ip := net.ParseIP(hostOf(candidate)); ip == nil || !ip.IsLoopback() {
				continue
			}
			if l, err = net.Listen("tcp", candidate); err == nil {
				break
			}
		}
		if l == nil {
			return fmt.Errorf("cannot listen on a local port: %w", err)
		}
		listener = l
		listen = l.Addr().String()
		defer listener.Close()
		relayIP, err := resolveRelay(ctx, relayServer)
		if err != nil {
			return err
		}
		want := c.RelayPort
		if c.Relay != relayServer || want == 0 {
			want = randomRelayPort()
		}
		tunnel = &relay.Tunnel{Server: relayServer, Port: want, Local: listen, Log: log.New(out, "", log.LstdFlags|log.LUTC)}
		session, err = openRelay(ctx, tunnel, out)
		if err != nil {
			return err
		}
		defer session.Close()
		advertise = "https://" + net.JoinHostPort(relayIP, strconv.Itoa(session.Port))
		public = true
		fmt.Fprintf(out, "Relay: %s:%d -> this computer (works from any network; traffic stays end-to-end encrypted).\n", relayServer, session.Port)
	}
	// Keep an existing group's address while this computer still owns it, so a
	// restart does not invalidate invitations or enrolled workers.
	oldIP := net.ParseIP(hostOf(c.Listen))
	keep := (auto || !explicitListen) && c.GroupHost && localAddress(hostOf(c.Listen)) && advertise == "" &&
		!(auto && oldIP != nil && oldIP.IsLoopback())
	if _, p, err := net.SplitHostPort(c.Listen); keep && err == nil && p != strconv.Itoa(port) && auto {
		keep = false
	}
	if relayServer != "" {
		keep = false
	}
	if relayServer != "" || meshName != "" {
		// listen and advertise were chosen by the relay or the mesh above.
	} else if keep {
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
	next.Relay, next.RelayPort, next.Mesh = "", 0, meshName
	if poolModel != "" {
		next.PoolModel = poolModel
		if next.PoolContext == 0 {
			next.PoolContext = agent.DefaultPoolContext
		}
	}
	if session != nil {
		next.Relay, next.RelayPort = relayServer, session.Port
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return errors.New("invitation expiry must be positive and at most 24h")
	}
	// Bind before generating a new identity so an occupied address fails cleanly.
	if listener == nil {
		l, err := net.Listen("tcp", listen)
		if err != nil {
			return err
		}
		listener = l
		defer listener.Close()
	}
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
		if !sameIPs(c.Listen, c.Advertise, listen, advertise) || c.Public != public {
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
	panel.EnableTasks(ctx)
	output := &synchronizedWriter{w: out}
	fmt.Fprintf(output, "Group host: %s\nPrivate single-use invitation (expires in %s):\n%s\n", advertise, ttl, code)
	fmt.Fprintf(output, "Dashboard: http://%s/?token=%s\n", panel.Listen(), panel.Token())
	fmt.Fprintln(output, "  Open it for live GPU/VRAM totals, one-click invitations for friends, and a built-in terminal.")
	fmt.Fprintln(output, "Friend on Windows: extract the release ZIP, double-click win.bat, choose 2 and paste the invitation above.")
	fmt.Fprintln(output, "Friend on Linux: ./linux.sh join <invitation>")
	hostIP := hostOf(listen)
	ip := net.ParseIP(hostIP)
	if session != nil {
		fmt.Fprintf(output, "Reachability: public relay %s:%d. Friends on any network can join: no IPv6, firewall or router setup needed.\n", relayServer, session.Port)
	} else if meshName != "" {
		_, port, _ := net.SplitHostPort(listen)
		fmt.Fprintf(output, "Reachability: %s. Only computers on the same VPN can join; they need nothing else (allow TCP %s on the VPN interface if a firewall is enabled).\n", meshName, port)
	} else if ip != nil && ip.IsLoopback() {
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
	if session != nil {
		// Hold the public port for the life of the host; after a drop the same
		// port is requested again because invitations and workers refer to it.
		go func() {
			err := session.Serve(ctx)
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(output, "Relay connection dropped (%v); reconnecting to keep %s:%d...\n", err, relayServer, session.Port)
			tunnel.Port = session.Port
			_ = tunnel.Keep(ctx, func(p int) {
				fmt.Fprintf(output, "Relay reconnected: %s:%d. Friends reconnect automatically.\n", relayServer, p)
			})
		}()
	}
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
	code, stdin, noStart, open := "", false, false, true
	const joinUsage = "usage: join [CODE|--stdin] [--no-start] [--no-open] [--allow-terminal|--no-terminal] [--tasks ask|auto|off] [--no-share-gpu]"
	changed := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-open":
			open = false
		case a == "--tasks" && i+1 < len(args):
			i++
			switch args[i] {
			case "ask", "auto", "off":
				c.TaskApproval, changed = args[i], true
			default:
				return errors.New("--tasks must be ask, auto or off")
			}
		case a == "--allow-tasks":
			c.TaskApproval, changed = "auto", true
		case a == "--stdin":
			stdin = true
		case a == "--no-start":
			noStart = true
		case a == "--allow-terminal":
			c.AllowTerminal, changed = true, true
		case a == "--no-terminal":
			c.AllowTerminal, changed = false, true
		case a == "--no-share-gpu":
			c.ShareGPU, changed = "off", true
		case a == "--share-gpu":
			c.ShareGPU, changed = "", true
		case strings.HasPrefix(a, "-") || code != "":
			return errors.New(joinUsage)
		default:
			code = a
		}
	}
	if stdin && code != "" {
		return errors.New(joinUsage)
	}
	if changed {
		if err := config.Save(dir, c); err != nil {
			return err
		}
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
					return serveWorker(ctx, dir, c, out, open)
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
	return serveWorker(ctx, dir, c, out, open)
}

// serveWorker runs this worker together with its own dashboard, where the
// owner sees what the host does here and approves tasks.
func serveWorker(ctx context.Context, dir string, c config.Config, out io.Writer, open bool) error {
	out = &synchronizedWriter{w: out}
	a := agent.New(dir, c, out)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if listen := workerDashboardAddress(); listen != "" {
		if panel, err := dashboard.NewWorker(a, listen, ""); err == nil {
			link := fmt.Sprintf("http://%s/?token=%s", panel.Listen(), panel.Token())
			a.SetDashboardURL(link)
			go func() {
				if err := panel.Serve(ctx); err != nil {
					fmt.Fprintf(out, "Worker dashboard stopped: %v\n", err)
				}
			}()
			fmt.Fprintf(out, "This computer's dashboard: %s\n  It shows what the group host does here, asks you to approve tasks, and has your on/off switches.\n", link)
			if open && openBrowser(link) {
				fmt.Fprintln(out, "Opened it in your browser.")
			}
		}
	}
	return a.Serve(ctx)
}

// workerDashboardAddress picks a free loopback port for the worker
// dashboard: MESHCOMPUTE_WORKER_UI, or the first free one from 8082.
func workerDashboardAddress() string {
	if v := os.Getenv("MESHCOMPUTE_WORKER_UI"); v != "" {
		if v == "off" {
			return ""
		}
		return v
	}
	for port := 8082; port < 8100; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if l, err := net.Listen("tcp", addr); err == nil {
			l.Close()
			return addr
		}
	}
	return ""
}

// resolveRelay returns the relay's IPv4 address; invitations carry literal IPs.
func resolveRelay(ctx context.Context, server string) (string, error) {
	if ip := net.ParseIP(server); ip != nil {
		return ip.String(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", server)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("cannot find relay %s; check the internet connection, or host without a relay using --direct", server)
	}
	return ips[0].String(), nil
}

// randomRelayPort picks a public port for a new group; it is saved and reused.
func randomRelayPort() int {
	n, err := rand.Int(rand.Reader, big.NewInt(40000))
	if err != nil {
		return 0
	}
	return 20000 + int(n.Int64())
}

// openRelay claims the group's public port. A port still held by this host's
// previous run is released by the relay within seconds, so it is retried
// before falling back to a new port (which would require new invitations).
func openRelay(ctx context.Context, t *relay.Tunnel, out io.Writer) (*relay.Session, error) {
	for attempt := 1; ; attempt++ {
		s, err := t.Open(ctx)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, relay.ErrPortInUse) || t.Port == 0 {
			return nil, fmt.Errorf("%w\n  check the internet connection, or host without a relay using --direct", err)
		}
		if attempt >= 6 {
			fmt.Fprintf(out, "Relay port %d is taken by someone else; choosing a new one (send friends a new invitation).\n", t.Port)
			t.Port = randomRelayPort()
			attempt = 0
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// sameIPs reports whether two listen/advertise pairs use the same IPs. The
// group certificate names IPs only, so a port change needs no new certificate.
func sameIPs(oldListen, oldAdvertise, listen, advertise string) bool {
	host := func(endpoint string) string {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			return u.Hostname()
		}
		return hostOf(endpoint)
	}
	return hostOf(oldListen) == hostOf(listen) && host(oldAdvertise) == host(advertise)
}
