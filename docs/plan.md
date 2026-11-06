# Implementation and validation plan

1. Establish architecture, package contracts and compatibility boundaries.
2. Implement CLI/configuration, hardware discovery, local agent and diagnostics;
   unit-test parsing, resource handling and persistence.
3. Implement TLS enrollment, single-use invitations, identity allowlist and
   revocation; exercise two agents locally with real HTTPS and certificates.
4. Integrate the verified llama.cpp HTTP API, local model registration and
   bounded inference. Test the HTTP contract with fixtures. Real CPU GGUF
   inference was executed on the development host; GPU and two-host execution
   remain distinct hardware-gated checks. Investigate RPC without claiming
   unverified participation.
5. Add memory/network admission and scheduling with explicit simulation tests,
   failed-worker handling, and no transparent generation retries.
6. Package user-scoped Linux/Windows installation, offline builds, CI, security
   guidance and reproducible verification commands.
7. Add the L1 localhost dashboard: token-protected, loopback-only aggregate
   telemetry and one-node SSE streaming. Defer true cross-device sharding and
   llama.cpp RPC; aggregate VRAM is not a shared pool.

Completion evidence is recorded in docs/validation.md. Planned work is not a
claim of passing tests or production readiness.
