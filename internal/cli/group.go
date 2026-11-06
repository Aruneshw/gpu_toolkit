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
	"sync"
	"time"
)

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
	if err := flags("host", args, func(f *flag.FlagSet) {
		f.StringVar(&listen, "listen", listen, "literal local address and port")
		f.StringVar(&advertise, "advertise", "", "reachable HTTPS IP:port (defaults to listen)")
		f.BoolVar(&public, "public", public, "allow public IP group hosting")
		f.StringVar(&ui, "ui", ui, "loopback dashboard address")
		f.DurationVar(&ttl, "ttl", ttl, "invitation expiry")
	}); err != nil {
		return err
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
	fmt.Fprintf(output, "Dashboard: http://%s/?token=%s\nKeep this process running. Ctrl-C stops this group host.\n", panel.Listen(), panel.Token())
	hostIP, _, _ := net.SplitHostPort(listen)
	if net.ParseIP(hostIP).IsLoopback() {
		fmt.Fprintln(output, "Loopback host: only this computer can connect. Select a reachable --listen address for remote friends.")
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
