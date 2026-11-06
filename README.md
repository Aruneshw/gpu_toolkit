# MeshCompute

MeshCompute is a conservative, single-binary tool for running authenticated
inference on separate devices that each own their models. It is not a
model-parallel distributed runtime: a request is executed by one local
`llama.cpp` server on one node, chosen explicitly or by bounded admission.

The CLI, HTTPS agent and localhost dashboard share one repository and one state
directory. There is no remote administration, no model download, and no
automatic retry or mid-generation placement change. The dashboard aggregates
telemetry and streams tokens from one selected node; it does not create a
shared VRAM pool.

## Status and scope

- Implemented and unit-tested: configuration, hardware discovery, TLS
  enrollment, single-use invitations, mutual authentication, revocation,
  model registry, llama.cpp HTTP adapter (including SSE streaming),
  memory/network admission, scheduler simulation, CLI, token-protected
  localhost dashboard, Linux/Windows installers, and CI.
- Verified on this machine: `go test ./...`, `go test -race ./...`, offline
  builds, Linux/Windows/arm64 cross-builds, two-home TLS enrollment/status/
  revocation, the Linux installer, real CPU-only GGUF inference, and
  end-to-end dashboard token streaming.
- **Not verified here:** CUDA/GPU execution, a physical two-machine VPN,
  Windows deployment beyond CI tests, or llama.cpp RPC. Cross-compiling is not
  runtime testing. See [docs/validation.md](docs/validation.md).

RPC is deliberately not built, launched, discovered, or claimed. Sending work to
one authenticated remote node that runs its own local model is remote
single-node inference, not tensor splitting.

## Requirements

- Go 1.24 or newer to build from source (standard library only; no modules to
  download). The prebuilt Windows installer does **not** require Go.
- A separately built and compatible `llama-server` for real inference.
- Linux amd64/arm64 or Windows amd64. macOS and AMD/Intel GPUs are not validated.
- For multi-machine use: a reachable creator-host address (public IP is opt-in),
  or a private LAN/VPN for legacy direct peers. MeshCompute does not change
  firewalls or elevate.

## Install

### Ready-to-run packages and creator-hosted groups

Download the Windows ZIP or Linux tarball from
[Releases](https://github.com/Aruneshw/gpu_toolkit/releases). On Windows, extract
the ZIP and run `.\join.cmd` in ordinary PowerShell. It installs, then prompts
for your friend's invitation; Git and Go are not needed. The GitHub source ZIP
also contains `join.cmd`, which fetches the published executable automatically.

The person inviting friends runs their own group host:

```sh
meshcompute --home ./my-group host --listen '[YOUR_PUBLIC_IPV6]:7443' --public
```

The host prints an invitation and its local dashboard link. Each creator hosts
an independent group; workers connect outbound and appear automatically.
The creator's IP/port must be reachable from the friends' networks. See
[Creator-hosted groups](docs/host-first-design.md) for IPv4 forwarding, Linux,
Windows, reconnection, inference setup, and network requirements.

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

## Quick start

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
selected node. See [Dashboard scope](#dashboard-scope).

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

## Dashboard scope

`meshcompute ui` is a control panel, not a distributed GPU runtime:

- Telemetry from healthy nodes is summed for display only. “VRAM total” is the
  sum of each GPU's memory, not one addressable pool.
- A prompt is dispatched to exactly one selected node. The model must already
  be registered and loaded there; nothing is sharded or migrated mid-generation.
- Tokens stream to the browser over server-sent events. Stopping the browser
  request cancels that generation; failures are reported and never retried.
- The server binds loopback only, requires a random 256-bit token, validates
  Host and Origin, and serves a strict CSP. There is no remote administration.

True cross-device model splitting is still not implemented; llama.cpp RPC
remains disabled.

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
- The local filesystem is the administrative authority. There are no remote
  administrative routes and no HTTP enrollment fallback.
- The dashboard binds `127.0.0.1` only, requires a 256-bit token, rejects
  non-loopback Host headers and cross-origin POSTs, and sets a strict CSP. It
  can only read telemetry and request inference; it cannot enroll, revoke,
  change configuration, or reach a shell.
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
- [Implementation plan](docs/plan.md)
- [Validation evidence](docs/validation.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
