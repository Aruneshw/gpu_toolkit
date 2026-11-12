// Package cli implements commands without global flag or process state.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"meshcompute/internal/config"
	"meshcompute/internal/security"
	"net"
	"net/url"
	"strconv"
	"time"
)

const usage = `MeshCompute — separate devices, authenticated inference
Usage: meshcompute [--home DIR] COMMAND
  install                         initialize user configuration (no downloads)
  config set KEY VALUE            listen | backend_url | allow_terminal true|false |
                                  allow_hop true|false | task_approval ask|auto|off |
                                  share_gpu on|off | rpc_server PATH | llama_server PATH
  cluster create                  create private CA and coordinator identity
  host [--auto|--listen IP:7443] [--public] [--advertise https://IP:7443] [--open]
       [--mesh auto|tailscale|wireguard|IFACE|IP] [--pool MODEL]
                                  create/resume a group, show invite and dashboard;
                                  --mesh hosts on a Tailscale/WireGuard address
  mesh                            list this computer's mesh VPN addresses
  pair [--ttl 5m]                 issue a secret, one-use invitation
  join [CODE|--stdin] [--no-start] [--no-open] [--allow-terminal] [--allow-hop]
       [--tasks ask|auto|off] [--no-share-gpu]
                                  enroll with an invite (replaces an old worker
                                  identity, kept as a backup) and start the worker
                                  with its own dashboard; --allow-terminal lets the
                                  host open a shell here; --allow-hop lets other
                                  workers reach the host through this laptop (it
                                  forwards encrypted bytes it cannot read) and puts
                                  it on the host's latency map; --tasks decides
                                  whether commands the host sends need your approval
  topology                        host: latency map between the host and workers
  pool start MODEL [--name N] [--context N] [--slots N] [--workers ID,..]
       [--split local=1,ID=3] [--kv q8_0] [--kv-on-host] [--flash on] [--output-layer host] [--no-host-gpu]
                                  host: serve MODEL across shared GPUs (one pool
                                  per name; a worker lends its GPU to one pool)
  pool stop [NAME] | pool remove NAME | pool status
  model scan DIR                  list .gguf files in DIR (e.g. an external disk)
  revoke NODE_ID                  revoke an enrolled node on coordinator
  peer add NAME HTTPS_ENDPOINT    remember an explicitly enrolled worker
  start [--no-open]               run the agent (a worker also gets its dashboard)
  status                          local and configured peer resources
  monitor [--interval 5s]          periodically show status until Ctrl-C
  ui [--listen 127.0.0.1:8081] [--open]  token-protected localhost dashboard
  doctor                          shareable redacted diagnostics
  backend list                    detect local llama.cpp backend
  backend serve NAME [options]    launch a registered local GGUF
  model add NAME PATH             register local GGUF, no implicit downloads
  model list                      list registered local models
  run NAME [--node NAME] [--pool POOL] [--prompt TEXT] [--max-tokens 128] [--context 2048]
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
	return fmt.Errorf("join failed: the host at %s was reachable but refused enrollment\n  the invitation is expired, already used, or was created before the host's address changed\n  ask the host for a NEW invitation (dashboard: Invite a friend, or: meshcompute pair --ttl 1h)\n  caused by: %w", u.Host, cause)
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
			return errors.New("usage: config set listen|backend_url|allow_terminal|allow_hop|task_approval|share_gpu|rpc_server|llama_server VALUE")
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
		case "allow_terminal":
			on, e := strconv.ParseBool(args[2])
			if e != nil {
				return errors.New("allow_terminal must be true or false")
			}
			c.AllowTerminal = on
		case "allow_hop":
			on, e := strconv.ParseBool(args[2])
			if e != nil {
				return errors.New("allow_hop must be true or false")
			}
			c.AllowHop = on
		case "task_approval":
			switch args[2] {
			case "ask", "auto", "off":
				c.TaskApproval = args[2]
			default:
				return errors.New("task_approval must be ask, auto or off")
			}
		case "share_gpu":
			switch args[2] {
			case "on", "true", "auto":
				c.ShareGPU = ""
			case "off", "false":
				c.ShareGPU = "off"
			default:
				return errors.New("share_gpu must be on or off")
			}
		case "rpc_server":
			c.RPCServer = args[2]
		case "llama_server":
			c.LlamaServer = args[2]
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
		return joinGroup(ctx, dir, c, args, in, out)
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
