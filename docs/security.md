# Cluster identity and trust

`internal/security` uses only the Go standard library. Initialize a coordinator
with `Init(dir, host)` where host is an explicit private/loopback IP (not a DNS
name or host:port). Its TLS certificate contains that IP SAN. Mount
`EnrollHandler(dir)` at `/enroll` and wrap **every other route** with
`Authorize(dir, handler)`. Set the HTTPS server TLSConfig from `ServerTLS(dir)`;
do not override its certificate or client-auth settings. Set server request/header
timeouts and connection/rate limits at the application layer. Never expose an
HTTP enrollment fallback.

## API

```go
func Init(dir, host string) error
func Pair(dir, endpoint string, ttl time.Duration) (string, error)
func Join(ctx context.Context, dir, code string) error
func JoinHost(ctx context.Context, dir, code, host string) error
func ServerTLS(dir string) (*tls.Config, error)
func Client(dir string) (*http.Client, error)
func EnrollHandler(dir string) http.Handler
func Authorize(dir string, next http.Handler) http.Handler
func Revoke(dir, id string) error
func NodeID(dir string) (string, error)
```

`Init`, `Pair`, and `Revoke` are local-only administration: possession of the
private coordinator filesystem is the administrative authority. There are no
remote administrative handlers. Pair accepts an HTTPS IP origin, optional port,
and a positive lifetime of at most 24 hours; prefer minutes. Deliver its opaque
invitation through an authenticated confidential channel. It contains a bearer
token, CA, exact coordinator certificate fingerprint, and endpoint. Whoever
obtains an unused invitation can enroll. Do not log invitations or put them in
shell history. A substituted invitation changes the trust root, so out-of-band
authenticity is essential.

Join generates the worker private key locally, verifies HTTPS with the invitation
CA, IP SAN, and exact leaf fingerprint, then sends a signed Ed25519 CSR. The
coordinator ignores requested names/extensions and assigns a random worker ID
and, by default, a client-auth-only certificate. The CA private key never leaves the coordinator.
Tokens have 256 bits of randomness; only SHA-256 hashes and expiry times are
persisted. Enrollment bodies and invitations are bounded to 64 KiB.

Workers use `Client` for subsequent mTLS calls. It accepts only HTTPS private or
loopback IP destinations, disables environment proxies and redirects, verifies
normal certificate validity and SANs, and pins the exact coordinator leaf. Even
another server certificate signed by the same CA is rejected. Workers cannot
perform administration with their identity. Plain Join does not enable a worker
listener. To enable coordinator-to-worker RPC explicitly, call JoinHost with the
worker's configured listen IP (extract the host from host:port); it adds that IP
SAN and server-auth usage. ServerTLS then requires a client certificate and pins
the coordinator; Authorize on the worker also accepts only that coordinator.
Worker-to-worker RPC is always rejected. Client on the coordinator accepts only
CA-verified, non-revoked worker servers with matching IP SANs. Each node also
accepts its own leaf certificate on loopback so local `status`/`run` can reach
its own agent; this grants no remote privilege and does not bypass peer
revocation. It disables
connection reuse so every outbound request rechecks disk revocations during a
fresh TLS handshake. This costs an extra handshake per request. The enrollment
host is an explicit claim by the invitation holder, not proof of IP ownership;
application routing must bind worker IDs to intended destinations where needed.

TLS is 1.3 minimum. The enrollment listener requests/verifies client certificates
when present but must allow their absence for enrollment. **ServerTLS alone does
not authorize application routes.** Authorize re-verifies client certificate
validity, worker role and CA chain and reloads revocations from disk for each
request, including requests on already-open TLS connections. Missing, corrupt,
or insecure state fails closed. Revocation does not cancel already-authorized
in-flight requests or streams.

## Localhost dashboard

`meshcompute ui` starts a separate HTTP server bound to an explicit loopback IP.
It is not part of the mTLS cluster plane: the browser cannot present a client
certificate, so the dashboard authenticates locally and then uses `Client(dir)`
for every agent call. It exposes only `GET /`, `GET /api/status` and
`POST /api/run`.

- A random 256-bit token is generated per start and printed once in the launch
  URL. It is never written to configuration. Restarting the dashboard invalidates
  old links. The first valid `?token=` request is exchanged for an HttpOnly,
  SameSite=Strict cookie and redirected to `/`, so the token is removed from the
  address bar.
- Every route requires the token (cookie, `X-MeshCompute-Token`, or query). Host
  must be a loopback name/IP with the configured port, which blocks DNS
  rebinding. Cross-origin POSTs are rejected from a fixed loopback origin
  allowlist; non-browser clients still need the token.
- Responses set `Content-Security-Policy` with no external origins, plus
  `nosniff`, `DENY` framing and no-referrer. Request bodies and streams are
  bounded. The run proxy has a 30-minute hard stream limit and per-write
  deadlines; it never retries or changes placement.
- The dashboard can read aggregate telemetry and request inference on an
  existing node. It cannot enroll, revoke, change configuration, read model
  files, or run commands. It is not a sandbox against the same OS user,
  malicious browser extensions, or a process that can read the launch terminal;
  localhost is not an authorization boundary by itself.

## Persistence and operational limits

Use a local POSIX filesystem supporting 0700 directories, 0600 files, atomic
rename, exclusive directory creation, and file/directory fsync. State is a single
private `identity.json` file replaced atomically after fsync. Local CLI and server
writers share an exclusive `.lock` directory with a five-second acquisition
timeout. Successful consumption is persisted before an enrollment response is
sent. This gives at-most-once enrollment: a network failure or worker crash can
burn an invitation without delivering a usable identity; issue a new invitation.

A crashed writer can leave `.lock`. Stop all writers and verify none remain
before manually removing it; locks are never automatically stolen. Paths and
their ancestors must be controlled by the service administrator. Final directory
and state symlinks and group/world-accessible permissions are rejected; this is
not a sandbox against root, the same OS user, malicious ancestor replacement, or
filesystem rollback. On Windows, Unix mode bits and directory `fsync` are not
portable: privacy relies on the inherited ACLs of the user profile, while file
contents are still flushed before the atomic rename. Do not use shared/untrusted or permission-emulating storage.
Protect coordinator backups as CA signing authority; restoring stale state can
restore consumed tokens or undo revocations.

CA lifetime is five years and leaf lifetime one year. There is no automatic
certificate rotation, renewal, CA migration, HA consensus, CRL/OCSP distribution,
or remote revocation API. Exact certificate pinning means coordinator certificate
replacement requires explicit worker reprovisioning. State is bounded to 8 MiB;
expired tokens are pruned when issuing invitations, while revocations are kept.
Rate limiting, audit logging without secrets, secure invitation delivery, clock
synchronization, and backup/rotation procedures are deployment responsibilities.
