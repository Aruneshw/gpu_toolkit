# Validation evidence

Date: 2026-11-06. This file records what was actually executed for this
checkout. It is not a production-readiness or hardware-certification claim.

## Environment

- Linux amd64 host, Go 1.26.8 (module declares Go 1.24).
- NVIDIA RTX 3050 6 GB visible to `nvidia-smi` for resource discovery.
- No `llama-server` binary and no licensed GGUF model were present, so no real
  inference was executed.
- No second physical host or VPN link was available.
- GitHub Actions also ran the test suite on Ubuntu and Windows runners with Go
  1.24.x and stable (see the CI section below).

## Commands and outcomes

All commands were run from the repository root.

| Command | Outcome |
| --- | --- |
| `gofmt -l .` | no output |
| `go vet ./...` | pass |
| `go test ./... -count=1` | pass (75 test cases including subtests) |
| `go test -race ./... -count=1` | pass |
| `GOPROXY=off GOTOOLCHAIN=local go build ./...` | pass |
| `go test ./... -count=1` with `GOPROXY=off` | pass |
| `GOOS=linux GOARCH=amd64/arm64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `sh -n installers/install.sh installers/uninstall.sh` | pass |
| Linux installer: install, `version`, uninstall | pass; binary removed, state preserved |

The repeated TLS dispatch test was also run 20 times to check the oversized-body
path:

```sh
go test ./internal/agent -run TestTwoAgentTLSDispatchFixture -count=20
```

Result: pass.

## Continuous integration

GitHub Actions run
[42248720737](https://github.com/Aruneshw/gpu_toolkit/actions/runs/42248720737)
completed successfully. All eight jobs passed:

- test: Ubuntu and Windows, each on Go 1.24.x and stable; race detector on Linux
- offline build and tests with `GOPROXY=off` and `GOTOOLCHAIN=local`
- cross-compile: Linux amd64/arm64 and Windows amd64
- Linux installer smoke: install, run, uninstall
- Windows: PowerShell installer syntax parse

The earlier run of the same commit series exposed a Windows-Go-1.24-only timing
failure; finding 5 below records the fix that made CI green.

## What the automated tests exercise

- **Security and transport:** coordinator initialization, real HTTPS enrollment,
  single-use invitations under concurrent processes, replay rejection, expired
  and malformed invitations, CA and leaf pinning, foreign-cluster rejection,
  same-CA non-coordinator rejection, worker-to-worker rejection, revocation
  reload on each handshake, fail-closed on corrupt/unsafe state, symlink
  rejection, and local self-access for both roles.
- **Backend and registry:** GGUF magic and full-file SHA-256 registration,
  duplicate/mutation rejection, KV arithmetic and overflow, loopback endpoint
  restrictions, health/model/context checks, tokenizer budget enforcement,
  cancellation, response limits, and exact launch arguments with ambient
  `LLAMA_*`/`GGML_RPC*` removal. The test backend is an HTTP fixture and a
  helper subprocess, not a language model.
- **Agent:** real TLS enrollment and dispatch between two state directories on
  loopback, participant attribution, busy rejection, missing-model rejection,
  prompt non-leakage into audit output, malformed/trailing/oversized request
  rejection, and revocation.
- **Resources, scheduler, network, CLI, config:** meminfo/GPU CSV parsing with
  unknown values left unknown, cancelled discovery, disk probing, synthetic-only
  scheduling simulations, stale-worker/unstable-network rejection, explicit
  non-pooling of RAM and VRAM, probe measurement and failure handling, CLI
  parsing, offline install, and strict configuration loading.

## Manual integration smoke tests

1. CLI lifecycle in a temporary home: `install`, `version`, `simulate`,
   `doctor`, `model add/list` with a fixture GGUF, `cluster create`, `pair`,
   and `status`. Expected JSON and file modes (0700 state, 0600 config/identity)
   were observed.
2. Two independent state directories and two agent processes on loopback:
   coordinator `cluster create`, `pair`, worker `join`, both agents started,
   `peer add`, coordinator `status` (both nodes healthy), worker self `status`,
   `network probe local`, then `revoke`. After revocation the worker row became
   unhealthy and authenticated access failed.
3. Local coordinator `start` followed by self `status`: healthy, proving the
   self-identity path. The Linux installer built offline, ran, and uninstalled
   cleanly.

These use one host and loopback, not a physical two-machine VPN.

## Integration review findings fixed during completion

1. **Local CLI could not reach its own agent.** The client authorized only the
   opposite cluster role, so `status`/`run --node local` failed on both
   coordinator and worker. `Client`, `ServerTLS`, and `Authorize` now also accept
   the node's own leaf certificate, still with CA-chain verification. Added
   `TestLocalSelfAccess`.
2. **Oversized streaming request produced a connection reset** instead of the
   documented 400 response. The agent now reads the bounded body before
   decoding and drains a bounded remainder before replying. The dispatch test
   passes repeatedly.
3. **Windows portability.** Unconditional Unix mode-bit checks and directory
   `fsync` made identity creation fail on Windows. These are now platform
   helpers: Unix keeps the 0700/0600 and fsync requirements; Windows relies on
   inherited user-profile ACLs and a file flush before rename. The full test
   suite now runs on Windows CI; the interactive installer was not executed
   there.
4. **Go 1.26 test temporaries.** `testing.T.TempDir()` now creates per-test
   directories as 0775 under the default umask, which the security package
   correctly rejects. Agent tests now create a private child directory, matching
   the security tests.
5. **Coarse probe clock on Windows Go 1.24.** A single 256 KiB loopback probe
   measured as zero, so throughput stayed zero despite a successful transfer.
   The probe now repeats a bounded number of times and divides total bytes by
   aggregate elapsed time. Windows Go 1.24 CI passes after the change.

## Security review notes retained as limitations

- There is no rate limiting, audit-log shipping, certificate rotation/renewal,
  CRL/OCSP, HA consensus, or remote revocation API. Those are deployment
  responsibilities.
- A crashed writer can leave `.lock`; locks are never stolen automatically.
- Revocation does not cancel an already-authorized in-flight request or stream.
- Exact coordinator pinning means replacing the coordinator certificate requires
  explicit worker reprovisioning.
- The scheduler is a planning/admission check only. It does not reserve memory,
  pool RAM and VRAM, or execute RPC. KV estimates are architecture-specific and
  must be supplied from verified metadata.
- A peer named `local` is currently accepted by configuration but shadows the
  built-in local endpoint in `peer` lookups. Use a different peer name.
- The loopback network probe can report throughput below the default 50 MB/s
  threshold on a busy host; this only annotates a recommendation and does not
  enable RPC or change placement mid-generation.

## Not validated

- Real GGUF loading or token generation against `llama-server`.
- CUDA execution, GPU layer offload, or measured VRAM behavior.
- Windows deployment beyond unit/integration tests, and PowerShell installer
  execution. CI runs the Go tests on Windows but only parses the scripts; it
  does not run them as a non-administrator.
- A physical two-machine VPN deployment, firewall behavior, or WAN reliability.
- macOS, AMD, or Intel GPU targets.
- Any distributed/model-parallel inference. RPC remains disabled and unclaimed.
