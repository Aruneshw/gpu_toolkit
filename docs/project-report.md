# MeshCompute — full project report

**Date:** 2026-11-07
**Repository:** https://github.com/Aruneshw/gpu_toolkit (branch `main`, HEAD `fdcc68e` + `scripts/windows-gpu-setup.ps1`)
**Release:** https://github.com/Aruneshw/gpu_toolkit/releases/tag/v0.2.0
**Machines:** creator Linux laptop (host) and a friend's Windows 11 laptop (worker)

> This is the short overview. The complete narrative — architecture explained
> from first principles, the full history, every failed attempt with evidence,
> the proposed architecture v2, and the complete command reference — is in
> [full-project-report.md](full-project-report.md).

This report records what was built, how it works, every connection method that was
tried, what failed and why, what is still missing, and a clean rebuild procedure
with every command and its purpose.

---

## 1. Executive summary

MeshCompute is a Go single-binary tool for running inference on **separate
machines**, each with its own local model and its own GPU. It is deliberately
*not* a VRAM-pooling or tensor-splitting runtime: one request runs on **one**
node.

What was built and verified:

| Item | State |
| --- | --- |
| Creator-hosted group relay (no central server) | built, running |
| TLS 1.3 mutual authentication, Ed25519 identities | built, verified |
| Single-use, expiring invitations | built, verified |
| Windows worker joins with one `join.cmd` | built, verified |
| Remote worker over IPv6 from another network | **verified live** |
| Worker hardware/GPU telemetry on host | **verified live** (RTX 3050 Laptop, 4.3 GB) |
| GPU inference on the worker | **not yet** — backend/model not installed on the friend's laptop |
| VRAM pooling / llama.cpp RPC | deliberately not implemented |
| Remote administration of a worker | deliberately not implemented |

The headline result: **the friend's laptop did connect and authenticate over the
public internet** — verified at 05:30–05:40 UTC on 2026-11-07, with a live TLS
session and full hardware telemetry. At 05:51 UTC the same worker disappeared
from `status` (`agent returned HTTP 503`), which means its `join` process is no
longer running (window closed, or the laptop slept / lost IPv6). A 503 from the
host broker is *not* a network failure and needs no new invitation: restarting
the worker with `meshcompute start` reconnects using the stored identity.

The remaining functional gap is still the same one: llama.cpp + a GGUF model
are not installed on the friend's laptop, so `backend_ready` is false.

---

## 2. Goal and constraints

The original objective was a creator-hosted group toolkit:

1. The creator runs the relay on their own laptop.
2. Friends join with **one PowerShell command** and a pasted invitation.
3. The creator can see and use the friends' GPUs.
4. No Tailscale, VPN, or separate networking product; everything inside
   MeshCompute.
5. No shared central relay; each group is a separate trust domain.
6. Windows and Linux laptops.
7. The dashboard is localhost-only and token-protected; **no remote
   administration**.
8. Do not claim pooled VRAM: one request = one node.

Constraints that shaped the design:

- Friends must not install Git or Go.
- The friend's laptop cannot be configured manually or controlled remotely by
  the creator.
- Ordinary users do not know their public IP, so address selection must be
  automatic or clearly explained.

---

## 3. Architecture

```
                    creator's laptop (Linux)                       friend's laptop (Windows)
 ┌──────────────────────────────────────────────┐        ┌──────────────────────────────────┐
 │  meshcompute host                            │        │  meshcompute join / start        │
 │                                              │        │                                  │
 │  CLI / dashboard ──► local agent ──► relay   │◄───────┤  worker agent (outbound TLS 1.3) │
 │  127.0.0.1:8081-83     (mTLS)      :7443 TLS │  IPv6  │                                  │
 │                                              │        │  └─► llama-server 127.0.0.1:8080 │
 │  group CA + invitations + revocation         │        │        (CUDA, RTX 3050)          │
 └──────────────────────────────────────────────┘        └──────────────────────────────────┘
```

### Components

| Component | File(s) | Role |
| --- | --- | --- |
| CLI | `internal/cli` | `host`, `pair`, `join`, `start`, `status`, `run`, `model`, `backend`, `ui` |
| Identity & enrollment | `internal/security` | Ed25519 CA + node certs, invitation tokens, pinning, revocation |
| Relay / tunnel | `internal/tunnel`, `internal/agent/relay.go` | Reverse HTTP over a TLS connection with `Upgrade: meshcompute/1` |
| Worker agent | `internal/agent` | Executes `/v1/status`, `/v1/run`, `/v1/run/stream`; proxies to llama.cpp |
| Backend adapter | `internal/backend` | llama.cpp contract (`/health`, `/v1/models`, `/props`, `/tokenize`, `/completion`) |
| Model registry | `internal/model` | `model add` stores absolute path, size, SHA-256; verified at launch |
| Admission | `internal/scheduler`, `internal/network` | Weight + KV + overhead fit check; network RTT/throughput check |
| Dashboard | `internal/dashboard` | Loopback-only HTML/JSON/SSE UI with bearer token |
| Packaging | `install.cmd`, `join.cmd`, `installers/`, `Makefile` | Windows ZIP + Linux tarballs, CI release |

### Request flow (single node)

```
meshcompute run qwen --node worker-<id>
  └─ mTLS request to the host's local agent: /v1/workers/<id>/v1/status
       └─ host leases one worker connection and forwards the request
            └─ worker agent: model registered? backend healthy? context fits?
                 └─ llama-server /completion (GPU)
                      └─ response returns over the same leased connection
```

A connection carries **one** dispatch. After the response, the worker opens a
new outbound connection. That is why the worker log repeatedly prints
`event=worker_connection state=reconnecting` — it is replenishment, not
failure.

---

## 4. Protocols and cryptography

### 4.1 Invitation format

`pair` produces `base64url(JSON)` with no padding:

```json
{
  "Endpoint": "https://[2409:...:f4fc]:7443",
  "CA":   "<PEM bundle of the group CA>",
  "Pin":  "<64 hex chars = SHA-256 of the host certificate DER>",
  "Token":"<64 hex chars = 32 random bytes>",
  "Public": true,
  "Reverse": true
}
```

- **Single use:** the host removes the token on first successful enrollment.
- **Expiring:** default 5 minutes; this run used 2 hours; maximum 24 hours.
- **Bearer secret:** anyone holding it can enroll until it expires or is used.
- **Out of band:** chat/email/QR; never served over the network.

### 4.2 Enrollment

1. Worker connects to `Endpoint` over **TLS 1.3 only** (`MinVersion:
   TLS13`), with the CA bundle from the invitation as the only root.
2. The host certificate must hash to `Pin`; redirects are forbidden, proxies
   disabled.
3. Worker generates an **Ed25519** key, sends a CSR plus the token to `/enroll`.
4. Host checks the token, signs a leaf cert with CN `worker-<64 hex>`,
   consumes the token, and returns the certificate.
5. Worker stores identity in its state directory; worker TLS then requires a
   client certificate signed by the group CA.

### 4.3 Worker transport

- Outbound TCP to host `:7443`, TLS 1.3 with client certificate.
- HTTP/1.1 request with `Connection: Upgrade` and `Upgrade: meshcompute/1`.
- Host answers `101 Switching Protocols` and then writes one HTTP request over
  the raw stream; the worker replies and the connection is consumed.
- Allowed operations only: `/v1/status`, `/v1/ping`, `/v1/probe`,
  `/v1/run`, `/v1/run/stream`. Nothing else is reachable, by design.
- Deadlines: 10 s connect, 5 s dispatch wait, 15 s stream write deadline,
  30 min stream cap, 8 MiB response cap, 1 MiB prompt cap.

### 4.4 Backend contract (llama.cpp)

- `/health` → `{"status":"ok"}`
- `/v1/models` → exactly one model whose id equals the alias
- `/props` → `default_generation_settings.n_ctx`
- `/tokenize` → prompt token count for admission
- `/completion` → completion (streaming or not)

`backend serve` launches:

```
llama-server --model <gguf> --alias <name> --host 127.0.0.1 --port 8080
             --ctx-size 2048 --parallel 1 --n-gpu-layers 99 --no-context-shift
```

It refuses to start without an architecture-derived `--kv-bytes-per-token`, and
it strips ambient `LLAMA_*` / `GGML_RPC*` variables so a launcher cannot enable
RPC or downloads implicitly.

### 4.5 Dashboard

- Bound to `127.0.0.1` only.
- Requires `?token=<random>`; no remote administration endpoints.
- Aggregates telemetry and streams tokens from one selected node.

---

## 5. Ports and addresses used in this run

| Machine | Address | Port | Purpose | Externally reachable |
| --- | --- | --- | --- | --- |
| Host | `2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc` | 7443/TCP | group relay (TLS + upgrade) | **yes (TCPOPEN)** |
| Host | `10.161.226.188` (Wi-Fi) | 7443/TCP | LAN-only host attempt | LAN only |
| Host | `10.10.10.1` (direct cable) | 7443/TCP | cable host attempt | cable only (never worked) |
| Host | `157.50.2.104` (public IPv4) | 7443/TCP | router-forward attempt | **no (closed)** |
| Host | `127.0.0.1` | 8083 | dashboard for the IPv6 group | loopback |
| Host | `127.0.0.1` | 8081, 8082 | dashboards of the abandoned hosts | loopback |
| Host | `0.0.0.0` | 8080 | temporary HTTP file share (IPv4) | LAN only |
| Host | `*` (IPv6) | 8084 | temporary GPU-script share | **filtered by firewall** |
| Worker | `127.0.0.1` | 8080 | llama-server backend | loopback only |
| Worker | ephemeral | → host 7443 | outbound worker connection | n/a (outbound) |

---

## 6. Chronological record of everything that was tried

### 6.1 Baseline: local CPU inference (2026-11-06)

- Go tests, race tests, cross-builds, installers, dashboard streaming all
  verified. Real llama.cpp CPU inference with an LFM3 1.7B GGUF on this laptop.
- GPU inference was not exercised (no CUDA runtime at the time).

### 6.2 Creator-hosted relay implemented and released

- Commits `2f4053c`, `7b9b797`; release `v0.2.0` with Windows exe/ZIP, Linux
  amd64/arm64 tarballs, `SHA256SUMS`.
- `host --auto` (prefer global IPv6, else private IPv4) and better join errors
  added in `fdcc68e`.

### 6.3 Tailscale / VPN idea (rejected)

Rejected at requirements stage: the user explicitly forbade a separate
networking product. No Tailscale commands were run in this run; the code only
reports whether it is available in `doctor`.

### 6.4 Direct Ethernet cable between the two laptops (failed)

Configured a NetworkManager profile on `enp3s0`:

```sh
nmcli con add type ethernet ifname enp3s0 con-name meshcompute-direct \
  ipv4.method manual ipv4.addresses 10.10.10.1/24 ipv6.method link-local
nmcli con up meshcompute-direct
ip -br addr show enp3s0
```

Then a static peer setup on Windows (`netsh ... static 10.10.10.2`), plus a
DHCP variant (`ipv4.method shared` → dnsmasq range 10.10.10.10–254).

Measured facts on the Linux side:

```
enp3s0  UP  10.10.10.1/24  fe80::c5a7:43c9:43f1:a247/64
Speed: 1000Mb/s  Duplex: Full  Link detected: yes

ping 10.10.10.2        → Destination Host Unreachable
ip neigh show          → 10.10.10.2 FAILED
DHCP leases            → none (ever)
rx frames in 10 s      → 0
```

Interpretation: the link partner was powered (1 Gbps link) but sent **zero
frames** — no DHCP DISCOVER, no ARP, no IPv6 neighbour discovery. A running
Windows laptop on that cable would have sent DHCP within seconds. So the far
end was not that laptop (sleeping laptop, dock, switch, or wall port). The test
was abandoned in favour of the remote path. This was never "solved"; it was
correctly diagnosed as a physical-layer problem.

### 6.5 Same-Wi-Fi attempt (inconclusive, likely client isolation)

- Host bound to the LAN address: `10.161.226.188:7443`, dashboard 8082.
- Temporary HTTP share on `0.0.0.0:8080` serving `index.html`,
  `JOIN-MESHCOMPUTE.cmd`, `JOIN-MESHCOMPUTE-WIFI.cmd`.
- Verified locally from both addresses (`http 200` from `10.161.226.188:8080`
  and `10.10.10.1:8080`).
- The friend's laptop could not reach them. The hostel/campus Wi-Fi
  (`BIT-*` SSIDs) very likely enforces **AP/client isolation**, which blocks
  client-to-client traffic. Not directly measurable from the host, and not
  fixable in software; a phone hotspot or cable would be required.

### 6.6 Public IPv4 attempt (blocked by NAT)

- Public IPv4 seen locally: `157.50.2.104`; LAN `10.161.226.188/24`, gateway
  `10.161.226.96` (a private gateway, typical of campus/shared NAT).
- External check of `157.50.2.104:7443` and `:8080`:

```
{"port":7443,"status":false}
{"port":8080,"status":false}
```

- Conclusion: the network is behind NAT/CGNAT with no port forwarding. The
  friend can never reach this IPv4 without a router forward — and only the
  **host's** router would need it, never the friend's.

### 6.7 IPv6 attempt (success)

- `host --auto` selected the global IPv6 address:

```
Selected [2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc]:7443
  (global IPv6; friends connect directly without router port forwarding)
```

- External scan of that address on 7443 returned **TCPOPEN**.
- The friend pasted the invitation; enrollment succeeded:

```
Joined the group. Worker connections are outbound and reconnect automatically.
2026/11/07 05:30:38 event=agent_start transport=mutual_tls
```

- Host status at 05:33 and 05:40 UTC showed the worker healthy with full
  telemetry:

```
worker-d73500f00498e65...  healthy true  backend_ready false
Windows amd64 | AMD Ryzen 5 7535HS | 12 cores | 16.3 GB RAM
GPU NVIDIA RTX 3050 Laptop 4.3 GB | driver 566.14
```

- An established TLS session was visible on the host:

```
[2409:...:f4fc]:7443 ← [2409:40f4:210e:4b05:977:b4ae:1fe5:57b1]:65518
```

### 6.8 Join error seen once

```
meshcompute: join failed: verify invitation expiry and host reachability:
security operation failed
```

This line appeared **before** the successful attempt (first paste). Cause: a
stale/expired or already-consumed token, or a truncated paste. The second paste
succeeded, producing `Joined the group`. Single-use is intentional.

### 6.9 "Reconnecting" confusion

The worker window prints `event=worker_connection state=reconnecting` about
every 30 s. The worker was healthy the whole time (fresh `collected_at`, live
TLS session). The line is emitted whenever a connection ends with a non-nil
error — including the normal close after one dispatch — and the loop
immediately opens a fresh connection. It is not a connectivity failure.

### 6.10 GPU backend setup (pending)

- Verified upstream artefacts and sizes:
  - llama.cpp `b12182` Windows CUDA 12.4: 275 MB
  - CUDA runtime DLLs (cuBLAS/cuDART): 374 MB
  - Qwen2.5-1.5B-Instruct Q4_K_M: 986 MB (HTTP 200, `content-length 986048768`)
- Added `scripts/windows-gpu-setup.ps1` to the repo so the friend runs one
  line, which downloads, extracts, registers the model, and starts the backend.
- Not yet executed on the friend's laptop.

---

## 7. Failure analysis

| # | Symptom | Root cause | Fix / status |
| --- | --- | --- | --- |
| 1 | Cable link 1 Gbps but `ping`/ARP fail | Far end sent zero frames (sleeping laptop, dock, switch or wall port) | Diagnosed with `rx_packets` and DHCP leases; abandoned, remote path used |
| 2 | DHCP server received no DISCOVER | No live Windows peer on the segment | Same as #1; not a DHCP bug |
| 3 | Friend could not open `http://10.161.226.188:8080` | AP/client isolation on campus Wi-Fi | Not fixable in software; use hotspot, cable, or internet path |
| 4 | Public IPv4 port closed | NAT/CGNAT, no router forward | Use IPv6 (works) or configure a real forward |
| 5 | Join error "security operation failed" | Invitation already used/expired, or truncated paste | Issue a fresh `pair`; paste the full code |
| 6 | `state=reconnecting` every 30 s | Normal one-dispatch-per-connection replenishment | No action; verify with `status` |
| 7 | `backend_ready: false` | No llama.cpp/model installed on the worker | Run the GPU setup script |
| 8 | Could not "control" the friend's laptop | No remote-shell/desktop channel exists; Windows has no remote access configured | Impossible by design; reduce to one pasted command |
| 9 | Script share on `:8084` not reachable | Host firewall allows only 7443 | Served the script from GitHub raw instead |
| 10 | Multiple hosts on 8081/8082/8083 caused token/identity confusion | Three experimental groups left running | Stop unused hosts; use one `--home` per group |

### Which protocols connected and which did not

| Path | Protocol | Result |
| --- | --- | --- |
| IPv6 internet | TCP + TLS 1.3 + HTTP upgrade | **connected** |
| IPv4 internet | TCP 7443 | never connected (closed / CGNAT) |
| Same Wi-Fi LAN | TCP 7443 / HTTP 8080 | local only; friend blocked (isolation) |
| Direct cable | Ethernet L2 + IPv4/IPv6 | no peer frames; never connected |
| Tailscale/VPN | — | intentionally not used |

---

## 8. Current verified state (2026-11-07)

Host process:

```
./bin/meshcompute --home ~/.meshcompute-auto host --auto \
  --ui 127.0.0.1:8083 --ttl 2h
```

Group endpoint: `https://[2409:40f4:210e:4b05:feea:f4b1:b93d:f4fc]:7443`
Dashboard: `http://127.0.0.1:8083/?token=2a526d3c39142370001209e005655163205699fb153c2efdaa911f9398b0d2ad`

At 05:40 UTC the worker was healthy:

```
worker-d73500f00498e65...  healthy true  backend_ready false
Windows amd64 | AMD Ryzen 5 7535HS | 12 cores | 16.3 GB RAM
GPU NVIDIA RTX 3050 Laptop 4.3 GB | driver 566.14 | 323 GB free disk

Established session:
[2409:...:f4fc]:7443 ← [2409:40f4:210e:4b05:977:b4ae:1fe5:57b1]:65518
```

At 05:51 UTC the same entry became:

```json
{
  "name": "worker-d73500f00498e65...",
  "healthy": false,
  "error": "agent returned HTTP 503"
}
```

`HTTP 503` from the host means "no worker connection is currently leased/idle":
the worker process stopped (the `join` window was closed, or the laptop slept
or changed networks). The identity is still valid, so no new invitation is
needed.

Missing: llama.cpp + GGUF + running backend on the worker.

---

## 9. Why it still "cannot connect" — the actual answer

Three layers are easy to confuse, and each has a different symptom:

1. **Enrollment** (identity): done once with the invitation. Stored in
   `%APPDATA%\meshcompute`. Survives reboots and window closes.
2. **Worker connection** (transport): the `meshcompute` process must be
   **running** on the Windows laptop. It is not a background service.
   - Worker healthy → host `status` shows it with a fresh `collected_at`, and a
     TLS session appears in `ss` on the host.
   - Worker stopped/sleeping → host returns **`agent returned HTTP 503`**
     ("no connection available right now"). That is not a firewall or IPv6
     problem.
3. **Inference backend** (llama.cpp + model): independent of 2; it makes
   `backend_ready` true.

At 05:40 UTC layer 2 was working (healthy worker, live session). At 05:51 UTC
the worker entry turned unhealthy with `HTTP 503`, so **the `join` window on the
friend's laptop is closed or the laptop slept**. The network path is unchanged.

Resume layer 2 without a new invitation (identity already exists):

```powershell
& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" start
```

Keep that window open. Then check on the host:

```sh
./bin/meshcompute --home ~/.meshcompute-auto status
```

A healthy result looks like `healthy: true` and `collected_at` a few seconds
old. Only when it is healthy does it make sense to start the GPU backend
(layer 3).

---

## 10. What is deliberately not implemented

- **VRAM pooling / tensor parallelism / llama.cpp RPC** — a request is one node.
- **Remote administration** — no shell, no file push, no remote config.
- **NAT traversal** — IPv6 or an explicit port forward only.
- **Automatic model download** — `model add` never downloads.
- **Automatic backend install** — the helper script is separate from the binary.
- **Resilience to host restart** — a new host with a different address is a new
  group identity (the certificate binds the listen/advertise address), so
  workers must re-enroll.
- **Windows service** — the worker stays foreground.

---

## 11. Rebuild from scratch — the reliable procedure

### Phase 0 — prerequisites

- Host: Linux with a **global IPv6** address (preferred) or a router that can
  forward TCP 7443.
- Worker: Windows 11 x64, NVIDIA GPU + recent driver.
- Both machines' clocks correct; no VPN/proxy.

Check IPv6 on both:

```sh
# Host (Linux)
ip -6 -br addr show scope global
curl -6 -s https://ifconfig.co
```

```powershell
# Worker (Windows)
curl.exe -6 -s https://ifconfig.co
```

If either prints no IPv6 address, use IPv4 + router forwarding instead.

### Phase 1 — build or download

Linux:

```sh
cd gpu_toolkit
GOPROXY=off GOTOOLCHAIN=local go build -o bin/meshcompute ./cmd/meshcompute
./bin/meshcompute version
```

Windows: download `meshcompute-windows-amd64.zip` from Releases, extract, run
`join.cmd`.

### Phase 2 — start the group host (creator's laptop only)

```sh
mkdir -p ~/mesh-group
./bin/meshcompute --home ~/mesh-group host --auto --ui 127.0.0.1:8081 --ttl 30m
```

Purpose of each flag:

- `--home ~/mesh-group`: one independent group (CA, identities, tokens) per
  directory. Never mix groups.
- `host`: create or resume the group and print one invitation + dashboard.
- `--auto`: choose global IPv6 first, otherwise private IPv4, bound to wildcard.
- `--ui 127.0.0.1:8081`: loopback dashboard only.
- `--ttl 30m`: invitation lifetime (max 24 h).

If you must use IPv4:

```sh
./bin/meshcompute --home ~/mesh-group host \
  --listen 0.0.0.0:7443 \
  --advertise https://203.0.113.10:7443 \
  --public --ui 127.0.0.1:8081
```

Then forward TCP 7443 on the **host's** router to the host's LAN IP, and verify:

```sh
# from an external network
curl -k --max-time 5 https://203.0.113.10:7443/
```

### Phase 3 — invite

```sh
./bin/meshcompute --home ~/mesh-group pair --ttl 30m
```

- One token, one enrollment. Do not reuse; do not post publicly.
- If the friend fails, issue a new one (`pair` again).

### Phase 4 — worker joins (friend's Windows laptop)

```powershell
# from the extracted ZIP folder, ordinary PowerShell
.\join.cmd
# paste the invitation, press Enter; keep the window open
```

Manual form:

```powershell
& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" join
```

Verify on the host:

```sh
./bin/meshcompute --home ~/mesh-group status
./bin/meshcompute --home ~/mesh-group monitor --interval 5s
```

### Phase 5 — GPU backend on the worker

```powershell
# one line, downloads llama.cpp CUDA + Qwen2.5 1.5B, registers, serves
irm https://raw.githubusercontent.com/Aruneshw/gpu_toolkit/main/scripts/windows-gpu-setup.ps1 | iex
```

Manual equivalent (same effect):

```powershell
$root = "$env:LOCALAPPDATA\meshcompute"
New-Item -ItemType Directory -Force -Path "$root\llama","$root\models" | Out-Null

curl.exe -L -o "$env:TEMP\llama.zip"  https://github.com/ggml-org/llama.cpp/releases/download/b12182/llama-b12182-bin-win-cuda-12.4-x64.zip
curl.exe -L -o "$env:TEMP\cudart.zip" https://github.com/ggml-org/llama.cpp/releases/download/b12182/cudart-llama-bin-win-cuda-12.4-x64.zip
Expand-Archive "$env:TEMP\llama.zip"  "$root\llama" -Force
Expand-Archive "$env:TEMP\cudart.zip" "$root\llama" -Force

curl.exe -L -o "$root\models\Qwen2.5-1.5B-Instruct-Q4_K_M.gguf" `
  https://huggingface.co/bartowski/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf

$mc = "$root\bin\meshcompute.exe"
& $mc model add qwen "$root\models\Qwen2.5-1.5B-Instruct-Q4_K_M.gguf"
& $mc backend serve qwen --binary "$root\llama\llama-server.exe" `
  --context 2048 --gpu-layers 99 --kv-bytes-per-token 28672
```

Why `28672`: 28 layers × 2 KV heads × 128 head_dim × 2 (K,V) × 2 bytes.

### Phase 6 — run inference from the host

```sh
./bin/meshcompute --home ~/mesh-group run qwen --node worker-<id> --prompt "hello"
```

Or use the dashboard printed by `host` (loopback + token).

### Phase 7 — verify

```sh
./bin/meshcompute --home ~/mesh-group status        # backend_ready must be true
./bin/meshcompute --home ~/mesh-group doctor        # redacted diagnostics
./bin/meshcompute --home ~/mesh-group network probe worker-<id>
```

### Phase 8 — clean shutdown

```sh
# stop experimental hosts
pkill -f 'meshcompute --home /home/USER/.meshcompute-wifi'
pkill -f 'meshcompute --home /home/USER/.meshcompute-direct'

# stop temporary file servers
pkill -f 'http.server 8080'
pkill -f 'http.server 8084'

# remove the cable profile
nmcli con down meshcompute-direct
nmcli con delete meshcompute-direct
```

---

## 12. Ethernet / direct-link appendix (every command used)

All Linux commands run on the creator laptop; Windows commands on the friend's
laptop.

### 12.1 Create the direct-link profile (Linux)

```sh
nmcli con add type ethernet ifname enp3s0 con-name meshcompute-direct \
  ipv4.method manual ipv4.addresses 10.10.10.1/24 ipv6.method link-local
```
Creates a dedicated profile so normal Wi-Fi is untouched.

```sh
nmcli con up meshcompute-direct
```
Activates it.

```sh
ip -br addr show enp3s0
ethtool enp3s0 | grep -E 'Speed|Duplex|Link detected'
```
Shows IP, link speed, duplex, and whether the PHY sees a partner.

### 12.2 Give the Windows side an address

```powershell
Get-NetAdapter | Format-Table Name,Status,LinkSpeed,MediaType -AutoSize
```
Lists adapters; `Status` must be `Up` and `MediaType` `802.3` for real Ethernet.

```powershell
netsh interface ipv4 set address name="Ethernet" static 10.10.10.2 255.255.255.0
```
Static peer address (replace `Ethernet` with the real adapter name).

```powershell
Get-NetIPAddress -InterfaceAlias "Ethernet" -AddressFamily IPv4 | Format-Table
ping 10.10.10.1
Test-NetConnection 10.10.10.1 -Port 7443
arp -a | findstr 10.10.10
```
Confirms address, ICMP, TCP reachability, and the neighbour table.

Undo on Windows:

```powershell
netsh interface ipv4 set address name="Ethernet" dhcp
```

### 12.3 DHCP alternative (no Windows configuration)

```sh
nmcli con mod meshcompute-direct ipv4.method shared ipv4.addresses 10.10.10.1/24
nmcli con up meshcompute-direct
```

`ipv4.method shared` starts NetworkManager's dnsmasq:

```
dnsmasq --dhcp-range=10.10.10.10,10.10.10.254,3600
        --listen-address=10.10.10.1
        --leasefile=/var/lib/NetworkManager/dnsmasq-enp3s0.leases
```

Check for a lease:

```sh
cat /var/lib/NetworkManager/dnsmasq-enp3s0.leases
```

### 12.4 Diagnostic commands that proved the cable was not connected

```sh
ping -c 3 -W 2 10.10.10.2          # Destination Host Unreachable
ip neigh show dev enp3s0           # 10.10.10.2 FAILED
ip -6 neigh show dev enp3s0        # empty
ping -6 -c 3 -W 2 -I enp3s0 ff02::1   # only our own link-local replies
```

```sh
a=$(cat /sys/class/net/enp3s0/statistics/rx_packets); sleep 10
b=$(cat /sys/class/net/enp3s0/statistics/rx_packets)
echo $((b-a))                      # 0 frames received in 10 s
```

Zero received frames is decisive: a live peer sends DHCP/ARP/NDP.

### 12.5 Link-local check

```sh
nmcli con mod meshcompute-direct ipv6.method link-local
nmcli con up meshcompute-direct
ip -6 -br addr show enp3s0
```
Gives `fe80::…/64` without needing a router; still requires a live peer.

---

## 13. Diagnostic command reference

### Host / Linux

| Command | Purpose |
| --- | --- |
| `ss -ltn \| grep 7443` | confirm the relay is listening on the intended address |
| `ss -tn state established '( sport = :7443 )'` | show live worker TLS sessions |
| `ip -br addr` | check which address the host actually bound |
| `ip route \| grep default` | confirm which interface carries internet |
| `./bin/meshcompute status` | health, resources, `backend_ready` |
| `./bin/meshcompute doctor` | redacted diagnostics to share |
| `./bin/meshcompute monitor --interval 5s` | continuous health |
| `tail -f host.log` | enrollments, agent starts, errors |

### Worker / Windows

| Command | Purpose |
| --- | --- |
| `Get-NetAdapter` | adapter names, status, speed |
| `Test-NetConnection HOST -Port 7443` | TCP reachability (better than ping) |
| `curl.exe -6 -s https://ifconfig.co` | does this laptop have IPv6? |
| `& "$env:LOCALAPPDATA\meshcompute\bin\meshcompute.exe" status` | worker-side view |
| `& ...\meshcompute.exe start` | resume a worker without a new invitation |
| `nvidia-smi` | driver and VRAM sanity check |
| `netstat -ano \| findstr 8080` | is llama-server listening? |

### External reachability checks

```sh
# IPv6 port scan of the caller's own address
curl -sS --noproxy '*' "https://www66.chappell-family.co.uk/cgi-bin6/ipscanfasttxt.cgi?includeexisting=-1&customport0=7443&customport1=&customport2=&customport3=&termsaccepted=1"
```

```sh
# IPv4 port check API
curl -s -X POST https://portchecker.io/api/v1/query \
  -H 'Content-Type: application/json' \
  -d '{"host":"157.50.2.104","ports":[7443]}'
```

---

## 14. Mistakes made (and the fix)

1. **Debugged the protocol before proving the link.** The cable test should
   have started by checking received frame counters and DHCP requests; instead
   the host and invitations were built first.
2. **Assumed the far end of the cable was the laptop.** A dock/switch/wall port
   also shows 1 Gbps. Zero frames is the tell.
3. **Reused/expired invitations.** They are single-use by design; always
   `pair` a fresh one per attempt.
4. **Ran three hosts at once** (direct, Wi-Fi, IPv6) with three dashboards and
   tokens, which caused confusion about which group was live.
5. **Bound the file server to `0.0.0.0`** (IPv4-only), so it could not serve the
   friend over IPv6; `--bind ::` was needed, and the firewall still blocked
   8084, so GitHub raw was used instead.
6. **Expected port forwarding on the friend's side.** Only the host's router
   ever needs a forward; IPv6 needs none.
7. **Treated `state=reconnecting` as an error.** It is the normal replenish
   cycle; `status` is the source of truth.
8. **Tried to "take control" of the friend's laptop.** No such channel exists;
   the correct design goal is one pasted command.
9. **Used an exFAT SSD for `--home`.** It cannot provide 0700 permissions; the
   state directory must live on ext4/NTFS (`$HOME`).
10. **Conflated "worker connected" with "GPU ready".** Connection and backend
    readiness are independent signals.

---

## 15. Security notes

- Group identity is a private CA created on the host; nothing is shared with a
  third party.
- Enrollment is pinned to the host certificate hash and restricted to TLS 1.3.
- Worker tokens are single-use and expire.
- Workers can only answer five fixed operations; there is no shell or file API.
- The dashboard binds to loopback; the token appears in the URL, so avoid
  pasting it into shared screenshots.
- A public host (`--public`) is opt-in; the invitation remains the only way in.
- Revocation exists for a node id, but a lost invitation cannot be "un-sent" —
  it can only expire or be consumed.

---

## 16. Acceptance checklist for the rebuild

- [ ] Both machines have IPv6 (or the host router forwards TCP 7443).
- [ ] Host prints a global address and `ss -ltn` shows it listening.
- [ ] External scan reports TCPOPEN on 7443.
- [ ] Fresh invitation generated immediately before the join.
- [ ] Worker prints `Joined the group` and `event=agent_start transport=mutual_tls`.
- [ ] Host `status` shows the worker healthy with a fresh timestamp.
- [ ] Backend script ends with `loaded CUDA backend` and `listening on 127.0.0.1:8080`.
- [ ] Host `status` shows `backend_ready: true`.
- [ ] `run qwen --node worker-<id>` returns text and a tokens/second figure.
- [ ] Dashboard streams tokens.
- [ ] Unused hosts and temporary file servers are stopped.

---

## 17. Immediate next step

On the friend's Windows laptop, in a new PowerShell window (keep the join window
open):

```powershell
irm https://raw.githubusercontent.com/Aruneshw/gpu_toolkit/main/scripts/windows-gpu-setup.ps1 | iex
```

When it prints `listening on 127.0.0.1:8080`, the host will report
`backend_ready: true` and inference can run on the RTX 3050.
