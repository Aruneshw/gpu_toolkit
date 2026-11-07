package cli

import (
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
	"sync"
	"time"
)

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
	port := 7443
	if err := flags("host", args, func(f *flag.FlagSet) {
		f.StringVar(&listen, "listen", listen, "literal local address and port")
		f.StringVar(&advertise, "advertise", "", "reachable HTTPS IP:port (defaults to listen)")
		f.BoolVar(&public, "public", public, "allow public IP group hosting")
		f.BoolVar(&auto, "auto", false, "pick the best local address automatically")
		f.IntVar(&port, "port", port, "port used by --auto")
		f.StringVar(&ui, "ui", ui, "loopback dashboard address")
		f.DurationVar(&ttl, "ttl", ttl, "invitation expiry")
	}); err != nil {
		return err
	}
	explicitListen := listen != c.Listen
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if auto || !explicitListen || unspecified(hostOf(listen)) {
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
			return errors.New("group address is part of its identity; use a new --home to change it")
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
	output := &synchronizedWriter{w: out}
	fmt.Fprintf(output, "Group host: %s\nPrivate single-use invitation (expires in %s):\n%s\n", advertise, ttl, code)
	fmt.Fprintf(output, "Dashboard: http://%s/?token=%s\n", panel.Listen(), panel.Token())
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
