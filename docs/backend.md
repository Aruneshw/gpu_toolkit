# Local llama.cpp backend

## Verified upstream contract

Implementation reviewed against official llama.cpp commit
`4b77dee0524c64b8471869232e2958bd9589607d` (upstream HEAD retrieved during
implementation), not an unpinned third-party binary:

- https://github.com/ggml-org/llama.cpp/blob/4b77dee0524c64b8471869232e2958bd9589607d/tools/server/README.md
- https://github.com/ggml-org/llama.cpp/blob/4b77dee0524c64b8471869232e2958bd9589607d/tools/rpc/README.md

The adapter uses native `POST /completion`, not OpenAI chat completion. It sends
`prompt`, bounded `n_predict`, `stream:false` and `cache_prompt:false`; reads
`content`, `truncated` and `timings.predicted_per_second`. Prompts are raw text;
chat template application is not implemented. Missing timing is reported as zero
(unavailable), never a fabricated benchmark.

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
redirects are disallowed. Each generation has a two-minute overall deadline,
including all checks; earlier caller cancellation wins. Prompt limit is 1 MiB,
each response limit 8 MiB, output limit 32,768 tokens, and context limit 1,048,576
tokens (also bounded by the actual server). No automatic retries or placement
changes occur. Loopback is not authentication against other local users.

## Go integration

```go
model.Add(dir, name, path string) (model.Entry, error)
model.List(dir string) ([]model.Entry, error)
model.Get(dir, name string) (model.Entry, error)
model.Verify(entry model.Entry) error
backend.New(endpoint string) (*backend.Llama, error)
(*backend.Llama).Health(ctx context.Context) (backend.Capabilities, error)
(*backend.Llama).Generate(ctx context.Context, r backend.Request) (backend.Response, error)
backend.Run(ctx context.Context, c backend.LaunchConfig) error
```

`Entry` fields: `Name, Path, SHA256 string; SizeBytes int64`.
`Request` fields: `Model, Prompt string; MaxTokens, Context int`.
`Response` fields: `Content string; TokensPerSecond float64`.
`Capabilities` fields: `Models []string; Context int`.
`LaunchConfig` fields: `Binary, RegistryDir, Model, Endpoint string;
Context, GPULayers int; Stdout, Stderr io.Writer`.

The caller must serialize inference, require local registration via Get/Verify,
and add only authenticated actual participant identity to its own response.
The backend does not invent participant, GPU-use or distributed-execution claims.
The exported `backend.Adapter` interface contains Health and Generate.
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

## Experimental RPC is not implemented distributed execution

Upstream names the RPC binary `ggml-rpc-server`, builds it with
`-DGGML_RPC=ON`, connects from llama-cli/llama-server with `--rpc`, and supports
`--tensor-split` for proportions. Its current README explicitly calls RPC
fragile, insecure proof-of-concept software and warns never to run it on an open
network or in a sensitive environment. A VPN does not fix unsafe parsers.

This toolkit does not build, start, expose, discover, configure or verify RPC
workers; does not claim verified model-parallel/distributed execution; and does
not aggregate unrelated GPUs as unified VRAM. The launcher removes ambient
LLAMA_* and GGML_RPC* settings. Sending a job to one authenticated node that
runs its own local model is remote single-node inference, not tensor splitting.

## Verification scope

`go test ./internal/model ./internal/backend` uses local HTTP fixtures and a
test helper subprocess. It verifies registry hashing/magic, mutation rejection,
KV arithmetic/overflow, endpoint restrictions, alias/context/token checks,
cancellation, response bounds, HTTP failures and launch arguments. A fake GGUF
and fixture text are deliberately not evidence of real model loading/inference.
No real llama-server, CUDA, Windows runtime or distributed inference result is
claimed by these tests. Run a real licensed model and inspect logs on each
target platform before deployment.
