# Host-first remote networking: proposed design

This document is a design, not a description of implemented functionality.
The existing MeshCompute release still requires directly reachable private
addresses for both coordinator and worker. The `internal/tunnel` package
contains framing primitives only and is not connected to the agent.

## Intended user flow

1. A host explicitly enables remote hosting, chooses an address and starts the
   agent. The host must be reachable by each worker over IPv6 or an explicitly
   forwarded port. A connection test can detect failure; it cannot guarantee
   that all future clients can reach the host.
2. The host generates an expiring, single-use invitation containing its exact
   endpoint and pinned cluster identity. The user transfers it through an
   authenticated confidential channel.
3. A worker joins using the invitation, generates its private key locally and
   receives an enrolled identity. It then maintains an outbound connection to
   the host. The worker need not expose an inbound listener to the internet.
4. The host lists connected worker identities and sends status and inference
   requests to one selected worker over that connection. Disconnection is
   visible and in-flight inference fails without silent retry.
5. The host can revoke a worker locally. Revocation must terminate its active
   connection and reject subsequent connections and requests.

Each host retains its own CA and is a separate trust domain. There is no
shared service or automatic discovery of other people's clusters. A host being
offline makes its group unavailable. An IPv6 address, NAT mapping, firewall
policy, or routing change may break reachability; automatic recovery cannot
make unreachable networks reachable.

## Transport requirements

- Require TLS 1.3 and validate both certificate chain and exact host pin; the
  worker must authenticate using its enrolled identity. Enrollment must retain
  single-use invitation, expiration, and certificate verification guarantees.
- Do not weaken the existing private-IP checks globally. Public listening and
  dialing must be explicit opt-ins with separately validated literal addresses.
  The dashboard and inference backend stay loopback-only.
- Authenticate before accepting tunnel frames. Enforce per-host limits on
  connection count, enrollment attempts, concurrent streams, frame size, total
  buffered bytes, idle time, and connection establishment time. The framing
  helper by itself does not provide any of these connection-level protections.
- Specify stream open, data, end, reset, ping and shutdown messages, stream ID
  allocation, flow control, and maximum request and response sizes before
  wiring up the existing status and streaming APIs. Reject malformed frames and
  duplicate or unexpected stream IDs. Slow peers must not cause unbounded
  memory growth or block unrelated streams.
- Bind each connection and request to the authenticated worker identity, not a
  worker-supplied name. Prevent cross-group routing and worker-to-worker access.
  On replacement or reconnect, prevent an older connection from removing or
  impersonating the new one. Limit reconnect frequency and add jitter.
- Keep prompt bodies and invitation codes out of logs. Cancellation, disconnect,
  revocation, and process shutdown must release streams and backend slots.
- Protect enrollment and tunnel handshakes from unauthenticated resource
  exhaustion. Public exposure also requires operational firewall policy,
  security review and abuse controls; mTLS does not eliminate denial of service.

## Required validation before claiming remote support

Test real coordinator/worker enrollment, status and SSE inference across the
reverse transport on Linux and Windows, including simultaneous independent
groups. Cover invalid invitations, expiration, replay, foreign identities,
revocation during a live connection, malformed frames, backpressure, bounded
memory, disconnect and reconnect, duplicate connections, cancellation, and
shutdown. Run physical two-machine tests on different networks and verify
firewall and IPv6 behavior. A loopback test or Wine is not evidence that an
arbitrary internet connection works.
