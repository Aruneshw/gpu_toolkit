# Validation evidence

Date: 2026-11-06. This file records what was actually executed for this
checkout. It is not a production-readiness or hardware-certification claim.

## Environment

- Linux amd64 host, Go 1.26.8 (module declares Go 1.24).
- NVIDIA RTX 3050 6 GB visible to `nvidia-smi` for resource discovery.
- A prebuilt llama.cpp `b12119` CPU runtime and a licensed LFM3 1.7B
  `Q4_K_M` GGUF were used for real inference. The GPU was not used
  (`--gpu-layers 0`); no CUDA toolkit was installed.
- No second physical host or VPN link was available.
- GitHub Actions also ran the test suite on Ubuntu and Windows runners with Go
  1.24.x and stable (see the CI section below).

## Commands and outcomes

All commands were run from the repository root.

| Command | Outcome |
| --- | --- |
| `gofmt -l .` | no output |
| `go vet ./...` | pass |
| `go test ./... -count=1` | pass (105 test cases including subtests) |
| `go test -race ./... -count=1` | pass |
| `GOPROXY=off GOTOOLCHAIN=local go build ./...` | pass |
| `go test ./... -count=1` with `GOPROXY=off` | pass |
| `GOOS=linux GOARCH=amd64/arm64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go build ./cmd/meshcompute` | pass |
| `GOOS=windows GOARCH=amd64 go vet ./...` | pass |
| `GOOS=windows GOARCH=amd64 go test -c ./internal/resource` | pass (Windows test binary compiled) |
| `make dist` | pass; Linux amd64/arm64 and Windows amd64 plus `SHA256SUMS` |
| `sh -n installers/install.sh installers/uninstall.sh` | pass |
| Linux installer: install, `version`, uninstall | pass; binary removed, state preserved |
| Windows binary under Wine: `version`, `install`, `doctor`, `config set`, `cluster create`, `start`, `status` | pass; CPU-only, no PowerShell in Wine |

The repeated TLS dispatch test was also run 20 times to check the oversized-body
path:

```sh
go test ./internal/agent -run TestTwoAgentTLSDispatchFixture -count=20
```

Result: pass.

The streaming and dashboard paths were also stress-run:

```sh
go test -race ./internal/backend ./internal/agent ./internal/dashboard ./internal/sse ./internal/cli -count=3
go test -race ./internal/agent -run TestTwoAgentTLSStreamFixture -count=10
```

Result: pass.

## Continuous integration

GitHub Actions run
[42258567595](https://github.com/Aruneshw/gpu_toolkit/actions/runs/42258567595)
completed successfully on the Windows-compatibility commit. All nine jobs
passed:

- test: Ubuntu and Windows, each on Go 1.24.x and stable; race detector on Linux
- offline build and tests with `GOPROXY=off` and `GOTOOLCHAIN=local`
- cross-compile: Linux amd64/arm64 and Windows amd64
- Linux installer smoke: install, run, uninstall
- Windows: PowerShell installer syntax parse for source and prebuilt installers
- Windows prebuilt installer: install, `version`, `install.json` checksum,
  tamper refusal, forced removal, reinstall, and clean uninstall verified
  through the marker rather than Go build info
- Windows Go tests now exercise the real PowerShell/CIM discovery path for CPU,
  RAM, and disk on the runner

The earlier run of the dashboard series exposed a Windows-Go-1.24-only timing
failure; finding 5 below records the fix. The Windows-compatibility series
exposed the elevation and WMI cold-start issues recorded as findings 6 and 7.

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
  cancellation, response limits, streaming chunk parsing, missing/invalid final
  chunks, emit-failure propagation, and exact launch arguments with ambient
  `LLAMA_*`/`GGML_RPC*` removal. The automated fixture is an HTTP server and a
  helper subprocess, not a language model.
- **Agent:** real TLS enrollment and dispatch between two state directories on
  loopback for both JSON and SSE, participant attribution, busy rejection,
  missing-model rejection, prompt non-leakage into audit output,
  malformed/trailing/oversized request rejection, stream error events, slot
  release after success/failure, and revocation.
- **Dashboard and SSE:** token bootstrap and cookie, constant-time token checks,
  non-loopback Host and cross-origin POST rejection, security headers, aggregate
  RAM/VRAM/model arithmetic, node status failures, run dispatch fields, strict
  request rejection, streamed token/done/error relay, CRLF/comments, truncated
  streams, and size limits.
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
4. Real CPU inference on the Linux host: llama.cpp `b12119`, LFM3 1.7B
   `Q4_K_M`, context 2048, `--gpu-layers 0`. `meshcompute run` returned
   `"Paris, and the population of France is about 67 million..."` at
   6.78 tok/s.
5. Real dashboard streaming on the same stack: `meshcompute ui` printed a
   64-hex-character token URL; requests without a token, with a non-loopback
   Host, or with a bad origin returned 403. `GET /api/status` reported one
   healthy node, one GPU, 6,442,450,944 bytes aggregate VRAM, and the explicit
   “not pooled” scope text. `POST /api/run` streamed 16 `event: token` frames
   followed by `event: done` at 9.80 tok/s.

These use one host and loopback, not a physical two-machine VPN. The GPU was
visible but not used for inference.

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
   suite now runs on Windows CI, and the prebuilt installer is executed there;
   the source-build `install.ps1` is still syntax-checked only.
4. **Go 1.26 test temporaries.** `testing.T.TempDir()` now creates per-test
   directories as 0775 under the default umask, which the security package
   correctly rejects. Agent tests now create a private child directory, matching
   the security tests.
5. **Coarse probe clock on Windows Go 1.24.** A single 256 KiB loopback probe
   measured as zero, so throughput stayed zero despite a successful transfer.
   The probe now repeats a bounded number of times and divides total bytes by
   aggregate elapsed time. Windows Go 1.24 CI passes after the change.
6. **Tailscale/CGNAT identity mismatch.** `config` accepted RFC 6598
   `100.64.0.0/10` listen addresses, but the TLS identity layer's private-IP
   check used `net.IP.IsPrivate` and rejected them. `privateIP` now accepts the
   CGNAT range explicitly, with `TestPrivateIP` covering accepted and rejected
   boundaries.
7. **PowerShell discovery encoding.** Redirected Windows PowerShell output can
   be UTF-8 with a BOM, UTF-16 LE/BE with a BOM, or unmarked UTF-16LE. JSON
   decoding now normalizes those forms, and the discovery tests cover them.
8. **CI elevation and WMI cold start.** GitHub's Windows runners are elevated,
   so the prebuilt installer's deliberate admin refusal blocked the smoke test;
   an explicit `-AllowElevated` switch is passed only by CI, and the default
   still refuses. A 5-second discovery timeout also killed the first
   `Get-CimInstance` call on a cold runner; hardware probing now allows 20
   seconds and reports timeouts/stderr instead of a bare exit status.

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

- CUDA execution, GPU layer offload, or measured VRAM behavior. CPU-only
  inference was executed; the RTX 3050 was detected but unused.
- Real inference over a remote node or WAN link.
- A physical Windows machine, Windows GPU/CUDA execution, or Windows firewall
  behavior. CI runs the Windows Go tests and the prebuilt installer; Wine runs
  the Windows binary for CLI lifecycle checks. Neither is GPU or physical-host
  validation, and the source-build `install.ps1` is not executed in CI.
- A physical two-machine VPN deployment, firewall behavior, or WAN reliability.
- macOS, AMD, or Intel GPU targets.
- Any distributed/model-parallel inference. RPC remains disabled and unclaimed.
