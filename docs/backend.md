# Local llama.cpp backend

## Verified upstream contract

Implementation reviewed against official llama.cpp commit
`4b77dee0524c64b8471869232e2958bd9589607d` (upstream HEAD retrieved during
implementation), not an unpinned third-party binary:

- https://github.com/ggml-org/llama.cpp/blob/4b77dee0524c64b8471869232e2958bd9589607d/tools/server/README.md
- https://github.com/ggml-org/llama.cpp/blob/4b77dee0524c64b8471869232e2958bd9589607d/tools/rpc/README.md

The adapter uses native `POST /completion`, not OpenAI chat completion. It sends
`prompt`, bounded `n_predict`, `stream:false` and `cache_prompt:false`; reads
`content`, `truncated` and `timings.predicted_per_second`. For streaming it sends
`stream:true`, parses `data: {...}` SSE frames, relays non-empty `content`, and
requires a final frame with `stop:true` carrying `timings`. A clean EOF or
`[DONE]` before that final frame is an error, so a truncated response is never
reported as complete. Prompts are raw text; chat template application is not
implemented. Missing timing is reported as zero (unavailable), never a fabricated
benchmark.

Before each inference it checks `GET /health` (`status:ok`), `GET /v1/models`
(exact selected alias, exactly one advertised model), and `GET /props`
(`default_generation_settings.n_ctx`). It then uses `POST /tokenize` with
`add_special:true` to check prompt tokens plus output against the requested
context budget. Context is a budget, not a command to resize the running server.
Unknown context fails closed. This supports a directly launched single-model
server, not llama.cpp router mode. Alias verification is not cryptographic proof
of the weights in an independently started server; use the registered launch
path and protect the local process and registry from modification.

Only HTTP numeric loopback IP origins are accepted (IPv4 or IPv6). Hostname
resolution, external endpoints, proxies, URL credentials, paths, queries and
redirects are disallowed. Non-streaming generation has a two-minute overall
deadline including all checks. Streaming uses the same two-minute admission
phase, then a hard 30-minute generation bound plus caller cancellation; the
agent also sets per-write deadlines so a stalled browser cannot hold the stream
open. Prompt limit is 1 MiB, each response limit 8 MiB, output limit 32,768
tokens, and context limit 1,048,576 tokens (also bounded by the actual server).
No automatic retries or placement changes occur. Loopback is not authentication
against other local users.

## Go integration

```go
model.Add(dir, name, path string) (model.Entry, error)
model.List(dir string) ([]model.Entry, error)
model.Get(dir, name string) (model.Entry, error)
model.Verify(entry model.Entry) error
backend.New(endpoint string) (*backend.Llama, error)
(*backend.Llama).Health(ctx context.Context) (backend.Capabilities, error)
(*backend.Llama).Generate(ctx context.Context, r backend.Request) (backend.Response, error)
(*backend.Llama).GenerateStream(ctx context.Context, r backend.Request, emit func(backend.Chunk) error) error
backend.Run(ctx context.Context, c backend.LaunchConfig) error
```

`Entry` fields: `Name, Path, SHA256 string; SizeBytes int64`.
`Request` fields: `Model, Prompt string; MaxTokens, Context int`.
`Response` fields: `Content string; TokensPerSecond float64`.
`Chunk` fields: `Content string; Done bool; TokensPerSecond float64`.
`Capabilities` fields: `Models []string; Context int`.
`LaunchConfig` fields: `Binary, RegistryDir, Model, Endpoint string;
Context, GPULayers int; Stdout, Stderr io.Writer`.

The caller must serialize inference, require local registration via Get/Verify,
and add only authenticated actual participant identity to its own response.
The backend does not invent participant, GPU-use or distributed-execution claims.
The exported `backend.Adapter` interface contains Health and Generate;
streaming is available on the concrete `*Llama` type through `GenerateStream`.
Run requires an absolute trusted executable path and explicit valid port. It
blocks in the foreground, uses argument-array exec without a shell, loads the
registered absolute GGUF path after SHA256 verification, fixes parallelism to
one and disables context shifting. Cancellation kills the child. Pass stdout
and stderr to the terminal to preserve diagnostics. Startup readiness is checked
separately with Health; launching is not proof of successful inference.

Registration reads a local regular file, verifies the four-byte GGUF magic and
hashes all bytes. It does not download, copy, parse all GGUF metadata, validate
tensors, check licensing, or prove a file is safe. Duplicate aliases are rejected.
Registry and model directories must be trusted and not writable by remote users;
do not mutate weights during launch (hashing and child file opening are separate).

## Memory and metadata

File size is not a RAM/VRAM admission estimate. No accurate total-memory estimate
is invented from file size, quantization label, or model name. Registration does
not infer architecture. Obtain architecture values from verified GGUF metadata
and confirm runtime allocation from llama-server logs and OS/GPU measurements.
`model.KVInputs` requires all of Layers, KVHeads, KeyHeadDimension,
ValueHeadDimension, KeyBytesPerElement, ValueBytesPerElement, Context and
ParallelSlots. `KVBytes()` checks zero/overflow and computes only the dense
unquantized KV payload:

`layers * kvHeads * context * slots * (keyDimension * keyBytes + valueDimension * valueBytes)`.

This formula is not appropriate for every architecture (for example recurrent,
hybrid, compressed latent attention or quantized block caches). Do not use it
for those without a verified architecture-specific accounting method. It omits
weights, compute buffers, alignment, graph allocations, fragmentation, and driver
overhead; it is not a fit guarantee. Unknown inputs mean unknown capacity, not
zero memory. Keep measured headroom and lower context/GPU offload on allocation
failure. No automatic memory-based placement is implemented here.

## Explicit official build and foreground run

Install Git, CMake and a supported C/C++ compiler explicitly. On Linux:

```sh
git clone https://github.com/ggml-org/llama.cpp.git
cd llama.cpp
git checkout --detach 4b77dee0524c64b8471869232e2958bd9589607d
cmake -S . -B build-cpu -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=OFF -DGGML_RPC=OFF
cmake --build build-cpu --config Release --target llama-server -j 2
./build-cpu/bin/llama-server --model /absolute/path/model.gguf --alias my-model --host 127.0.0.1 --port 8080 --ctx-size 2048 --parallel 1 --n-gpu-layers 0 --no-context-shift
```

For NVIDIA, install a compatible NVIDIA driver, CUDA toolkit (including nvcc),
and a CUDA-supported host compiler. Build a separate directory:

```sh
cmake -S . -B build-cuda -DCMAKE_BUILD_TYPE=Release -DGGML_CUDA=ON -DGGML_RPC=OFF
cmake --build build-cuda --config Release --target llama-server -j 2
./build-cuda/bin/llama-server --model /absolute/path/model.gguf --alias my-model --host 127.0.0.1 --port 8080 --ctx-size 2048 --parallel 1 --n-gpu-layers 20 --no-context-shift
```

The GPU layer value is an explicit example, not an assurance of fitting your GPU.
Verify actual GPU offload in logs. Never use `-hf`/URL-based model options here;
obtain a licensed GGUF separately, then register the local path with the CLI's
`model add NAME PATH` workflow before using `backend serve NAME --context 2048
--gpu-layers 0 --kv-bytes-per-token VERIFIED_VALUE` or
`run NAME --prompt ... --max-tokens 128 --context 2048`.
Set the CLI's local backend executable/endpoint configuration as documented by
its help. These CLI command names belong to the coordinator layer.

Windows: use an x64 Visual Studio Developer PowerShell with C++ build tools and
CMake. Check out the same commit, configure `cmake -S . -B build-cpu -A x64
-DGGML_CUDA=OFF -DGGML_RPC=OFF`, then `cmake --build build-cpu --config Release
--target llama-server`. For the Visual Studio multi-config generator the binary
is normally `.\build-cpu\bin\Release\llama-server.exe`. Pass a quoted absolute
Windows GGUF path and the same loopback/alias/context arguments. For CUDA use a
separate build directory and `-DGGML_CUDA=ON` with compatible CUDA/Visual Studio
versions. CUDA runtime DLLs and driver must be available. Cross-compiling this
Go toolkit neither builds llama.cpp nor installs GPU dependencies; Windows
runtime, CPU/GPU compatibility and performance have not been validated here.

## Pooled GPUs through llama.cpp RPC

Upstream ships an RPC worker (`ggml-rpc-server`, formerly `rpc-server`; built
with `-DGGML_RPC=ON`) and `llama-server --rpc host:port,...`, which offloads a
model's layers to those workers. Upstream calls this backend experimental and
warns never to run the RPC server on an open network: it has no
authentication and its parsers are not hardened.

MeshCompute uses it as follows, so that warning is respected:

- **Worker.** On `join`/`start`, the agent looks for the RPC server
  (`rpc_server` setting, then PATH, then the llama directory the setup scripts
  use) and runs it as `--host 127.0.0.1 --port <free port> -c`. It is never
  bound to another address. `share_gpu off` disables it. The worker's status
  then reports `rpc_ready`.
- **Tunnel.** The host reaches that port only through `POST /v1/stream/rpc`
  with `Upgrade: meshcompute-stream/1` on the worker's own outbound,
  mutually authenticated TLS 1.3 connection (pinned host certificate, revocation
  re-checked every second). Bytes are then piped unmodified.
- **Host.** `pool start NAME` (dashboard: *Start pool*) opens one loopback
  forwarding port per lending worker and starts `llama-server --model ...
  --n-gpu-layers 999 --rpc 127.0.0.1:p1,127.0.0.1:p2 ...` on the configured
  loopback `backend_url`. llama.cpp divides the layers over the local GPU and
  the RPC devices by free memory. The choice is saved (`pool_model`,
  `pool_context`) and resumed by the next `host`.
- **Autonomy.** Every three seconds the host compares the lending workers with
  the running server's. If the set changed, it reloads the model (after the
  request in flight); if the server died, it restarts it with backoff. A worker
  that merely stops answering for a few seconds is not dropped.

Limits that are llama.cpp's, not hidden by this toolkit: all computers need the
same llama.cpp release (the RPC handshake checks the version); weights travel
to each worker on first load (the `-c` cache avoids repeats); every token
crosses the link between layers, so throughput follows link latency; a worker
leaving mid-generation fails that generation. Ambient `LLAMA_*` and
`GGML_RPC*` environment variables are still removed: only the arguments above
configure the processes. A worker that lends its GPU exposes the RPC server's
parsers to the group host and to nobody else.

Without the pool, sending a job to one authenticated node that runs its own
model remains remote single-node inference.

## Verification scope

`go test ./internal/model ./internal/backend` uses local HTTP fixtures and a
test helper subprocess. It verifies registry hashing/magic, mutation rejection,
KV arithmetic/overflow, endpoint restrictions, alias/context/token checks,
cancellation, response bounds, HTTP failures, streaming frame parsing,
missing/truncated final frames and launch arguments. A fake GGUF
and fixture text are deliberately not evidence of real model loading/inference.
No real llama-server, CUDA, Windows runtime or distributed inference result is
claimed by these tests. Run a real licensed model and inspect logs on each
target platform before deployment.
