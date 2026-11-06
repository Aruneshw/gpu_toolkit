# ADR 001: a conservative single-binary toolkit

Status: accepted for initial implementation. License: Apache-2.0.

Go is selected over Rust for the standard-library HTTPS, certificate, process,
and cross-compilation support and the available toolchain. Python is not needed
for the first adapter. CLI, agent and dashboard share typed packages in one repository. The dashboard
is an optional loopback-only process inside the same binary; there is no separate
orchestration service and no remote administration plane.

The control plane is HTTPS with pinned cluster CA and mutually authenticated
node certificates. Enrollment uses a short-lived single-use random invitation,
with the CA fingerprint inside the invitation. The coordinator retains the CA
key; workers do not. Only local CLI administrative operations may issue
invitations or revoke identities. Remote peers cannot administer the cluster.
An invitation is a credential: exchange it out of band and protect it.

Loopback is the default listener. For multi-machine use, bind an explicit private
VPN address. Tailscale/WireGuard installation and authentication remain explicit
operator actions; no firewall changes or elevation are performed. VPN encryption
does not cure backend parser vulnerabilities. llama.cpp RPC is experimental and
is not automatically exposed or enabled.

Each node owns its own RAM/VRAM and model files. Initial inference uses a local
llama-server, or sends a bounded authenticated request to a remote node running
its own local llama-server. The adapter is an interface. There is no pretense
that remote single-node inference is model-parallel distributed execution.

The L1 dashboard browsers `GET /v1/status` from the local agent and configured
peers over the existing pinned mTLS identity, sums the returned telemetry for
display, and proxies one `POST /v1/run/stream` request to the selected node.
Tokens are relayed as server-sent events. The browser never holds node
certificates and cannot administer the cluster; the dashboard itself has no
enrollment, revocation, configuration or shell route. Aggregate VRAM is a sum of
independent GPUs, never an addressable pool.
Memory estimates are conservative admission checks, not guarantees; KV cache
requires model architecture information and actual runtime measurements.

Initial targets: Linux amd64/arm64 and Windows amd64, CPU execution; NVIDIA CUDA
depends on separately built compatible llama.cpp, drivers, and hardware. macOS,
AMD and Intel are not validated targets. Cross-compilation is not runtime testing.

Risk boundaries: untrusted GGUF and backend binaries, invitation leakage, cluster
CA compromise, stale capacity reports, GPU fragmentation, unreliable WAN links,
and llama.cpp API changes. Never retry an inference implicitly or change its
placement mid-generation. No arbitrary remote commands, URLs, or model paths.
