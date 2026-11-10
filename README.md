<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img alt="MeshCompute" src="docs/assets/logo-light.svg" width="520">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/Aruneshw/gpu_toolkit/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Aruneshw/gpu_toolkit/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/Aruneshw/gpu_toolkit/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/Aruneshw/gpu_toolkit?color=2ea44f"></a>
  <img alt="Go" src="https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go&logoColor=white">
  <img alt="Platforms" src="https://img.shields.io/badge/platform-Linux%20%7C%20Windows-555">
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
</p>

<p align="center">
  <b>Bring your friends' laptops and GPUs together in one dashboard, from anywhere.</b><br>
  One host, any number of friends, no VPN, no port forwarding, no IPv6 required.
</p>

---

## What it does

- **One dashboard for every GPU.** Each friend's laptop joins your group and
  shows up live: GPU model, VRAM used/free, utilisation, RAM, models and
  backend state, with totals across all laptops.
- **Joining is one key.** You click *Generate invitation*; your friend pastes
  the key into `win.bat` or `linux.sh`. Their computer creates its own key pair
  automatically. Keys are single use and expire.
- **Works from any network.** By default the host goes through the public relay
  **bore.pub**, so it does not matter that phone hotspots rotate IPv6 every few
  minutes, that a campus is behind CGNAT, or that Wi-Fi isolates clients.
- **Run AI on any member.** Send a prompt to a chosen laptop's `llama.cpp`
  backend and stream the answer back into the dashboard.
- **Built-in terminal.** A real Linux shell in the dashboard (colours, Ctrl-C,
  tab completion, `vim`, `htop`), with buttons for *New invitation*, *Status*,
  *Doctor* and `nvidia-smi`.
- **Secure by construction.** TLS 1.3 mutual authentication with a pinned host
  certificate runs end to end, even through the relay. One Go binary using only
  the standard library: no runtime downloads, no services, no admin rights.

## Quick start (2 minutes)

Download the package for your system from
[**Releases**](https://github.com/Aruneshw/gpu_toolkit/releases/latest) and
extract it (`meshcompute-windows-amd64.zip` or `meshcompute-linux-amd64.tar.gz`).

**You (the host):**

| Linux | Windows |
| --- | --- |
| `./linux.sh` and choose **1** | double-click `win.bat` and choose **1** |

The dashboard opens in your browser and a key is printed (and copied to the
clipboard on Windows). Keep the window open and send the key privately.

**Your friend:**

| Windows | Linux |
| --- | --- |
| double-click `win.bat`, choose **2**, paste the key | `./linux.sh join <KEY>` |

Their laptop appears on your dashboard within seconds. Next time they only need
option **3 (Reconnect)**; the key keeps working across restarts and IP changes.

All menu options:

| | Linux | Windows |
| --- | --- | --- |
| Start the menu | `./linux.sh` | double-click `win.bat` |
| 1. Generate a key: host a group, open the dashboard | `./linux.sh host` | option 1 |
| 2. Join with a friend's key | `./linux.sh join <KEY>` | option 2 (or `.\join.cmd <KEY>`) |
| 3. Reconnect after closing the window | `./linux.sh reconnect` | option 3 |
| 4. Another key for your running group | `./linux.sh invite` | option 4 |
| 5. Install the GPU AI backend (llama.cpp + small model) | see [docs/backend.md](docs/backend.md) | option 5 |

The launchers install or update MeshCompute when needed (the bundled binary, or
the latest release with its SHA-256 checked). Pasting the whole command a friend
sent also works: the key is always the last word. Run from a source checkout,
`linux.sh` rebuilds the binary first.

## How it works

<p align="center"><img alt="Friends' laptops connect outbound through the bore.pub relay to your laptop, which runs the group host and dashboard" src="docs/assets/architecture.svg" width="860"></p>

1. `host` creates a private certificate authority for your group, listens on
   loopback and registers a public port on the relay (saved and reused on
   every restart).
2. An invitation carries that address, the CA and the exact host-certificate
   fingerprint plus a single-use token.
3. `join` makes a key pair on the friend's computer, enrolls over pinned TLS
   and then keeps outbound connections open. The host sends status and
   inference requests down those connections; workers never accept inbound
   traffic and cannot reach each other.
4. The relay only copies encrypted bytes. Use `host --direct` (or
   `MESHCOMPUTE_RELAY=none`) to host on your own IP instead. See
   [Creator-hosted groups](docs/host-first-design.md#connectivity).

## Honest scope

- **VRAM is shown together, not merged.** Totals add up every laptop's GPUs so
  you see the whole group at once, but each prompt runs entirely on **one**
  selected laptop. A model must fit on that laptop's GPU; models are not split
  across machines (llama.cpp RPC is deliberately disabled).
- **Inference needs a backend on the worker.** Telemetry works out of the box;
  running prompts needs `llama-server` and a registered GGUF on that laptop
  (`win.bat` option 5 sets this up on Windows with an NVIDIA GPU).
- **Tested:** unit and race tests; Linux and real-Windows CI that install,
  upgrade, host, generate a key and join through `linux.sh`/`win.bat`; live
  joins over IPv6 and through bore.pub; real CPU GGUF inference. **Not yet
  established:** CUDA inference on a friend's GPU end to end. Details in
  [docs/validation.md](docs/validation.md).

## Requirements

- Linux amd64/arm64 or Windows 10/11 amd64. macOS and AMD/Intel GPUs are not
  validated. NVIDIA GPUs are detected through `nvidia-smi`.
- Outbound internet access on every computer (any network; IPv4 is enough).
- For inference: a compatible `llama-server` and a licensed GGUF model on the
  worker. Nothing is downloaded silently.
- To build from source: Go 1.24 or newer (standard library only, builds
  offline). The release packages need no Go.

## Install

### Manual commands (without the launchers)

The launchers above only wrap these commands:

```sh
# host: relay by default; prints a key and the dashboard link
meshcompute --home ~/.meshcompute-group host --relay bore.pub --ttl 1h --open
meshcompute --home ~/.meshcompute-group pair --ttl 1h      # another key

# friend: enroll with the key and stay connected (reconnects automatically)
meshcompute join <KEY>
meshcompute start                                          # later: reconnect
```

Without a relay, `host --direct --auto` picks this computer's global IPv6 (or a
private IPv4 that needs a router port forward). Each creator hosts an
independent group. See [Creator-hosted groups](docs/host-first-design.md) for
addresses, reconnection, inference setup and network requirements.

### Source and manual installation

Linux (user-scoped, offline build):

```sh
sh installers/install.sh
# binary: ~/.local/bin/meshcompute
```

Windows (non-administrator PowerShell, no Go required):

```powershell
.\installers\install-prebuilt.ps1 -BinaryPath .\meshcompute-windows-amd64.exe -Sha256 <verified-sha256>
# binary: %LOCALAPPDATA%\meshcompute\bin\meshcompute.exe
```

The prebuilt installer verifies SHA-256, stages the file, runs the one-time
`install` step, and records the hash in `install.json`; uninstall verifies that
marker and also needs no Go. See [docs/windows.md](docs/windows.md) for the
release download, firewall, CUDA, enrollment, and rollback details.

To build on Windows with Go instead:

```powershell
.\installers\install.ps1
```

All installers are user-scoped, build or stage the binary, and refuse to replace
an unrelated or symlinked file. They do not install a service, driver, model,
firewall rule, or PATH entry. Uninstall removes only the binary and preserves
configuration/state.

Or build directly:

```sh
make build        # bin/meshcompute
make verify       # formatting, vet, tests, cross-builds, offline build
```

## Advanced: private LAN/VPN cluster

The original direct-peer mode for machines on one private network or VPN.
Coordinator (machine A, e.g. a private VPN IP `100.64.0.10`):

```sh
meshcompute install
meshcompute config set listen 100.64.0.10:7443
meshcompute cluster create
meshcompute start                 # foreground HTTPS agent
```

In another terminal, issue a short-lived, single-use invitation:

```sh
meshcompute pair --ttl 5m
```

Deliver the invitation out of band through an authenticated, confidential
channel. Do not paste it into logs or shell history.

Worker (machine B, e.g. `100.64.0.11`):

```sh
meshcompute install
meshcompute config set listen 100.64.0.11:7443
meshcompute join --stdin          # paste invitation, then Ctrl-D
meshcompute start
```

Back on the coordinator, register the worker's exact endpoint and inspect both
nodes over mutual TLS:

```sh
meshcompute peer add worker1 https://100.64.0.11:7443
meshcompute status
meshcompute monitor --interval 5s
```

`status` and `run --node local` also work on a node against its own loopback
agent. A node accepts only its own leaf certificate or the cluster peer it is
configured to trust; worker-to-worker access is rejected.

Start the localhost dashboard on any enrolled node:

```sh
meshcompute ui
# MeshCompute dashboard: http://127.0.0.1:8081/?token=<one-time printed token>
```

Open the printed URL. The token is exchanged for an HttpOnly, SameSite=Strict
cookie and removed from the address bar. The page shows every configured node's
GPU, VRAM, RAM, models and backend health, and can stream a prompt to one
selected node. See [Honest scope](#honest-scope).

Revoke a worker locally on the coordinator:

```sh
meshcompute revoke worker-<id>
```

Revocation is reloaded on every authenticated request, including new handshakes
on already-open clients. In-flight streams are not cancelled.

## Local model and inference

Build `llama-server` from the pinned upstream commit and register a licensed GGUF
locally. Downloads are never automatic. Full build commands, CUDA notes, and the
verified HTTP contract are in [docs/backend.md](docs/backend.md).

```sh
meshcompute model add my-model /absolute/path/model.gguf
meshcompute model list
meshcompute backend list
meshcompute backend serve my-model --context 2048 --gpu-layers 0 \
  --kv-bytes-per-token VERIFIED_VALUE
```

In a second terminal, on the same node:

```sh
meshcompute run my-model --prompt "Hello" --max-tokens 128 --context 2048
# or target an enrolled worker from the coordinator:
meshcompute run my-model --node worker1 --prompt "Hello"
```

`run --node auto` considers local first, then configured peers, and requires the
selected node to advertise exactly the requested model and enough context. It
never falls back silently after generation starts. `simulate` prints clearly
labeled synthetic scheduling examples and performs no inference.

## Dashboard

`host` starts it automatically; on any enrolled node `meshcompute ui --open`
starts one too. It listens on `127.0.0.1` only and opens with a one-time token.

- **Totals and inventory:** nodes, GPUs, VRAM total/free, RAM, models, and a
  per-GPU table (used, free, utilisation, driver) for every laptop.
- **Invite a friend:** choose an expiry (30 minutes to 24 hours) and click
  *Generate invitation*: copy-ready commands for Windows and Linux plus a live
  countdown.
- **Run inference on one node:** pick a ready laptop and model, stream tokens
  back, stop at any time. Failures are reported and never retried elsewhere.
- **Terminal (Linux hosts):** a real shell on this computer (xterm.js on a
  pseudo-terminal) that survives page reloads, with `meshcompute` on PATH and
  quick buttons for *New invitation*, *Status*, *Models*, *Doctor* and
  `nvidia-smi`. It never opens a shell on a friend's computer.

## Configuration and commands

State lives under the user config directory (`MESHCOMPUTE_HOME` overrides it):

- `config.json` — listen, advertise, backend URL, peers (no secrets)
- `identity.json` — private CA/leaf material owned by the local user (0600)
- `models/` — local model registry entries (paths and SHA-256)

`meshcompute help` lists every command. Useful diagnostics:

```sh
meshcompute doctor              # redacted, shareable output
meshcompute network probe NAME  # authenticated RTT/throughput; no RPC
```

## Security boundaries

Read [docs/security.md](docs/security.md) before deployment. Key points:

- The coordinator keeps the CA private key; workers never receive it.
- Invitations are bearer credentials with 256-bit tokens, hashed at rest,
  single-use, and expire (maximum 24 hours; prefer minutes).
- An invitation also carries the CA and exact coordinator certificate
  fingerprint. Out-of-band authenticity is essential: a substituted invitation
  changes the trust root.
- TLS 1.3 mutual authentication, pinned coordinator leaf, fixed private/loopback
  IP endpoints, proxies and redirects disabled.
- Through the public relay, traffic stays TLS 1.3 with mutual authentication
  and the pinned host certificate; the relay sees only ciphertext and
  connection metadata. Use `--relay` with your own bore server, or `--direct`,
  if that metadata exposure is unacceptable.
- The local filesystem is the administrative authority. There are no remote
  administrative routes and no HTTP enrollment fallback.
- The dashboard binds `127.0.0.1` only, requires a 256-bit token, rejects
  non-loopback Host headers and cross-origin POSTs, and sets a strict CSP. It
  reads telemetry, requests inference, issues invitations (group host only),
  and opens a terminal on the local computer as the local user. Treat the
  dashboard URL/token like a password: it grants that shell.
- No certificate rotation, CRL/OCSP, HA consensus, rate limiting, or audit log
  shipping is built in. Those remain deployment responsibilities.
- Treat GGUF files and backend binaries as untrusted inputs. Never expose
  `llama.cpp` RPC to an open network.

On Windows, privacy depends on the inherited ACLs of the user profile; Unix mode
bits and directory `fsync` are not portable and are substituted accordingly.

## Documentation

- [Architecture decisions](docs/architecture.md)
- [Security model](docs/security.md)
- [llama.cpp backend contract](docs/backend.md)
- [Windows deployment](docs/windows.md)
- [Creator-hosted groups](docs/host-first-design.md)
- [Project overview and rebuild summary](docs/project-report.md)
- [Complete report: architecture, history, failures, architecture v2, all commands](docs/full-project-report.md)
- [Implementation plan](docs/plan.md)
- [Validation evidence](docs/validation.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
