# Creator-hosted groups

Each group creator runs that group's host/relay inside MeshCompute. Other
creators run independent groups with their own CA, invitations, and workers.
There is no shared MeshCompute server, central relay, or VPN dependency.

## Quick start

Shortest path: **Linux** `./linux.sh` (choose 1 to host, 2 to join),
**Windows** double-click `win.bat` (1 = host, 2 = join). The manual
commands below are what those menus run.

Download a package from [Releases](https://github.com/Aruneshw/gpu_toolkit/releases).
The Windows ZIP includes the executable; the Linux tarball contains `meshcompute`.

**Creator, Linux:** extract the tarball and run:

```sh
./meshcompute --home "$HOME/.config/meshcompute-group" host --auto
```

`--auto` selects a globally routable IPv6 address when the computer has one,
because such an address needs no router port forwarding. Otherwise it selects
the private IPv4 address and tells you which TCP port to forward. An explicit
`--listen`/`--advertise` pair always overrides the automatic choice.

**Creator, Windows:** extract the ZIP, run `.\install.cmd`, then in a new terminal:

```powershell
meshcompute --home "$env:APPDATA\meshcompute-group" host --listen "[YOUR_PUBLIC_IPV6]:7443" --public
```

For an IPv4 port forward, bind the exact local interface IP and advertise the
forwarded public IP and port:

```sh
./meshcompute --home ./group host --listen 192.168.1.20:7443 \
  --advertise https://YOUR_PUBLIC_IPV4:7443 --public
```

The host prints a private single-use invitation (five-minute expiry) and the
localhost dashboard URL. Send the invitation privately to one friend. For
additional friends, run `meshcompute --home SAME_DIRECTORY pair --ttl 5m` in
another terminal. The creator must stay online with `host` running.

**Friend, Windows:** extract either the release ZIP or GitHub source ZIP and run:

```powershell
.\join.cmd
```

This installs the bundled executable (or downloads the official release when
using the source ZIP), verifies its checksum, and prompts for the invitation.
Paste it and press Enter. No Git, Go, permanent execution-policy change, or
separate networking tool is required. The worker stays in the foreground.
Use the same command again to reconnect an already-enrolled worker: press
Enter to reconnect, or paste a NEW invitation to switch to a new or moved group.
The invitation can also be passed directly, which is what the dashboard's
*Invite a friend* panel generates: `.\join.cmd <INVITATION>`.

**Friend, Linux:** extract the tarball and run:

```sh
./meshcompute join
```

After enrollment, `meshcompute start` also resumes the worker. Use separate
`--home` directories for different groups or for an existing legacy cluster.
`join` with a new invitation replaces an existing **worker** identity only after
the new enrollment succeeds, keeping the old one as
`identity.json.bak-<timestamp>`. A group host or coordinator identity is never
replaced by `join`.

Workers appear automatically in the creator's dashboard under their certificate
IDs (`worker-...`), without `peer add` or knowing a worker's IP address. The
creator can use that ID with `run --node`, `network probe`, and `revoke`.

## What inference needs

Telemetry works without a language model. To run inference, register a local
GGUF and start a compatible llama.cpp backend **on the worker** using
[backend.md](backend.md) / [windows.md](windows.md). Then select that worker
and its model in the dashboard. Models and GPU drivers are hardware-specific
and are not silently downloaded. Each request runs on one computer; memory
is not pooled across computers.

## Connectivity

The creator must have a reachable IP and TCP port. Public IPv6 requires IPv6 on
both ends and a suitable inbound firewall rule on the creator's network. IPv4
requires a public address plus a port forward when behind a router. Carrier
NAT, blocked IPv6, sleeping hosts, and changing IPs can prevent connection.
An invitation establishes trust, not an internet route. MeshCompute does not
claim to bypass these restrictions or automatically change router/firewall rules.

`host --auto` prints the chosen address, whether the friend needs port
forwarding, and the port to allow. `join` now distinguishes the two failure
cases: it first tests TCP reachability of the address in the invitation and
reports "cannot reach the group host" separately from "the host was reachable
but refused enrollment" (expired or reused invitation).
For two processes on one machine, use loopback and omit `--public`.

`listen` and `advertise` addresses are certified. Home and mobile IPv6 prefixes
change often, so `host` handles this automatically: if the saved address is
still on this computer it is reused; otherwise `host --auto` picks the new one,
re-issues the host certificate with the **same group CA** (revocations are
kept), discards old invitations, and prints a NOTICE. Workers that joined
before are pinned to the old certificate; their window says it cannot reach the
host, and they run `join` again with a new invitation from the dashboard.

## Implemented architecture

1. HTTPS enrollment retains single-use invitations, expiration, local worker key
   generation, CA-chain verification, and the exact creator certificate pin.
2. A worker opens outbound TLS 1.3 to the creator's same HTTPS port, authenticates
   with its enrolled certificate, and requests `Upgrade: meshcompute/1`.
3. The creator validates group membership and revocation, then registers the
   connection in its bounded broker. The host sends one reverse HTTP operation
   over each connection; the worker replenishes it automatically. This avoids
   multiplexing complexity and never retries a dispatched inference request.
4. Only the creator's own certificate can use `/v1/workers` and its proxy routes.
   The CLI/dashboard talk to these through local mTLS. Workers cannot route to
   one another, enroll other workers, or use the proxy as an arbitrary HTTP relay.
5. Only status, ping/probe, and inference operations are forwarded. Headers,
   request bodies, response sizes, connection counts (128), group workers (64),
   and timeouts are bounded. TCP provides streaming backpressure. The old frame
   primitives remain tested but are not used by this HTTP transport.
6. A worker's status is cached during an active request with `busy=true`, so the
   single inference connection is not stolen by telemetry. Disconnected known
   workers remain visible as unhealthy until host restart; reconnect repopulates
   discovery after a restart. Resource timestamps indicate measurement age.
7. Failed connections retry with bounded delay and jitter. Idle tunnels renew
   after 30 seconds. Every dispatch rechecks authorization; active sessions
   recheck about once per second and close on revocation or invalid state.
8. Cancellation closes the leased connection and cancels backend generation.
   Shutdown closes listeners, active requests, and the broker. Logs omit keys
   and prompts. Invitations are printed only when explicitly requested.

The localhost token-protected dashboard and backend stay loopback-only. Public
IP support is opt-in for group hosting; legacy private-network defaults remain.

## Validation boundary

End-to-end tests exercise enrollment, outbound TLS, automatic discovery,
dashboard status, SSE and JSON dispatch, reconnection, access isolation, live
revocation, and shutdown with a fixture backend. See `docs/validation.md` for
executed results. Physical Windows CUDA and a two-location internet test require
real target hardware/networks and are not established by CI or Wine.
