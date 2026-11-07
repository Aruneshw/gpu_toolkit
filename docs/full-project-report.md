# MeshCompute — the complete story, architecture, failures and rebuild guide

**Version of this document:** 2026-11-07
**Repository:** https://github.com/Aruneshw/gpu_toolkit
**Release:** https://github.com/Aruneshw/gpu_toolkit/releases/tag/v0.2.0
**Live test machines:** creator's Linux laptop (host) + friend's Windows 11 laptop (worker)

---

## 0. How to read this, and the shortest possible summary

This document is written to be read start to finish, but you can jump:

| If you want… | Read |
| --- | --- |
| The idea in plain English | §1 |
| How the system works, with diagrams | §2, §3 |
| What actually happened, in order | §4 |
| Why each connection attempt failed | §5, §6 |
| What to build next (architecture v2) | §7 |
| A clean rebuild, step by step | §8 |
| Every command, with purpose | §9 |
| "It doesn't work" decision tree | §10 |

**Short version.** MeshCompute lets one person host a private group on their own
laptop and let friends' laptops join it as GPU workers over the internet. It
uses its own certificate authority, single-use invitations, and a reverse TLS
tunnel — no Tailscale, no VPN, no central server. A request runs on exactly one
worker; VRAM is not pooled.

During the live test the friend's Windows laptop **did join successfully over
IPv6**, and the host saw its hardware (AMD Ryzen 5 7535HS, 16.3 GB RAM,
RTX 3050 Laptop 4.3 GB, driver 566.14). Later the worker process stopped (the
window was closed or the laptop slept), so the host started returning
`agent returned HTTP 503`. The GPU backend was never installed, so inference has
not run on that laptop yet. Every other connection path (cable, Wi-Fi, public
IPv4) failed for identifiable physical or network reasons documented below.

---

## 1. The problem we set out to solve

### 1.1 The idea

You have a laptop. Your friends have laptops with GPUs. You want to run a model
on their hardware from your machine, without asking them to install developer
tools, configure routers, or understand networking.

The original design goal was:

> The creator runs one command. Friends paste one invitation. Their GPUs appear
> in the creator's dashboard. The creator picks one and runs a model there.

### 1.2 Hard rules that shaped everything

These came from you, and they eliminated several obvious solutions:

1. **No Tailscale, no VPN, no separate networking product.** Everything had to
   be inside MeshCompute.
2. **No shared central relay.** Each group's creator hosts the relay themselves.
   Groups must be independent trust domains.
3. **Friends must not install Git or Go.** One PowerShell command, then paste an
   invitation.
4. **Windows and Linux.**
5. **Do not pretend VRAM is pooled.** One request = one node. No tensor
   splitting, no llama.cpp RPC.
6. **Dashboard is localhost-only and token-protected. No remote
   administration.**
7. **A working result, not an architecture essay.**
8. **Public-IP discovery must be simple**, because normal people do not know
   their public IP.

Those rules produced the architecture in §3: a creator-hosted relay, outbound
worker connections, certificate pinning, and single-use invitations.

### 1.3 What MeshCompute is *not*

Being explicit prevents a lot of confusion later:

- It is **not** a distributed model runtime. There is no layer splitting, no
  pipeline parallelism, no shared KV cache.
- It is **not** a remote desktop or remote shell. The host cannot type on a
  worker.
- It is **not** a VPN. It does not route arbitrary traffic.
- It does **not** download models behind your back, and it does **not** manage
  your router.
- It does **not** survive the worker process being killed. The worker is a
  foreground program.

---

## 2. Mental model: the whole system on one page

### 2.1 The cast

| Name | What it is | Where it lives |
| --- | --- | --- |
| **Group** | A private trust domain: one CA, one host, many workers | Created by `meshcompute host` |
| **Creator / host** | The laptop that accepts workers and shows the dashboard | Creator's machine |
| **Worker** | A laptop that offers its CPU/GPU to the group | Friend's machine |
| **Invitation** | A one-use, expiring secret that lets one worker enroll | Sent out of band |
| **Backend** | `llama-server` running a GGUF model on the worker | Worker, loopback `127.0.0.1:8080` |
| **Model registry** | A JSON file per model: alias, absolute path, size, SHA-256 | Worker state dir |
| **Relay / broker** | The host component that holds worker connections and dispatches requests | Host, TLS `:7443` |
| **Dashboard** | Token-protected HTML/JSON/SSE UI | Host, `127.0.0.1` |

### 2.2 The picture

```
                         INTERNET (IPv6, no NAT)
                                 │
        ┌────────────────────────┴─────────────────────────┐
        │                                                  │
┌───────▼──────────────────────────┐        ┌──────────────▼─────────────────┐
│ CREATOR'S LAPTOP (Linux)         │        │ FRIEND'S LAPTOP (Windows)      │
│                                  │        │                                │
│  meshcompute host                │        │  meshcompute start             │
│  ┌────────────────────────────┐  │        │  ┌──────────────────────────┐  │
│  │ local agent + broker       │  │        │  │ worker agent             │  │
│  │  • owns group CA           │  │◄───────┼──┤  • outbound TLS 1.3      │  │
│  │  • validates invitations   │  │  TLS   │  │  • client certificate    │  │
│  │  • leases worker channels  │  │  1.3   │  │  • reconnects after each │  │
│  │  • dispatches one request  │  │        │  │    dispatched request    │  │
│  └─────────────┬──────────────┘  │        │  └────────────┬─────────────┘  │
│                │                 │        │               │ loopback HTTP  │
│  dashboard 127.0.0.1:8083        │        │  llama-server 127.0.0.1:8080   │
│  CLI `run` / `status`            │        │  model: Qwen2.5-1.5B Q4_K_M    │
└──────────────────────────────────┘        │  GPU: RTX 3050 Laptop 4.3 GB   │
                                            └────────────────────────────────┘
```

### 2.3 A day in the life of one request

This is the part that explains every symptom you will ever see.

1. **You type** `meshcompute run qwen --node worker-abc`.
2. The CLI opens a mutual-TLS connection to the **local** agent on the host
   (it never talks to the friend directly).
3. It asks the local agent for `/v1/workers/worker-abc/v1/status`.
4. The local agent's **broker** needs a channel to that worker. Workers keep
   channels open *inbound to the host*; the broker leases one.
5. If no channel is idle, the broker answers **HTTP 503**. This is the exact
   error seen at 05:51 UTC. It means "the worker program is not running right
   now", not "the network is broken".
6. If a channel exists, the host writes **one HTTP request** down that TLS
   connection: `GET /v1/status`.
7. The worker agent replies with hardware telemetry and backend readiness. The
   connection is now **consumed**.
8. The worker immediately dials a **new** connection and waits. That is the
   `state=reconnecting` log line — it is replenishment, not failure.
9. For inference, steps 4–7 repeat with `POST /v1/run`. The worker checks the
   model registry, checks the backend, then calls
   `POST http://127.0.0.1:8080/completion`.
10. `llama-server` generates tokens on the GPU and returns them; the host
    relays the answer to your CLI or dashboard.

The key insight: **the worker is the client of the TLS connection, not the
server.** That is why no port forwarding is needed on the friend's side, and why
the friend's router never has to be touched.

---

## 3. Architecture in depth

### 3.1 Group identity

When you first run `meshcompute host`, it creates a brand-new private CA:

- **Ed25519** keys (fast, small, modern).
- One self-signed CA certificate.
- One coordinator certificate with CN `coordinator-<64 hex>`.
- The CA private key stays in the group state directory on your laptop.

Each worker that joins gets its own Ed25519 key and a certificate with CN
`worker-<64 hex>`. That CN is also the node ID you see in `status`. There is no
global namespace and no shared CA between groups — two groups are cryptographically
unrelated.

State files (per group, under the `--home` directory):

| File | Contents | Sensitivity |
| --- | --- | --- |
| `config.json` | listen/advertise endpoints, backend URL, peers, `group_host`, `public` | low |
| `identity.json` | CA cert+key, node cert+key, pin, unexpired tokens, revoked set | **high** |
| `models/<alias>.json` | absolute GGUF path, size, SHA-256 | low |

### 3.2 Invitations

An invitation is `base64url(JSON)`:

```json
{
  "Endpoint": "https://[2409:...:f4fc]:7443",
  "CA":       "-----BEGIN CERTIFICATE-----\n...",
  "Pin":      "<sha256 of the host certificate DER, hex>",
  "Token":    "<32 random bytes, hex>",
  "Public":   true,
  "Reverse":  true
}
```

Why each field exists:

- **Endpoint** — where to connect. It is also what the CA certificate is bound
  to, so a stolen invitation cannot be redirected to another host.
- **CA** — the worker trusts exactly this CA and nothing else (no system root
  store, no public CAs).
- **Pin** — the worker also pins the exact host certificate hash. Even a
  CA-signed impostor cannot impersonate the host.
- **Token** — the one-use enrollment secret. The host stores only its
  SHA-256, so a stolen state file cannot reveal unused tokens.
- **Public / Reverse** — allow public (non-private) IPs only when explicitly
  opted in, and mark this as the reverse-connection group design.

Life cycle:

1. `pair --ttl` creates a token and stores `sha256(token) -> expiry`.
2. The invitation is printed once. Nothing is transmitted automatically.
3. On `/enroll`, the host checks the token, signs the CSR, and **deletes the
   token**. Replays fail.
4. Tokens also expire. Maximum lifetime is 24 hours; default is 5 minutes.
5. If your friend fails, issue a new one. Never reuse.

### 3.3 Enrollment handshake

```
worker                                            host
  │  TLS 1.3 ClientHello                            │
  │────────────────────────────────────────────────►│
  │  ServerHello + cert (must match pin + CA)        │
  │◄────────────────────────────────────────────────│
  │  POST /enroll  {Token, CSR, Host}                │
  │────────────────────────────────────────────────►│
  │                          validate token          │
  │                          verify CSR              │
  │                          sign worker-<id> cert   │
  │                          delete token            │
  │  200 {ID, Cert}                                  │
  │◄────────────────────────────────────────────────│
  │  store identity.json, start agent                │
```

Properties:

- TLS 1.3 minimum, redirects forbidden, proxies disabled, 30 s timeout.
- The worker's identity file is created only after a successful enrollment.
- The host can `revoke <node-id>`; revoked node IDs are rejected afterwards.

### 3.4 The reverse tunnel

This is the heart of the design and the source of most confusion.

```
Host :7443 (TLS, client cert required)
  ▲
  │   worker dials outbound, presents its client certificate
  │
Worker ── "POST / Connection: Upgrade, Upgrade: meshcompute/1" ──►
Host   ── "101 Switching Protocols" ─────────────────────────────►
Host   ── writes ONE HTTP request (status/run/stream) ────────────►
Worker ── writes the HTTP response ───────────────────────────────►
       connection closed; worker dials a fresh one
```

Why one request per connection?

- It makes accounting trivial: one in-flight request per channel.
- It means a crashed or hung request cannot poison a shared stream.
- It lets the host bound concurrency by simply limiting idle channels.
- It costs a TLS handshake per request, which is acceptable at dashboard and
  inference cadence (dashboard polls are seconds apart).

The broker:

- Holds idle channels in a map keyed by node ID.
- **Leases** exactly one channel per dispatch; a leased channel is not reused.
- Returns **HTTP 503** when a worker has no idle channel. `status` reports the
  worker as unhealthy with that error.
- A worker with zero channels is indistinguishable from a worker that is off —
  and that is exactly the right user-facing meaning.

Allowed worker operations (nothing else exists):

| Path | Method | Purpose |
| --- | --- | --- |
| `/v1/status` | GET | hardware, GPU, backend readiness, busy flag |
| `/v1/ping` | GET | liveness |
| `/v1/probe` | POST | network RTT/throughput measurement |
| `/v1/run` | POST | one non-streaming completion |
| `/v1/run/stream` | POST | SSE token stream |

Limits: 1 MiB prompt, 8 MiB response, 32768 max output tokens, 30 minute stream
cap, 15 s per-write deadline, 10 s dial, 5 s dispatch wait.

### 3.5 Worker agent

On start (`join` or `start`) the worker agent:

1. Loads `identity.json` (certificate, key, host endpoint).
2. Discovers resources: OS, arch, CPU model, logical CPUs, RAM, disk, GPUs.
3. Enters `connectLoop`: dial → serve one request → redial.
4. Registers nothing centrally; the host identifies it by certificate CN.

Resource discovery on Windows uses CIM/PowerShell (`Win32_OperatingSystem`,
`Win32_Processor`, `PSDrive`) and `nvidia-smi` for GPUs. If WMI is blocked, the
fields are reported as unknown with an issue note — the tool never invents
numbers.

Backend readiness is simply: can the agent reach `http://127.0.0.1:8080`,
does `/health` return `{"status":"ok"}`, does `/v1/models` return exactly one
model with the requested alias, and what is `default_generation_settings.n_ctx`
from `/props`.

### 3.6 Backend and model registry

`model add NAME PATH`:

- Requires an absolute path.
- Hashes the entire file with SHA-256.
- Stores size and hash in `<home>/models/NAME.json`.
- Refuses to overwrite (the file is created exclusively). Delete the JSON to
  re-register.
- Never downloads anything.

`backend serve NAME`:

1. Loads and **re-verifies** the model file (hash + size).
2. Runs memory admission (see below).
3. Locates `llama-server` (`--binary` or `PATH`).
4. Strips `LLAMA_*` and `GGML_RPC*` environment variables, so an ambient config
   cannot enable RPC or downloads.
5. Launches:

```
llama-server --model <path> --alias <name> --host 127.0.0.1 --port 8080
             --ctx-size 2048 --parallel 1 --n-gpu-layers 99 --no-context-shift
```

Memory admission math for Qwen2.5-1.5B-Instruct Q4_K_M:

```
weights           ≈ 986 MB
KV per token      = layers × kv_heads × head_dim × 2 (K,V) × 2 bytes
                  = 28 × 2 × 128 × 2 × 2 = 28,672 bytes/token
KV for 2048 ctx   ≈ 58.7 MB
overhead          = 512 MB (runtime + temporaries)
------------------------------------------------
total             ≈ 1.56 GB  → fits a 4.3 GB RTX 3050 Laptop
```

`--gpu-layers 99` makes the admission require a GPU candidate; if it does not
fit, the tool refuses to start rather than silently falling back to CPU.

### 3.7 Dashboard

- Binds to `127.0.0.1` by default and must stay loopback for
  security.
- Prints a URL with `?token=<random>`; the token is exchanged for an HttpOnly
  cookie (per `docs/windows.md`) so it does not stay in browser history.
- Shows per-node GPU telemetry, aggregated totals (display only), and can
  stream tokens from **one** selected node.
- No endpoint accepts remote administration commands.

### 3.8 Security posture

| Property | Implementation |
| --- | --- |
| Transport | TLS 1.3 only |
| Server authentication | CA from invitation + exact certificate pin |
| Client authentication | Worker client certificate signed by group CA |
| Enrollment | Single-use token, ≤24 h, CSR-based |
| Redirects/proxies | Forbidden/disabled during enrollment |
| Worker API | Five fixed operations, no shell/files |
| Dashboard | Loopback + bearer token |
| Revocation | Node ID denylist on the host |
| Model integrity | SHA-256 verified at every backend launch |
| Ambient config | `LLAMA_*`/`GGML_RPC*` stripped |

Known limits: the invitation is a bearer secret; anyone who gets it before the
friend can enroll first. The dashboard token is in the URL (though exchanged for
a cookie). There is no rate limiting, audit shipping, or certificate rotation.

---

## 4. History: how we got here

### 4.1 Phase 0 — the idea and the constraints (2026-11-06)

The starting question was not "how do we write a distributed runtime" but "how
do two normal laptops, on two different networks, run a model on one of them
without asking either owner to become a network administrator". That framing
produced the constraints in §1.2 and immediately ruled out the easy answer
(Tailscale) and the easy fake answer (claiming a pooled GPU).

### 4.2 Phase 1 — first working implementation

Commit `46bc102 Initial implementation of MeshCompute` established:

- Group identity and certificate enrollment.
- HTTPS agent with hardware discovery.
- Model registry and llama.cpp adapter.
- CLI and a token-protected loopback dashboard.
- Memory and network admission.
- Linux and Windows installers.

At this point the system worked on **one** machine, and could dispatch to a
second machine if it was directly reachable.

### 4.3 Phase 2 — making the tunnel safe

Commits `b14b52f` → `bdc6d7f` → `a9f4cc7` → `7294a0e` → `9623659` built the
reverse connection in layers:

- bounded framing for reverse connections,
- a bounded worker connection broker,
- cluster-TLS authentication of broker sessions,
- session teardown on revocation,
- concurrency and fuzz tests.

This is where "one request per connection" and the `meshcompute/1` upgrade
protocol were finalized.

### 4.4 Phase 3 — packaging and release

- `43a7083`, `a86902a`, `a1d88c8`, `4585779`, `3fc372b` made Windows usable
  without Go: bundled binary, `install.cmd`, `join.cmd`, checksum verification,
  CIM discovery fixes.
- `06c2819` added the dashboard with SSE streaming.
- CI ran on Ubuntu and Windows runners; cross-builds for linux/amd64,
  linux/arm64, windows/amd64.
- Release `v0.2.0` published:

```
meshcompute-windows-amd64.exe
meshcompute-windows-amd64.zip
meshcompute-linux-amd64.tar.gz
meshcompute-linux-arm64.tar.gz
SHA256SUMS
```

### 4.5 Phase 4 — creator-hosted groups

Commit `2f4053c` turned the tool into the product you asked for:

- `meshcompute host` creates/resumes a group on the creator's laptop.
- The host prints a single-use invitation and a dashboard URL.
- Workers connect **outbound**, so friends never open a port.
- Group state is isolated per `--home`.

### 4.6 Phase 5 — automatic addresses and honest errors

Commit `fdcc68e` added:

- `host --auto`: prefer a global IPv6 address, otherwise a private IPv4.
- Wildcard binding when an address is auto-selected.
- `joinFailure`: distinguish "host unreachable" from "invitation expired or
  already used".
- Reachability guidance printed by `host`.

This was directly motivated by the live test: ordinary users cannot be expected
to know their public address or to interpret `security operation failed`.

### 4.7 Phase 6 — the live test (2026-11-07)

The test ran in this order. Each attempt is documented in full in §5.

| Time (UTC) | Attempt | Outcome |
| --- | --- | --- |
| ~09:20 | Cable link, static `10.10.10.x` | link up 1 Gbps, zero frames |
| ~10:43 | Cable with DHCP (`ipv4.method shared`) | zero DHCP requests |
| ~10:52 | Same-Wi-Fi host on `10.161.226.188` | local 200, friend blocked |
| ~10:57 | Public IPv4 check `157.50.2.104` | closed (CGNAT) |
| ~10:58 | IPv6 host `--auto` | external scan TCPOPEN |
| 05:30:38 | Friend joins over IPv6 | **success** |
| 05:33, 05:40 | Host status | worker healthy, GPU telemetry visible |
| 05:51 | Worker entry | `agent returned HTTP 503` (process stopped) |
| pending | GPU backend setup | not yet run |

Note: the first eight rows use local wall-clock times from the session
(UTC+5:30); the join and later rows are UTC from the host log.

### 4.8 Phase 7 — worker GPU provisioning (in progress)

Added `scripts/windows-gpu-setup.ps1` so the friend runs one line that downloads
llama.cpp CUDA 12.4 and Qwen2.5-1.5B, registers the model, and starts the
backend. Verified the upstream artefacts (275 MB, 374 MB, 986 MB; all HTTP
200). Not yet executed on the friend's laptop.

## 5. The connection attempts, one at a time

This section is the forensic record. For each attempt: what we wanted, what we
ran, what we measured, what it means, and what to do instead.

### 5.1 Attempt A — direct Ethernet cable between the two laptops

**Goal:** prove inference with the friend's laptop physically beside yours,
using a Cat6 cable and no router, no internet, no NAT.

**Setup on the Linux side.** A dedicated NetworkManager profile was attached to
`enp3s0` so Wi-Fi stayed untouched:

```sh
nmcli con add type ethernet ifname enp3s0 con-name meshcompute-direct \
  ipv4.method manual ipv4.addresses 10.10.10.1/24 ipv6.method link-local
nmcli con up meshcompute-direct
```

(During the session the profile already existed, so the equivalent
`nmcli con mod meshcompute-direct ...` form was used.)

**Setup on the Windows side (instructed).**

```powershell
Get-NetAdapter | Format-Table Name,Status,LinkSpeed,MediaType -AutoSize
netsh interface ipv4 set address name="Ethernet" static 10.10.10.2 255.255.255.0
Get-NetIPAddress -InterfaceAlias "Ethernet" -AddressFamily IPv4 | Format-Table
ping 10.10.10.1
Test-NetConnection 10.10.10.1 -Port 7443
arp -a | findstr 10.10.10
```

**What the Linux side measured.**

```
enp3s0   UP   10.10.10.1/24
Speed: 1000Mb/s   Duplex: Full   Link detected: yes

ping -c 3 -W 2 10.10.10.2
  → Destination Host Unreachable

ip neigh show dev enp3s0
  → 10.10.10.2 FAILED

ip -6 neigh show dev enp3s0
  → (empty)
```

The link layer looked perfect. So we tried DHCP, in case the Windows laptop was
still on "Automatic":

```sh
nmcli con mod meshcompute-direct ipv4.method shared ipv4.addresses 10.10.10.1/24
nmcli con up meshcompute-direct
```

NetworkManager then ran dnsmasq:

```
dnsmasq --dhcp-range=10.10.10.10,10.10.10.254,3600
        --listen-address=10.10.10.1
        --leasefile=/var/lib/NetworkManager/dnsmasq-enp3s0.leases
```

**The decisive measurement** was the frame counter, not ping:

```sh
a=$(cat /sys/class/net/enp3s0/statistics/rx_packets)
b=$(cat /sys/class/net/enp3s0/statistics/rx_bytes)
sleep 10
a2=$(cat /sys/class/net/enp3s0/statistics/rx_packets)
b2=$(cat /sys/class/net/enp3s0/statistics/rx_bytes)
echo "rx frames in 10s: $((a2-a))  rx bytes: $((b2-b))"
```

Result:

```
rx frames in 10s: 0  rx bytes: 0
errors: 0
```

Also:

```sh
cat /var/lib/NetworkManager/dnsmasq-enp3s0.leases
# (no lease ever)
```

**Interpretation.** A 1 Gbps link only proves that *something powered* is on the
other end. A docking station, a switch, a wall port, or a sleeping laptop with
Wake-on-LAN all keep the PHY link up. A running Windows machine on that segment
would have sent at least one of:

- a DHCP DISCOVER within seconds of the link coming up,
- ARP for its gateway or for `10.10.10.1`,
- IPv6 router solicitation / neighbour discovery.

It sent **zero frames in a minute**. Therefore the far end was not an awake
Windows laptop, and no amount of software configuration could fix it.

**Why this failed (root causes, in order of likelihood):**

1. The far end was not the laptop (dock, switch, wall socket).
2. The laptop was asleep or the lid closed; the adapter stayed powered.
3. The laptop had no RJ45 port and a USB-C dock was used, but the dock was not
   attached to the laptop.

**What would have proven it:** the frame counter, or simply checking on the
Windows side that the wired adapter says `Up` and gets an address. Both require
physical access to that laptop, which was the recurring blocker.

**Lesson:** in any future build, a `meshcompute link check` command should
report link speed, carrier, rx/tx frames, DHCP leases, and neighbour state in
one screen before any invitation is generated.

### 5.2 Attempt B — same Wi-Fi network

**Goal:** avoid the cable; use the hostel/campus Wi-Fi both laptops were on.

**Setup.** A second group host bound to the Wi-Fi address, plus a temporary
HTTP server to distribute join scripts:

```sh
./bin/meshcompute --home "$HOME/.meshcompute-wifi" host \
  --listen 10.161.226.188:7443 \
  --advertise https://10.161.226.188:7443 \
  --ui 127.0.0.1:8082 --ttl 2h

cd /tmp/opencode/share
python3 -m http.server 8080 --bind 0.0.0.0
```

Files served: `index.html`, `JOIN-MESHCOMPUTE.cmd` (cable), and
`JOIN-MESHCOMPUTE-WIFI.cmd` (Wi-Fi, with the invitation embedded).

**Verification from the host itself:**

```sh
curl -s -o /dev/null -w "%{http_code}\n" http://10.161.226.188:8080/
# 200
curl -s -o /dev/null -w "%{http_code}\n" http://10.10.10.1:8080/
# 200
```

Both worked locally, which only proves the server and address are valid.

**What the friend saw:** the page did not load from their laptop.

**Why this likely failed.** Campus and hostel SSIDs (`BIT-*` in the scan) very
often enable **AP/client isolation**: each client can reach the gateway and the
internet, but not another client on the same SSID. That is a deliberate
security feature of the access point and cannot be disabled from a laptop. It
would also explain why the IPv6 path worked while the LAN path did not: IPv6
traffic went through the router to the internet and back, not directly
client-to-client.

**What would have fixed it:** a phone hotspot (hotspots normally allow
client-to-client traffic), a working cable, or the internet path. We chose the
internet path.

### 5.3 Attempt C — public IPv4 with router forwarding

**Goal:** the classic approach — the host exposes one port on its router, the
friend connects to the public IPv4.

**Addresses observed:**

```
LAN IPv4:      10.161.226.188/24
Default gateway: 10.161.226.96
Public IPv4:   157.50.2.104
```

The gateway being a private address (and the public address not appearing on
any local interface) means the network is behind NAT — most likely a shared
campus NAT, possibly CGNAT. Port forwarding requires administrative access to
that router, which we do not have.

**External verification.** Using an external port-check API from outside the
network:

```sh
curl -s -X POST https://portchecker.io/api/v1/query \
  -H 'Content-Type: application/json' \
  -d '{"host":"157.50.2.104","ports":[7443]}'
```

Result:

```json
{"error":false,"check":[{"port":7443,"status":false}],"host":"157.50.2.104"}
{"error":false,"check":[{"port":8080,"status":false}],"host":"157.50.2.104"}
```

Both ports closed. No invitation would ever work over this path until the
router forwards TCP 7443 to `10.161.226.188`.

**Important clarification that caused confusion in the chat:** the *friend's*
router never needs a forward. Only the host's side does, and only for IPv4. In
the reverse-connection design, workers dial out.

### 5.4 Attempt D — IPv6 (the one that worked)

**Why IPv6 is the right default here:** there is no NAT. If both ends have a
global IPv6 address, the worker can dial the host directly. No port forwarding,
no router login, no third-party service. The only requirements are that the
host's firewall allows inbound TCP 7443 and the friend's network provides IPv6.

**Host command:**

```sh
./bin/meshcompute --home "$HOME/.meshcompute-auto" host \
  --auto --ui 127.0.0.1:8083 --ttl 2h
```

`--auto` printed:

```
Selected [2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc]:7443
  (global IPv6; friends connect directly without router port forwarding).
Group host: https://[2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc]:7443
```

Listening socket confirmed:

```sh
ss -ltn | grep 7443
LISTEN 0 4096 [2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc]:7443 [::]:*
```

**External reachability test.** The host's own IPv6 address was scanned from
the internet using a service that scans the caller's source address:

```
https://www66.chappell-family.co.uk/cgi-bin6/ipscanfasttxt.cgi
  ?includeexisting=-1&customport0=7443&termsaccepted=1

Port 7443 = TCPOPEN
```

Then, as a control, the same scan for port 8084 (where a helper HTTP server was
tried) returned `TCPSTEALTH` — the host firewall allows 7443 but not arbitrary
ports. This explained why serving the setup script from the host did not work
and why GitHub raw was used instead.

**The join.** A fresh invitation was generated and the friend pasted it into
`join.cmd` on Windows. The first attempt printed:

```
meshcompute: join failed: verify invitation expiry and host reachability:
security operation failed
```

That is the generic failure for an invalid, expired, or already-consumed
invitation. A second, fresh invitation succeeded:

```
Joined the group. Worker connections are outbound and reconnect automatically.
2026/11/07 05:30:38 event=agent_start transport=mutual_tls
```

**Host verification at 05:33 and 05:40 UTC:**

```
worker-d73500f00498e65a5c6b777d47b69776a4f524ce0a66d562241633978218c559
  healthy: true
  Windows amd64 | AMD Ryzen 5 7535HS with Radeon Graphics | 12 logical CPUs
  16.3 GB RAM (6.3 GB available) | 323 GB free disk
  GPU: NVIDIA GeForce RTX 3050 Laptop GPU, 4.3 GB total, 4.2 GB free
  driver 566.14 | backend_ready: false
```

A live TLS session was visible:

```
ss -tn state established '( sport = :7443 )'
[2409:...:f4fc]:7443  ←  [2409:40f4:210e:4b05:977:b4ae:1fe5:57b1]:65518
```

**This is the proof that the core product works across the internet**: two
unrelated networks, mutual TLS, no VPN, no port forward, no central server.

### 5.5 Why the worker later showed HTTP 503

At 05:51 UTC the same node reported:

```json
{"name":"worker-d73500f00498e65...","healthy":false,"error":"agent returned HTTP 503"}
```

`503` is produced by the host broker when it has **no idle channel** for that
node. The causes, in order:

1. The `join`/`start` window on Windows was closed. The worker is a foreground
   process; closing it disconnects.
2. The laptop slept or lost its network, so the process is alive but cannot
   reach the host.
3. The worker crashed.

None of these is a firewall, IPv4, or invitation problem. The identity is still
stored, so the fix is:

```powershell
& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" start
```

If that command says the identity is missing, the enrollment was never
completed and a new invitation is required — but that was not the case here.

### 5.6 Protocol scoreboard

| Path | Protocols involved | Connected? | Why |
| --- | --- | --- | --- |
| IPv6 internet | TCP, TLS 1.3, HTTP/1.1 Upgrade | **YES** | no NAT, firewall allows 7443 |
| IPv4 internet | TCP | no | CGNAT, no forward |
| Wi-Fi LAN | TCP, HTTP | no (friend) | AP/client isolation |
| Ethernet cable | Ethernet L2, IPv4, DHCP, IPv6 NDP | no | no live peer on the wire |
| Tailscale/VPN | WireGuard etc. | not used | forbidden by requirements |

---

## 6. Failure catalogue (symptom → cause → evidence → fix)

### F1. Cable link is up but nothing answers

- **Symptom:** `ping` gives "Destination Host Unreachable"; ARP entry `FAILED`.
- **Evidence:** `rx_packets` delta 0 over 10 s; no DHCP lease; no IPv6
  neighbour.
- **Root cause:** no live peer on the segment (dock/switch/wall port/sleeping
  laptop).
- **Fix:** plug into the laptop's own RJ45 or USB Ethernet adapter; verify the
  adapter is `Up` on Windows; or use the internet path.
- **Status:** unresolved physically, bypassed with IPv6.

### F2. DHCP server receives no request

- **Symptom:** dnsmasq running, lease file empty.
- **Root cause:** same as F1 — no client.
- **Fix:** none in software; this is a cabling/wake problem.
- **Status:** closed as "not a bug".

### F3. Friend cannot open the host's LAN URL

- **Symptom:** local `curl` returns 200, friend's browser times out.
- **Root cause:** Wi-Fi client isolation, or the two laptops are on different
  SSIDs.
- **Fix:** phone hotspot, cable, or internet path.
- **Status:** bypassed with IPv6.

### F4. Public IPv4 port is closed

- **Symptom:** external check `status:false` for 7443.
- **Root cause:** NAT/CGNAT without a port forward.
- **Fix:** configure a forward on the host's router, or use IPv6.
- **Status:** not attempted (no router admin); IPv6 used instead.

### F5. `join failed: security operation failed`

- **Symptom:** generic error on the worker.
- **Root cause:** invitation expired, already used, malformed, or truncated
  during copy/paste.
- **Fix:** `meshcompute pair --ttl 30m` on the host; paste the complete code.
- **Status:** resolved (second invitation worked).

### F6. `agent returned HTTP 503`

- **Symptom:** host `status` marks a previously healthy worker unhealthy.
- **Root cause:** worker process not running / no idle channel.
- **Fix:** run `meshcompute start` on the worker; keep the window open; disable
  sleep while working.
- **Status:** open (requires action on the friend's laptop).

### F7. `state=reconnecting` every 30 seconds

- **Symptom:** scary-looking repeated log line.
- **Root cause:** normal replenishment after each consumed connection; the line
  is emitted because the closed connection returns a non-nil error.
- **Fix:** none; verify health with `status`, not with log lines.
- **Status:** explained.

### F8. `backend_ready: false`

- **Symptom:** worker healthy but cannot run inference.
- **Root cause:** no `llama-server`, no registered model.
- **Fix:** run the GPU setup script, or `model add` + `backend serve`.
- **Status:** pending.

### F9. Cannot take control of the friend's laptop

- **Symptom:** request to "run commands on my friend's laptop automatically".
- **Root cause:** the assistant executes only on the local Linux machine; there
  is no remote shell channel into Windows, and none is exposed by the product.
- **Fix:** reduce the worker side to a single paste; that is a product goal, not
  a remote-control feature.
- **Status:** accepted as a design boundary.

### F10. Helper script not downloadable from the host

- **Symptom:** `http://[host]:8084/...` unreachable from outside.
- **Root cause:** host firewall permits 7443 only (confirmed by `TCPSTEALTH`).
- **Fix:** serve the script from GitHub raw (HTTPS 443), which always works.
- **Status:** resolved.

### F11. Three hosts, three dashboards, three tokens

- **Symptom:** confusion about which group was live and which token was valid.
- **Root cause:** experimental hosts on `:8081`, `:8082`, `:8083` left running.
- **Fix:** one `--home` and one dashboard per real group; stop the rest.
- **Status:** needs cleanup.

### F12. `--home` on an exFAT SSD

- **Symptom:** permission errors; state directory rejected.
- **Root cause:** exFAT does not provide Unix mode bits; the tool requires 0700
  semantics.
- **Fix:** keep group state on ext4/NTFS (`$HOME`), not on a removable exFAT
  volume.
- **Status:** resolved by moving to `$HOME`.

### F13. Confusing "connected" with "GPU ready"

- **Symptom:** expectation that seeing the worker means inference works.
- **Root cause:** three independent layers (enrollment, transport, backend).
- **Fix:** check `healthy` and `backend_ready` separately.
- **Status:** explained.

---

## 7. If it failed, what next: architecture v2

The current design is sound but has sharp edges that the live test exposed. The
following is a concrete, buildable plan — ordered by user pain, not by
elegance. Each item maps to a failure above.

### 7.1 Worker as a managed service (fixes F6)

**Problem:** closing a terminal stops the worker; a sleeping laptop silently
disappears.

**Design:**

- `meshcompute service install` on Windows creates a **Scheduled Task** (not a
  Windows service, to avoid admin rights) that runs `meshcompute start` at
  logon, restarts on failure, and starts on resume.
- `meshcompute service status` shows whether it is running.
- The task uses the existing identity, so no invitation is needed.
- A small heartbeat file lets `doctor` distinguish "process dead" from
  "network down".

**Why not a Windows service:** services run as SYSTEM and complicate GPU access
and user-scoped state. A user-level task is sufficient and needs no elevation.

### 7.2 A reachability assistant (fixes F3, F4, F5)

**Problem:** the user cannot tell whether IPv6, IPv4 forwarding, or LAN will
work, and the error messages arrive only after a failed join.

**Design: `meshcompute reach`**

1. Enumerate candidate addresses: global IPv6, private IPv4, public IPv4 (via an
   external echo), LAN IPv4.
2. Test local bindability and, where possible, external reachability.
3. Try **UPnP-IGD / NAT-PMP** to request a TCP 7443 mapping automatically
   (no third party, no router login). Fall back to a printable instruction
   block if the router refuses.
4. Print a matrix:

```
IPv6 global     2409:...:f4fc   available   external: OPEN     ← use this
IPv4 LAN        10.161.226.188  available   external: n/a
IPv4 public     157.50.2.104    CGNAT       external: CLOSED   ← needs forward
```

5. `host --auto` consumes the same logic.

**Why this matters:** it turns a 30-minute debugging session into one command.

### 7.3 Onboarding without long pastes (fixes F5)

**Problem:** a 1.4 KB base64 blob is easy to truncate and impossible to read.

**Design:**

- `pair` also emits a **short code** (e.g. 9 characters) plus a **bootstrap
  URL** on the host: `https://[host]:7443/j/<short>`.
- `join.cmd` accepts either form; the short code is exchanged for the full
  invitation over TLS, pinned to the host.
- Optionally render a QR code in the terminal for phone-mediated transfer.
- The underlying token stays single-use and expiring; only the transport of the
  invitation improves.

**Security note:** a short code must be rate-limited and expire in minutes,
because its entropy is lower. It is a pointer, not the secret itself.

### 7.4 Backend provisioning as a first-class flow (fixes F8)

**Problem:** the worker is connected but useless until someone installs 1.6 GB
of software by hand.

**Design:**

- `meshcompute prepare --gpu` on the worker runs a **signed manifest**:
  llama.cpp build URL + SHA-256, CUDA runtime URL + SHA-256, model URL +
  SHA-256, expected VRAM.
- Downloads are resumable, verified, and written only under the user's state
  directory.
- The host prints the exact one-liner when a worker is healthy but
  `backend_ready` is false.
- No silent downloads: `prepare` is explicit, and `model add` remains
  offline-only.

This is essentially what `scripts/windows-gpu-setup.ps1` already does; v2
should move the logic into the binary so it is versioned with the protocol.

### 7.5 Address changes and re-enrollment (currently a hard edge)

**Problem:** the certificate identity is bound to the listen/advertise address.
If the host's IPv6 changes (common on residential/campus networks), the old
group cannot be reused; everyone must re-enroll.

**Design options:**

1. **Signed address update:** the host signs a new endpoint with the same CA
   key; workers accept it if the certificate chain and pin still match.
2. **Stable name:** integrate optional dynamic DNS, but keep the CA as the
   authority; names are hints, pins are truth.
3. **Re-enrollment token:** a short-lived token that lets an existing worker
   re-pin a new endpoint without a new identity.

Option 1 is the smallest change and preserves the trust model.

### 7.6 Diagnostics that match reality (fixes F1, F2, F6, F10)

**Design: `meshcompute doctor --net`**

- Interface table with carrier, speed, addresses.
- Frame counters sampled over 5 seconds.
- DHCP lease presence for direct-link profiles.
- Neighbour table freshness.
- Listener inventory (`7443`, dashboard port).
- External reachability result for the advertised address.
- Last time each worker was seen, and whether its process is reachable.
- Firewall rule check for 7443 (read-only where possible).

Output should be one screen, redacted, and shareable — that is the artifact a
non-expert can send to whoever is helping.

### 7.7 One group at a time (fixes F11)

- A lock file in the state directory prevents two hosts from using the same
  `--home`.
- `meshcompute groups` lists known state directories and their endpoints.
- Refuse to start if another host is already bound to the same advertised
  address/port, with a clear message.

### 7.8 Scheduling and admission (small but valuable)

- `run --node auto` should rank workers by: backend ready → model match →
  context fit → measured RTT → free VRAM.
- Keep the hard rule: one worker per request; no sharding.
- Cache the last successful RTT for 10 minutes to avoid probing on every call.

### 7.9 Optional, opt-in relay fallback (only if constraints relax)

The requirement says "no shared central relay". If that ever changes, the
correct fallback is a **user-hosted relay**: a third friend with a public IPv6
address can run `meshcompute relay` and forward encrypted channels. The relay
must never see plaintext or hold keys — it forwards bytes between pinned
endpoints. Treat this as a last resort, clearly labelled, off by default.

### 7.10 What must *not* change

- No pooled VRAM claims; one request, one node.
- No remote shell or file push into workers.
- No automatic model downloads without explicit consent.
- No reliance on third-party identity or VPN providers.
- Dashboard stays loopback-only.

### 7.11 Suggested order of work

| Priority | Item | Reason |
| --- | --- | --- |
| 1 | 7.4 backend provisioning | without it the worker cannot do the thing it joined for |
| 2 | 7.1 service mode | removes the most common "it disconnected" cause |
| 3 | 7.2 reachability assistant | prevents the cable/Wi-Fi/IPv4 dead ends |
| 4 | 7.6 `doctor --net` | turns support into a single output |
| 5 | 7.3 short-code onboarding | removes paste errors |
| 6 | 7.5 address update | long-term usability |
| 7 | 7.7, 7.8 | hygiene and quality |

## 8. Rebuild from scratch — the reliable path

This is the procedure to follow when starting over, with the lessons above
baked in. Every step states what you should see before moving on.

### 8.0 Choose the path before touching anything

```
Do both laptops have a global IPv6 address?
├── YES → use IPv6 (easiest, no router changes)                  §8.2
└── NO
    ├── Can you log into the host's router and forward TCP 7443?
    │   ├── YES → IPv4 + port forward                            §8.3
    │   └── NO  → put both laptops on a phone hotspot            §8.4
    └── Physically together? → cable, but verify frames first    §8.5
```

Test IPv6 on both machines:

```sh
# Linux host
ip -6 -br addr show scope global
curl -6 -s https://ifconfig.co
```

```powershell
# Windows worker
curl.exe -6 -s https://ifconfig.co
```

If either prints no global IPv6 address, that machine cannot use the IPv6 path
*right now* (it may still work after reconnecting to Wi-Fi or enabling IPv6 in
the router).

### 8.1 Phase 1 — build or download

**Linux (source):**

```sh
cd gpu_toolkit
GOPROXY=off GOTOOLCHAIN=local go build -o bin/meshcompute ./cmd/meshcompute
./bin/meshcompute version
```

Expected: version string, no network access needed.

**Windows (prebuilt):**

1. Download `meshcompute-windows-amd64.zip` from Releases.
2. Extract it anywhere (e.g. `Downloads\meshcompute-windows-amd64`).
3. Open PowerShell in that folder.

Do **not** use the GitHub source ZIP unless you want the bootstrap that
downloads the binary.

### 8.2 Phase 2 — start the group host (IPv6 path)

```sh
mkdir -p "$HOME/mesh-group"
./bin/meshcompute --home "$HOME/mesh-group" host --auto \
  --ui 127.0.0.1:8081 --ttl 30m
```

What each part does:

- `--home "$HOME/mesh-group"` — a fresh, isolated trust domain. Never point two
  groups at the same directory.
- `host` — create (first run) or resume (later runs) the group.
- `--auto` — pick a global IPv6 address if present, otherwise a private IPv4;
  bind the wildcard address.
- `--ui 127.0.0.1:8081` — dashboard on loopback only.
- `--ttl 30m` — invitations live 30 minutes (default 5, max 24 h).

Expected output:

```
Selected [2409:...:f4fc]:7443 (global IPv6; friends connect directly ...)
Group host: https://[2409:...:f4fc]:7443
Private single-use invitation (expires in 30m0s):
eyJ...
Dashboard: http://127.0.0.1:8081/?token=...
Keep this process running. Ctrl-C stops this group host.
```

Verify the listener:

```sh
ss -ltn | grep 7443
```

### 8.3 Phase 2b — IPv4 path (only if IPv6 is unavailable)

```sh
./bin/meshcompute --home "$HOME/mesh-group" host \
  --listen 0.0.0.0:7443 \
  --advertise https://203.0.113.10:7443 \
  --public \
  --ui 127.0.0.1:8081
```

- `--listen 0.0.0.0:7443` — accept on all interfaces.
- `--advertise` — the address friends will dial; must be the **public** IPv4.
- `--public` — explicitly opt in to public hosting (validation requires it).

On the router: forward **TCP 7443** to the host's LAN IP (here
`10.161.226.188`). Then verify from outside:

```sh
curl -k --max-time 5 https://203.0.113.10:7443/
```

A TLS error is fine — a timeout or connection refused is not.

### 8.4 Phase 2c — hotspot path (no IPv6, no router access)

1. Turn on the phone hotspot.
2. Connect both laptops to it.
3. Run `host --auto` again and let it select the hotspot's private IPv4.
4. The friend joins with the printed `https://<private-ip>:7443` invitation.
5. Keep the hotspot on while the worker is connected.

Hotspots usually allow client-to-client traffic, unlike campus Wi-Fi.

### 8.5 Phase 2d — direct cable (only when physically together)

Follow §5.1, but **before generating any invitation**, require:

```sh
ethtool enp3s0 | grep -E 'Speed|Duplex|Link detected'   # link is up
ip neigh show dev enp3s0                                 # peer is REACHABLE
```

and the zero-frame test:

```sh
a=$(cat /sys/class/net/enp3s0/statistics/rx_packets); sleep 5
b=$(cat /sys/class/net/enp3s0/statistics/rx_packets); echo $((b-a))
```

If the delta is 0, stop: the peer is not there. Do not debug TLS.

### 8.6 Phase 3 — issue an invitation

```sh
./bin/meshcompute --home "$HOME/mesh-group" pair --ttl 30m
```

Rules:

- One token, one worker, one attempt.
- Generate it immediately before the friend joins.
- If anything fails, generate a new one; do not retry the old one.
- Send it privately. It is a bearer secret.

### 8.7 Phase 4 — worker joins

**Windows, from the extracted folder:**

```powershell
.\join.cmd
```

Paste the invitation, press Enter. Expected:

```
Joined the group. Worker connections are outbound and reconnect automatically.
2026/11/07 ... event=agent_start transport=mutual_tls
```

Keep the window open. If it later closes, resume without a new invitation:

```powershell
& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" start
```

**Verify on the host:**

```sh
./bin/meshcompute --home "$HOME/mesh-group" status
```

Expected: a `worker-<64 hex>` entry with `healthy: true` and a `collected_at`
timestamp seconds old.

### 8.8 Phase 5 — install the GPU backend on the worker

One line, in a **second** PowerShell window:

```powershell
irm https://raw.githubusercontent.com/Aruneshw/gpu_toolkit/main/scripts/windows-gpu-setup.ps1 | iex
```

It performs, in order:

1. Creates `%LOCALAPPDATA%\meshcompute\llama` and `...\models`.
2. Downloads `llama-b12182-bin-win-cuda-12.4-x64.zip` (275 MB).
3. Downloads `cudart-llama-bin-win-cuda-12.4-x64.zip` (374 MB).
4. Extracts both flat into `...\llama` (so `llama-server.exe` and the CUDA DLLs
   sit together).
5. Downloads `Qwen2.5-1.5B-Instruct-Q4_K_M.gguf` (986 MB).
6. Removes any stale `models\qwen.json` and runs
   `meshcompute model add qwen <path>` (hashes the file).
7. Runs `backend serve` with `--context 2048 --gpu-layers 99
   --kv-bytes-per-token 28672`.

Expected ending:

```
load_backend: loaded CUDA backend
llama_model_load: model size = 986 MB
server is listening on 127.0.0.1:8080
```

Manual equivalent is in §9.5.

### 8.9 Phase 6 — run inference

```sh
./bin/meshcompute --home "$HOME/mesh-group" run qwen \
  --node worker-<id> --prompt "Explain what a KV cache is."
```

Or from the dashboard printed by `host`.

Expected: JSON with `content`, `tokens_per_second`, `node_id`, `mode:
single_remote`, plus a network admission note.

If you see `selected agent/model is not ready`, check in this order:

1. `status` — is the worker healthy?
2. `backend_ready` — is the backend running?
3. Model alias — does `/v1/models` return exactly `qwen`?
4. Context — is the request context ≤ 2048?

### 8.10 Phase 7 — verification and shutdown

```sh
./bin/meshcompute --home "$HOME/mesh-group" status
./bin/meshcompute --home "$HOME/mesh-group" doctor
./bin/meshcompute --home "$HOME/mesh-group" network probe worker-<id>
```

Shutdown:

```sh
# host: Ctrl-C in its terminal, or
pkill -f 'meshcompute --home /home/USER/mesh-group'

# worker (Windows): Ctrl-C in the join/start window
```

---

## 9. Complete command reference

Every command below is grouped by purpose. Entries marked **[given]** were sent
to the Windows worker; **[recommended]** were not run but are the correct next
commands. Everything else was run during this project.

### 9.1 Build, test, package

| Command | Purpose | Expected |
| --- | --- | --- |
| `gofmt -l .` | style check | no output |
| `go vet ./...` | static analysis | pass |
| `go test ./... -count=1` | full test suite | pass |
| `go test -race ./... -count=1` | race detector | pass |
| `GOPROXY=off GOTOOLCHAIN=local go build ./...` | offline build | pass |
| `GOPROXY=off GOTOOLCHAIN=local go build -o bin/meshcompute ./cmd/meshcompute` | produce the binary | `bin/meshcompute` |
| `GOOS=windows GOARCH=amd64 go build ./cmd/meshcompute` | cross-compile Windows | `.exe` |
| `make dist` | release tarballs/zip + `SHA256SUMS` | `dist/` |
| `sh installers/install.sh` | user-scoped Linux install | `~/.local/bin/meshcompute` |
| `.\installers\install-prebuilt.ps1 -BinaryPath .\meshcompute-windows-amd64.exe -Sha256 <hash>` | user-scoped Windows install | `%LOCALAPPDATA%\meshcompute\bin` |

### 9.2 Group host

| Command | Purpose |
| --- | --- |
| `meshcompute install` | initialize user config; no downloads |
| `meshcompute config set listen IP:PORT` | set listen address before creating identity |
| `meshcompute config set backend_url URL` | point the agent at a different loopback backend |
| `meshcompute host` | create/resume a group using saved config |
| `meshcompute host --auto` | auto-select global IPv6, else private IPv4 |
| `meshcompute host --auto --ui 127.0.0.1:8083 --ttl 2h` | what the live test used |
| `meshcompute host --listen 10.10.10.1:7443 --advertise https://10.10.10.1:7443` | direct-cable group |
| `meshcompute host --listen 10.161.226.188:7443 --advertise https://10.161.226.188:7443 --ui 127.0.0.1:8082 --ttl 2h` | same-Wi-Fi group |
| `meshcompute host --listen 0.0.0.0:7443 --advertise https://PUBLIC:7443 --public` | public IPv4 group |
| `meshcompute host --port 7443` | port used by `--auto` |
| `meshcompute pair --ttl 30m` | new single-use invitation |
| `meshcompute revoke worker-<id>` | revoke an enrolled worker |
| `meshcompute peer add NAME https://IP:7443` | legacy direct-peer bookmark |
| `meshcompute cluster create` | legacy coordinator identity |

Notes:

- `--home DIR` must come **before** the subcommand.
- Changing `listen` after identity creation is refused: the address is part of
  the certificate identity. Create a new `--home` instead.
- `--ttl` maximum is 24 h.

### 9.3 Worker (Windows)

| Command | Purpose |
| --- | --- |
| `.\join.cmd` **[given]** | install if needed, prompt for invitation, start worker |
| `& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" join` | manual join |
| `"<invitation>" \| meshcompute join --stdin` | non-interactive join (what the `.cmd` files do) |
| `& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" start` **[given]** | resume with stored identity; no invitation |
| `& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" status` | worker's own view |
| `meshcompute ui` | dashboard for a standalone/worker node |
| `meshcompute doctor` | redacted diagnostics |

### 9.4 Status and diagnostics

| Command | Purpose |
| --- | --- |
| `meshcompute status` | local + worker health, resources, `backend_ready` |
| `meshcompute monitor --interval 5s` | repeat status until Ctrl-C |
| `meshcompute doctor` | shareable, redacted diagnostics |
| `meshcompute network probe worker-<id>` | authenticated RTT/throughput test |
| `meshcompute backend list` | adapter health and capabilities |
| `meshcompute model list` | registered models |

### 9.5 Model and backend

| Command | Purpose |
| --- | --- |
| `meshcompute model add qwen "C:\path\model.gguf"` | register absolute path, compute SHA-256 |
| `meshcompute model list` | show aliases, sizes, hashes |
| `meshcompute backend serve qwen --binary "...\llama-server.exe" --context 2048 --gpu-layers 99 --kv-bytes-per-token 28672` | verify model, admit memory, launch llama-server |
| `meshcompute backend serve qwen --context 2048 --gpu-layers 0 --kv-bytes-per-token 28672` | CPU-only run |
| `meshcompute config set backend_url http://127.0.0.1:8081` | use a non-default loopback port |

Rules encoded in `backend serve`:

- Model path must be absolute and registered.
- The file is re-hashed at launch.
- `--kv-bytes-per-token` is mandatory and must come from architecture, not file
  size.
- `--gpu-layers > 0` requires a GPU candidate that fits.
- `LLAMA_*` and `GGML_RPC*` environment variables are stripped.

### 9.6 Inference

| Command | Purpose |
| --- | --- |
| `meshcompute run qwen --node worker-<id> --prompt "..."` | one completion on a chosen worker |
| `meshcompute run qwen --node auto --prompt "..."` | pick the first ready node |
| `echo "prompt" \| meshcompute run qwen --node auto` | prompt from stdin |
| `meshcompute run qwen --node local --max-tokens 256 --context 2048` | local execution |

### 9.7 Dashboard

| Command | Purpose |
| --- | --- |
| `meshcompute ui --listen 127.0.0.1:8081` | standalone dashboard |
| `meshcompute host ... --ui 127.0.0.1:8083` | dashboard for a group |
| open `http://127.0.0.1:8081/?token=...` | authenticated UI |

The dashboard is loopback-only by design. Do not bind it to `0.0.0.0`.

### 9.8 Linux network diagnostics (host)

```sh
ip -br addr                     # addresses per interface
ip -br link                     # link state
ip route | grep default         # which interface carries the internet
ip -6 route                     # IPv6 routes
ss -ltn                         # listening TCP sockets
ss -tn state established '( sport = :7443 )'   # live worker sessions
ping -c 3 -W 2 10.10.10.2       # ICMP to a direct peer
ip neigh show dev enp3s0        # ARP table
ip -6 neigh show dev enp3s0     # NDP table
ethtool enp3s0 | grep -E 'Speed|Duplex|Link detected'
cat /sys/class/net/enp3s0/statistics/rx_packets
cat /sys/class/net/enp3s0/statistics/rx_errors
cat /var/lib/NetworkManager/dnsmasq-enp3s0.leases
tcpdump -i enp3s0 -n -c 10      # needs root
nmcli -t -f NAME,DEVICE,STATE con show --active
```

Purpose, in order: prove the address, prove the link, prove the route, prove
the listener, prove a live session, prove L2/L3 reachability, prove a peer is
actually transmitting.

### 9.9 Windows network diagnostics (worker) **[given]**

```powershell
Get-NetAdapter | Format-Table Name,Status,LinkSpeed,MediaType -AutoSize
Get-NetAdapter -Physical | Format-Table Name,InterfaceDescription,Status,LinkSpeed -AutoSize
Get-NetIPAddress -AddressFamily IPv4 | Format-Table InterfaceAlias,IPAddress,PrefixLength -AutoSize
netsh interface ipv4 show config
ping 10.10.10.1
Test-NetConnection 10.10.10.1 -Port 7443
arp -a | findstr 10.10.10
curl.exe -6 -s https://ifconfig.co
nvidia-smi
netstat -ano | findstr 8080
```

`Test-NetConnection` is more useful than `ping`: ICMP is often blocked while TCP
works.

### 9.10 Ethernet configuration — the complete set

**Linux: create the profile**

```sh
nmcli con add type ethernet ifname enp3s0 con-name meshcompute-direct \
  ipv4.method manual ipv4.addresses 10.10.10.1/24 ipv6.method link-local
nmcli con up meshcompute-direct
```

**Linux: check it**

```sh
ip -br addr show enp3s0
ethtool enp3s0 | grep -E 'Speed|Duplex|Link detected'
ip neigh show dev enp3s0
ping -c 3 -W 2 10.10.10.2
```

**Linux: DHCP variant (no Windows configuration needed)**

```sh
nmcli con mod meshcompute-direct ipv4.method shared ipv4.addresses 10.10.10.1/24
nmcli con up meshcompute-direct
cat /var/lib/NetworkManager/dnsmasq-enp3s0.leases
```

`ipv4.method shared` runs dnsmasq with `10.10.10.10–10.10.10.254` and hands out
an address to any Windows machine left on DHCP.

**Linux: link-local IPv6 variant**

```sh
nmcli con mod meshcompute-direct ipv6.method link-local
nmcli con up meshcompute-direct
ip -6 -br addr show enp3s0
ping -6 -c 3 -W 2 -I enp3s0 ff02::1
```

**Windows: static peer**

```powershell
Get-NetAdapter | Format-Table Name,Status,LinkSpeed,MediaType -AutoSize
netsh interface ipv4 set address name="Ethernet" static 10.10.10.2 255.255.255.0
Get-NetIPAddress -InterfaceAlias "Ethernet" -AddressFamily IPv4 | Format-Table
ping 10.10.10.1
Test-NetConnection 10.10.10.1 -Port 7443
arp -a | findstr 10.10.10
```

**Windows: back to DHCP**

```powershell
netsh interface ipv4 set address name="Ethernet" dhcp
```

**Linux: remove the profile**

```sh
nmcli con down meshcompute-direct
nmcli con delete meshcompute-direct
```

### 9.11 Firewall and router **[recommended]**

Host Linux:

```sh
sudo ufw status verbose
sudo ufw allow 7443/tcp
# firewalld equivalent
sudo firewall-cmd --add-port=7443/tcp --permanent && sudo firewall-cmd --reload
```

Host Windows (if the host were Windows):

```powershell
New-NetFirewallRule -DisplayName "MeshCompute group" -Direction Inbound `
  -Protocol TCP -LocalPort 7443 -Action Allow
```

Router (IPv4 only): forward **TCP 7443 → host LAN IP**. IPv6 needs no forward,
but the router's IPv6 firewall must allow inbound 7443.

### 9.12 External reachability

IPv6 (scans the caller's own address):

```sh
curl -sS --noproxy '*' \
 "https://www66.chappell-family.co.uk/cgi-bin6/ipscanfasttxt.cgi?includeexisting=-1&customport0=7443&customport1=&customport2=&customport3=&termsaccepted=1"
```

Look for `Port 7443 = TCPOPEN`. `TCPSTEALTH` means filtered/closed.

IPv4:

```sh
curl -s -X POST https://portchecker.io/api/v1/query \
  -H 'Content-Type: application/json' \
  -d '{"host":"157.50.2.104","ports":[7443]}'
```

`status:false` means closed.

From the friend's side:

```sh
curl -6 -s https://ifconfig.co        # macOS/Linux
```

```powershell
curl.exe -6 -s https://ifconfig.co    # Windows
```

### 9.13 Packaging and release

| Command/step | Purpose |
| --- | --- |
| `make dist` | builds Linux amd64/arm64 + Windows amd64 + checksums |
| GitHub Actions release workflow | publishes assets on tag |
| `sha256sum -c SHA256SUMS` | verify downloads |
| `.\join.cmd` | bundled installer + join |
| `.\install.cmd` | install only |

Release `v0.2.0` assets: Windows exe, Windows zip, Linux amd64/arm64 tarballs,
`SHA256SUMS`.

### 9.14 Cleanup

```sh
# stop experimental hosts
pkill -f 'meshcompute --home /home/USER/.meshcompute-direct'
pkill -f 'meshcompute --home /home/USER/.meshcompute-wifi'

# stop temporary file servers
pkill -f 'http.server 8080'
pkill -f 'http.server 8084'

# remove a cable profile
nmcli con down meshcompute-direct && nmcli con delete meshcompute-direct

# delete an experimental group (IRREVERSIBLE: identity, CA, tokens die)
rm -rf /home/USER/.meshcompute-direct
rm -rf /home/USER/.meshcompute-wifi
```

Keep `~/.meshcompute-auto` — it is the group the worker joined.

---

## 10. Troubleshooting decision tree

```
Worker does not appear in `status`
├── Is the `join`/`start` window open on Windows?
│   ├── no  → start it: meshcompute.exe start            (identity is saved)
│   └── yes
│       ├── does `status` say HTTP 503?                 → worker has no channel
│       │     ├── check Windows network / IPv6
│       │     └── restart the worker window
│       ├── does it say unreachable/timeout?            → host side
│       │     ├── ss -ltn | grep 7443                   (is the host listening?)
│       │     ├── external scan of the advertised IP    (is the port open?)
│       │     └── host firewall / IPv6 firewall / router forward
│       └── does it say refused?                        → identity/revocation
│             ├── was the invitation used already?
│             └── regenerate with `pair`
└── never joined at all → re-read the join error; it names the layer

Worker is healthy but `backend_ready: false`
├── is llama-server running?  netstat -ano | findstr 8080
├── is the model registered?  meshcompute model list
├── does /v1/models return exactly the alias?
├── does the KV estimate fit?  reduce --context or --gpu-layers
└── driver/CUDA mismatch?  nvidia-smi, llama-server log

`run` fails with "not ready or context exceeds capacity"
├── context request > server slot context  → lower --context
├── alias mismatch                          → register with the same name
└── backend not ready                       → start backend serve

Join fails with "security operation failed"
├── expired?      → pair a fresh one
├── already used? → pair a fresh one
└── truncated?    → copy the whole code, no line breaks

Cable link is up but nothing works
├── rx_packets not increasing → wrong far end / sleeping laptop
├── no DHCP lease             → no DHCP client there
└── ARP FAILED                → no host with that IP
```

## 11. Acceptance checklist

- [ ] IPv6 (or a working IPv4 forward) proven from outside.
- [ ] Host listening on the intended address (`ss -ltn`).
- [ ] Firewall allows TCP 7443 (and the router's IPv6 firewall, if any).
- [ ] Fresh invitation generated immediately before joining.
- [ ] Worker prints `Joined the group` and `event=agent_start`.
- [ ] `status` shows the worker healthy with a fresh timestamp.
- [ ] `backend_ready` becomes true after the GPU setup.
- [ ] llama-server log shows `loaded CUDA backend` and `listening on 127.0.0.1:8080`.
- [ ] `run qwen --node worker-<id>` returns content and tokens/second.
- [ ] Dashboard streams tokens.
- [ ] Worker can be resumed with `start` after the window closes.
- [ ] Unused hosts and temporary file servers are stopped.

---

## 12. Glossary (plain language)

| Term | Meaning |
| --- | --- |
| **CA** | Certificate authority. The group's own root of trust; it signs worker certificates. |
| **mTLS** | Mutual TLS: both sides present certificates. |
| **Pin** | A hash of the exact host certificate. Even a valid CA cannot impersonate the host. |
| **CSR** | Certificate signing request. The worker proves it owns a key before getting a certificate. |
| **Invitation** | Base64url JSON with endpoint, CA, pin, and a one-use token. |
| **Broker** | Host component that holds idle worker channels and leases one per request. |
| **Reverse tunnel** | The worker dials out to the host; no inbound port on the worker. |
| **Upgrade: meshcompute/1** | HTTP mechanism that turns a TLS connection into a raw message channel. |
| **Backend** | `llama-server` serving one GGUF on the worker's loopback. |
| **Registry** | JSON entries mapping a model alias to path + SHA-256. |
| **Admission** | Refusing to start or route when weights + KV + overhead do not fit. |
| **GGUF** | llama.cpp model file format. |
| **Q4_K_M** | A 4-bit quantization; smaller and faster, slight quality loss. |
| **KV cache** | Per-token attention memory; grows with context length. |
| **NAT** | Router rewrites addresses; blocks unsolicited inbound connections. |
| **CGNAT** | Carrier-grade NAT; port forwarding is impossible for the user. |
| **AP isolation** | Wi-Fi feature that blocks client-to-client traffic. |
| **DHCP** | Automatic address assignment. |
| **ARP / NDP** | IPv4 / IPv6 neighbour discovery. |
| **Loopback** | `127.0.0.1` / `::1`; only the local machine. |
| **SSE** | Server-sent events; used for token streaming. |
| **503** | Here: the broker has no idle channel for that worker. |

---

## 13. Appendices

### A. Invitation shape (placeholders, not a real secret)

```
base64url({
  "Endpoint": "https://[2409:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx]:7443",
  "CA":       "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
  "Pin":      "<64 hex>",
  "Token":    "<64 hex>",
  "Public":   true,
  "Reverse":  true
})
```

### B. Worker API

| Endpoint | Method | Body | Response |
| --- | --- | --- | --- |
| `/v1/status` | GET | — | resources, backend readiness, busy, model |
| `/v1/ping` | GET | — | liveness |
| `/v1/probe` | POST | — | RTT/throughput |
| `/v1/run` | POST | `{model,prompt,max_tokens,context}` | `{content,tokens_per_second}` |
| `/v1/run/stream` | POST | same | SSE chunks |

### C. Hard limits

| Limit | Value |
| --- | --- |
| Prompt | 1 MiB |
| Response | 8 MiB |
| Max output tokens | 32,768 |
| Context | 1,048,576 |
| Stream cap | 30 min |
| Enrollment timeout | 30 s |
| Dial timeout | 10 s |
| Dispatch wait | 5 s |
| Invitation TTL | ≤ 24 h |

### D. Config keys (`config.json`)

| Key | Meaning |
| --- | --- |
| `version` | schema version |
| `listen` | address the agent/host binds |
| `advertise` | address printed in invitations |
| `backend_url` | loopback llama.cpp origin (default `http://127.0.0.1:8080`) |
| `peers` | legacy direct-peer bookmarks |
| `group_host` | this state is a group host |
| `public` | public hosting explicitly allowed |

### E. State layout

```
<home>/
  config.json         # endpoints, backend URL, flags
  identity.json       # CA, node cert/key, pin, live tokens, revocations
  models/
    qwen.json         # {name, path, size_bytes, sha256}
```

Permissions: the directory is 0700; identity files are 0600. This is why exFAT
(no Unix modes) is unsuitable.

### F. Verified environment

- Host: Linux amd64, Go 1.26.8, RTX 3050 6 GB (discovery only in this run).
- Worker: Windows 11, Ryzen 5 7535HS, 16.3 GB RAM, RTX 3050 Laptop 4.3 GB,
  driver 566.14.
- Host addresses: global IPv6 `2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc`,
  LAN `10.161.226.188`, public IPv4 `157.50.2.104` (CGNAT).
- Verified upstream assets: llama.cpp `b12182` CUDA 12.4 (275 MB), CUDA runtime
  (374 MB), Qwen2.5-1.5B-Instruct Q4_K_M (986 MB, `content-length 986048768`).

### G. What "works" means for this project

1. A friend with no developer tools pastes one command and appears on the
   creator's dashboard.
2. The creator sees real hardware, not fabricated numbers.
3. A prompt runs entirely on one worker's GPU and returns tokens/second.
4. Nothing is exposed beyond one TLS port and a loopback dashboard.
5. The system never claims a capability it does not have (no pooled VRAM, no
   remote administration, no magic NAT traversal).

---

## 14. Immediate next steps (short list)

1. On the friend's Windows laptop, resume the worker:

```powershell
& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" start
```

2. In a second PowerShell window, install the backend:

```powershell
irm https://raw.githubusercontent.com/Aruneshw/gpu_toolkit/main/scripts/windows-gpu-setup.ps1 | iex
```

3. On the host, confirm:

```sh
./bin/meshcompute --home "$HOME/.meshcompute-auto" status
```

`backend_ready: true` is the signal to run the first GPU inference.

4. Keep the join/start window open while testing, and stop the unused hosts on
   `:8081` and `:8082` to avoid confusion.


