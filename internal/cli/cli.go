// Package cli implements commands without global flag or process state.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"meshcompute/internal/agent"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"net"
	"net/url"
	"strings"
	"time"
)

const usage = `MeshCompute — separate devices, authenticated inference
Usage: meshcompute [--home DIR] COMMAND
  install                         initialize user configuration (no downloads)
  config set KEY VALUE            listen | backend_url (before cluster creation)
  cluster create                  create private CA and coordinator identity
  host [--auto|--listen IP:7443] [--public] [--advertise https://IP:7443]
                                  create/resume a group, show invite and dashboard
  pair [--ttl 5m]                 issue a secret, one-use invitation
  join [CODE|--stdin]             paste invite; interactive join starts the worker
  revoke NODE_ID                  revoke an enrolled node on coordinator
  peer add NAME HTTPS_ENDPOINT    remember an explicitly enrolled worker
  start                           run the HTTPS agent in the foreground
  status                          local and configured peer resources
  monitor [--interval 5s]          periodically show status until Ctrl-C
  ui [--listen 127.0.0.1:8081]     token-protected localhost dashboard
  doctor                          shareable redacted diagnostics
  backend list                    detect local llama.cpp backend
  backend serve NAME [options]    launch a registered local GGUF
  model add NAME PATH             register local GGUF, no implicit downloads
  model list                      list registered local models
  run NAME [--node NAME] [--prompt TEXT] [--max-tokens 128] [--context 2048]
  network probe NAME              measure authenticated request RTT/throughput
  simulate                        labeled scheduling examples; no inference
  version                         print build version
See docs/host-first-design.md for group hosting and address selection.
`

func writeJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
// joinFailure separates "the host is unreachable" from "the invitation was
// refused" so users are not told to check the wrong thing.
func joinFailure(code string, cause error) error {
	endpoint, _, err := security.InvitationEndpoint(code)
	if err != nil {
		return fmt.Errorf("join failed: the invitation is malformed or truncated; paste the whole code\n  caused by: %w", cause)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("join failed: the invitation has an invalid endpoint\n  caused by: %w", cause)
	}
	d := net.Dialer{Timeout: 8 * time.Second}
	conn, dialErr := d.Dial("tcp", u.Host)
	if dialErr != nil {
		return fmt.Errorf("join failed: cannot reach the group host at %s\n  the host must be running, the address must be current, and TCP must be allowed through its firewall and router\n  caused by: %v", u.Host, dialErr)
	}
	conn.Close()
	return fmt.Errorf("join failed: the host at %s was reachable but refused enrollment\n  the invitation is probably expired or already used; generate a new one on the host with: meshcompute pair\n  caused by: %w", u.Host, cause)
}

func flags(name string, args []string, setup func(*flag.FlagSet)) error {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	setup(f)
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", f.Arg(0))
	}
	return nil
}
func Run(ctx context.Context, args []string, in io.Reader, out io.Writer, version string) error {
	root := flag.NewFlagSet("meshcompute", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	home := root.String("home", "", "state directory")
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, e := io.WriteString(out, usage)
		return e
	}
	if e := root.Parse(args); e != nil {
		return e
	}
	args = root.Args()
	if len(args) == 0 {
		return errors.New("command required")
	}
	dir := *home
	if dir == "" {
		var e error
		dir, e = config.Dir()
		if e != nil {
			return e
		}
	}
	command := args[0]
	args = args[1:]
	if command == "version" {
		if len(args) != 0 {
			return errors.New("version takes no arguments")
		}
		_, e := fmt.Fprintln(out, version)
		return e
	}
	if command == "install" {
		if len(args) != 0 {
			return errors.New("install takes no arguments")
		}
		if e := config.Install(dir); e != nil {
			return e
		}
		_, e := fmt.Fprintln(out, "Configuration ready. No backend, driver, model, firewall rule, or service was installed.")
		return e
	}
	if command == "host" || command == "join" {
		if e := config.Install(dir); e != nil {
			return e
		}
	}
	c, e := config.Load(dir)
	if e != nil {
		return e
	}
	switch command {
	case "host":
		return hostGroup(ctx, dir, c, args, out)
	case "config":
		if len(args) != 3 || args[0] != "set" {
			return errors.New("usage: config set listen|backend_url VALUE")
		}
		switch args[1] {
		case "listen":
			if _, e := security.NodeID(dir); e == nil {
				return errors.New("listen is part of certificate identity; choose it before cluster create/join")
			}
			c.Listen = args[2]
			c.Advertise = "https://" + args[2]
		case "backend_url":
			c.BackendURL = args[2]
		default:
			return errors.New("unknown configuration key")
		}
		return config.Save(dir, c)
	case "cluster":
		if len(args) != 1 || args[0] != "create" {
			return errors.New("usage: cluster create")
		}
		host, _, _ := net.SplitHostPort(c.Listen)
		if e := security.Init(dir, host); e != nil {
			return e
		}
		_, e := fmt.Fprintln(out, "Private cluster created. Start the agent before enrolling workers.")
		return e
	case "pair":
		ttl := 5 * time.Minute
		if e := flags("pair", args, func(f *flag.FlagSet) { f.DurationVar(&ttl, "ttl", ttl, "expiry") }); e != nil {
			return e
		}
		code, e := security.Pair(dir, c.Advertise, ttl)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintln(out, code)
		return e
	case "join":
		if len(args) > 1 {
			return errors.New("usage: join [CODE|--stdin]")
		}
		interactive := len(args) == 0
		if interactive {
			if isHost, reverse, _, err := security.Group(dir); err == nil && reverse && !isHost {
				fmt.Fprintln(out, "Resuming this worker's group connection. Press Ctrl-C to stop.")
				return agent.New(dir, c, out).Serve(ctx)
			}
		}
		code := ""
		if interactive {
			fmt.Fprintln(out, "Paste the private invitation and press Enter:")
			line, err := bufio.NewReader(io.LimitReader(in, 32769)).ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			if len(line) > 32768 {
				return errors.New("invitation too large")
			}
			code = strings.TrimSpace(line)
		} else {
			code = args[0]
		}
		if code == "--stdin" {
			b, e := io.ReadAll(io.LimitReader(in, 32769))
			if e != nil {
				return e
			}
			if len(b) > 32768 {
				return errors.New("invitation too large")
			}
			code = strings.TrimSpace(string(b))
		}
		host, _, _ := net.SplitHostPort(c.Listen)
		if e := security.JoinHost(ctx, dir, code, host); e != nil {
			return joinFailure(code, e)
		}
		_, reverse, _, e := security.Group(dir)
		if e != nil {
			return e
		}
		if reverse {
			fmt.Fprintln(out, "Joined the group. Worker connections are outbound and reconnect automatically. Restart with meshcompute start.")
			if interactive {
				return agent.New(dir, c, out).Serve(ctx)
			}
			return nil
		}
		_, e = fmt.Fprintln(out, "Enrolled. Start this worker and add its VPN endpoint on the coordinator with peer add.")
		return e
	case "revoke":
		if len(args) != 1 {
			return errors.New("usage: revoke NODE_ID")
		}
		return security.Revoke(dir, args[0])
	case "peer":
		if len(args) != 3 || args[0] != "add" {
			return errors.New("usage: peer add NAME HTTPS_ENDPOINT")
		}
		c.Peers = append(c.Peers, config.Peer{Name: args[1], Endpoint: args[2]})
		return config.Save(dir, c)
	default:
		return operational(ctx, dir, c, command, args, in, out)
	}
}
