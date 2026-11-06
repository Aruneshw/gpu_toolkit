# MeshCompute

MeshCompute is a conservative, single-binary tool for running authenticated
inference on separate devices that each own their models. It is not a
model-parallel distributed runtime: a request is executed by one local
`llama.cpp` server on one node, chosen explicitly or by bounded admission.

The CLI and HTTPS agent share one repository and one state directory. There is
no dashboard, no remote administration, no model download, and no automatic
retry or mid-generation placement change.

## Status and scope

- Implemented and unit-tested: configuration, hardware discovery, TLS
  enrollment, single-use invitations, mutual authentication, revocation,
  model registry, llama.cpp HTTP adapter, memory/network admission, scheduler
  simulation, CLI, Linux/Windows installers, and CI.
- Verified on this machine: `go test ./...`, `go test -race ./...`, offline
  builds, Linux/Windows/arm64 cross-builds, two-home TLS enrollment/status/
  revocation, and the Linux installer.
- **Not verified here:** real GGUF inference, CUDA/GPU execution, a physical
  two-machine VPN, Windows runtime behavior, or llama.cpp RPC. Cross-compiling
  is not runtime testing. See [docs/validation.md](docs/validation.md).

RPC is deliberately not built, launched, discovered, or claimed. Sending work to
one authenticated remote node that runs its own local model is remote
single-node inference, not tensor splitting.

## Requirements

- Go 1.24 or newer to build from source (standard library only; no modules to
  download).
- A separately built and compatible `llama-server` for real inference.
- Linux amd64/arm64 or Windows amd64. macOS and AMD/Intel GPUs are not validated.
- For multi-machine use: an explicit private VPN address (Tailscale/WireGuard)
  configured by the operator. MeshCompute does not change firewalls or elevate.

## Install

Linux (user-scoped, offline build):

```sh
sh installers/install.sh
# binary: ~/.local/bin/meshcompute
```

Windows (non-administrator PowerShell):

```powershell
.\installers\install.ps1
# binary: %LOCALAPPDATA%\meshcompute\bin\meshcompute.exe
```

Both installers build with `GOPROXY=off`, `GOTOOLCHAIN=local`, `CGO_ENABLED=0`,
stage the binary, and refuse to replace an unrelated or symlinked file. They do
not install a service, driver, model, firewall rule, or PATH entry. Uninstall
removes only the binary and preserves configuration/state.

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
- [Implementation plan](docs/plan.md)
- [Validation evidence](docs/validation.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
