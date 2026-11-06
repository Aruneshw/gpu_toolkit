# Windows deployment

This guide covers Windows 11 x64 as a MeshCompute coordinator or worker. The
prebuilt path needs neither Go nor administrator rights for installation. The
toolkit still does not pool VRAM: a request runs on one node's local
`llama-server`, and the dashboard only aggregates telemetry.

## Status

- CI builds and tests the Go code on Windows, and executes the prebuilt
  install/uninstall path in PowerShell.
- The prebuilt installer is designed for PowerShell 5.1 and does not require a
  Go toolchain on the worker.
- CUDA/GPU execution, a physical Windows-to-Linux VPN deployment, and firewall
  behavior have **not** been validated on hardware here. Verify them on the
  target machine; do not treat CI as GPU evidence.

## Install without Go

Download the release asset `meshcompute-windows-amd64.exe` and `SHA256SUMS` from
the repository releases page, then verify the hash before running it:

```powershell
Get-FileHash .\meshcompute-windows-amd64.exe -Algorithm SHA256
```

Install from a local file. The optional `-Sha256` makes the script fail closed
on a mismatch:

```powershell
.\installers\install-prebuilt.ps1 `
  -BinaryPath .\meshcompute-windows-amd64.exe `
  -Sha256 <64-hex-character-sha256>
```

Or download over HTTPS (a checksum is mandatory for `-Url`):

```powershell
.\installers\install-prebuilt.ps1 `
  -Url https://github.com/Aruneshw/gpu_toolkit/releases/download/<tag>/meshcompute-windows-amd64.exe `
  -Sha256 <64-hex-character-sha256>
```

The installer:

- refuses administrator elevation, non-regular files, symlinks/reparse points,
  and non-`MZ` executables;
- stages a copy, checks SHA-256, runs `meshcompute install` once, then moves the
  binary into place;
- keeps the previous binary as `meshcompute.exe.previous` and refuses to
  overwrite an unreviewed backup;
- writes `%LOCALAPPDATA%\meshcompute\bin\install.json` with the verified hash.

It does **not** modify `PATH`, install a service, open a firewall port, download
a model, or install a GPU driver. Add the directory for the current session:

```powershell
$env:PATH = "$env:LOCALAPPDATA\meshcompute\bin;$env:PATH"
meshcompute version
```

To make that permanent, add `%LOCALAPPDATA%\meshcompute\bin` to the user `Path`
in System Properties or with `setx`; no elevation is required.

### Upgrade and uninstall

Re-run `install-prebuilt.ps1` to upgrade. It moves the current executable to
`meshcompute.exe.previous`, so remove that reviewed backup before a second
upgrade.

Uninstall verifies the executable against `install.json` and needs no Go:

```powershell
.\installers\uninstall.ps1
```

Only the binary and marker are removed. Configuration, identity, model
registry, and the `.previous` backup are preserved. If the binary was modified,
uninstall refuses; inspect it, then use `-Force` as an explicit override.

Source builds still work with `installers\install.ps1`, which requires Go 1.24+.

## Network identity and firewall

Choose the listening address **before** `cluster create` or `join`; it is part
of the certificate identity. Use the exact private LAN or Tailscale address,
never `0.0.0.0`:

```powershell
meshcompute install
meshcompute config set listen 100.64.0.10:7443
meshcompute cluster create
```

Both RFC 1918 LAN addresses and RFC 6598/Tailscale `100.64.0.0/10` addresses are
accepted by configuration and by the TLS identity layer.

When the agent first binds a non-loopback address, Windows Defender Firewall
normally prompts. Allow it on **Private** networks only. If the prompt does not
appear, an administrator may add an explicit rule for the chosen port:

```powershell
New-NetFirewallRule -DisplayName "MeshCompute agent" `
  -Direction Inbound -Protocol TCP -LocalPort 7443 `
  -Action Allow -Profile Private
```

MeshCompute never changes firewall rules or elevates itself. Tailscale or
WireGuard must be installed and configured separately; do not expose the agent
port directly to the public internet.

Coordinator:

```powershell
meshcompute start
```

In a second terminal, issue a short-lived, single-use invitation:

```powershell
meshcompute pair --ttl 5m
```

Worker:

```powershell
meshcompute install
meshcompute config set listen 100.64.0.11:7443
meshcompute join --stdin
meshcompute start
```

Back on the coordinator:

```powershell
meshcompute peer add worker1 https://100.64.0.11:7443
meshcompute status
```

Deliver invitations only through an authenticated, confidential channel. Treat
them as bearer credentials.

## llama.cpp and NVIDIA

Build `llama-server.exe` from the pinned commit in
[docs/backend.md](backend.md) with an x64 Visual Studio Developer PowerShell:

```powershell
cmake -S . -B build-cuda -A x64 -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=ON -DGGML_RPC=OFF
cmake --build build-cuda --config Release --target llama-server
```

Keep the CUDA runtime DLLs next to `llama-server.exe` or on `PATH`. Register a
licensed local GGUF explicitly; MeshCompute never downloads a model:

```powershell
meshcompute model add my-model "D:\models\my-model-Q4_K_M.gguf"
meshcompute model list
```

Launch the backend in a dedicated terminal. Either put `llama-server.exe` on
`PATH` or pass `--binary`. `--kv-bytes-per-token` must come from verified GGUF
architecture values, not from file size:

```powershell
meshcompute backend serve my-model `
  --binary "C:\llama.cpp\build-cuda\bin\Release\llama-server.exe" `
  --context 2048 `
  --gpu-layers 20 `
  --kv-bytes-per-token <verified-value>
```

The adapter expects the backend at `http://127.0.0.1:8080` by default. If you
use another loopback port, set `meshcompute config set backend_url
http://127.0.0.1:<port>` before serving. `--gpu-layers 20` is an example, not a
fit guarantee; confirm actual offload in the llama-server log and reduce it if
allocation fails. RPC remains disabled and is not a supported distributed path.

## Dashboard and diagnostics

```powershell
meshcompute ui
```

Open the printed `http://127.0.0.1:8081/?token=...` URL on the same machine.
The dashboard is loopback-only, exchanges the token for an HttpOnly cookie, and
can display every configured peer's GPUs and stream a prompt to one selected
node. “VRAM total” is a sum of per-GPU capacities for display, not a shared
pool, and a request never spans machines.

Useful checks:

```powershell
meshcompute doctor
meshcompute status
meshcompute monitor --interval 5s
meshcompute network probe worker1
```

Hardware discovery uses Windows PowerShell CIM (`Win32_OperatingSystem`,
`Win32_Processor`, disk `PSDrive`, and GPU providers). Redirected output is
normalized for UTF-8, UTF-16 LE/BE, and BOM variants. If WMI/PowerShell is
disabled or policy-blocked, fields remain `null` and an issue is reported; the
tool does not fabricate RAM, GPU, or disk values.

## Operational limits

- No Windows service, auto-start, tray app, or remote administration is
  installed. Use Task Scheduler manually if you need startup behavior.
- No certificate rotation, CRL/OCSP, rate limiting, or audit shipping.
- Worker-to-worker requests are rejected; a worker dashboard sees itself and its
  coordinator only.
- Killing the agent or `llama-server` cancels or fails in-flight work. There is
  no automatic retry or mid-generation placement change.
- Windows ACLs on the user profile protect state; Unix mode-bit checks and
  directory `fsync` are intentionally absent on Windows.
