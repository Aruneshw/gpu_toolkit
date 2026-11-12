#!/usr/bin/env bash
# Live pool test with a real llama.cpp: builds MeshCompute, starts a group host
# and 3 workers on loopback that lend real ggml-rpc-servers, then checks two
# named pools of one model at once (chosen workers, split weights, q8_0 KV,
# flash attention, KV on host RAM, request slots), round-robin between the
# replicas, refusals with reasons, and that no llama-server outlives a removed
# pool or the host.
#   LLAMA_DIR=~/llama POOL_MODEL=~/models/small.gguf scripts/pool-test.sh
set -uo pipefail
cd "$(dirname "$0")/.."
: "${LLAMA_DIR:?set LLAMA_DIR to a llama.cpp folder with llama-server and ggml-rpc-server}"
: "${POOL_MODEL:?set POOL_MODEL to a small .gguf file}"
R="$(mktemp -d)"
B="$R/meshcompute"
go build -o "$B" ./cmd/meshcompute || exit 2
L="$LLAMA_DIR"
M="$POOL_MODEL"
mkdir -m 700 "$R/H"
export MESHCOMPUTE_RELAY=none MESHCOMPUTE_NO_BROWSER=1
cleanup() { for p in $(pgrep -x meshcompute); do tr '\0' ' ' </proc/$p/cmdline 2>/dev/null | grep -q -- "--home $R/" && kill $p; done; sleep 2; rm -rf "$R"; }
trap cleanup EXIT
port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
fail=0
ok() { if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi; }
H() { "$B" --home "$R/H" "$@"; }
servers() { pgrep -xc llama-server || true; }

H install >/dev/null; H config set llama_server "$L/llama-server" >/dev/null
hp=$(port); ui=$(port)
"$B" --home "$R/H" host --listen 127.0.0.1:$hp --ui 127.0.0.1:$ui >"$R/host.log" 2>&1 & hostpid=$!
for _ in $(seq 50); do grep -q '^eyJ' "$R/host.log" && break; sleep 0.2; done
for w in W1 W2 W3; do
  mkdir -m 700 "$R/$w"
  code=$(H pair --ttl 5m 2>/dev/null | grep '^eyJ' | head -1)
  "$B" --home "$R/$w" install >/dev/null
  "$B" --home "$R/$w" config set listen "127.0.0.1:$(port)" >/dev/null
  "$B" --home "$R/$w" config set rpc_server "$L/ggml-rpc-server" >/dev/null
  echo "$code" | MESHCOMPUTE_WORKER_UI="127.0.0.1:$(port)" "$B" --home "$R/$w" join --stdin --no-open --tasks auto >"$R/$w.log" 2>&1 &
done
for _ in $(seq 150); do [ "$(grep -l 'Connected to the group host' "$R"/W*.log 2>/dev/null | wc -l)" -eq 3 ] && break; sleep 0.2; done
ok "3 workers connected" '[ "$(grep -l "Connected to the group host" "$R"/W*.log | wc -l)" -eq 3 ]'
H model add qwen "$M" >/dev/null
ok "model scan lists the model" 'H model scan "$(dirname "$M")" | grep -qF "$(basename "$M")"'

J() { H pool status | python3 -c "import json,sys;d=json.load(sys.stdin);$1"; }
for _ in $(seq 120); do [ "$(J 'print(len(d["sharing"]))')" = 3 ] && break; sleep 0.5; done
ok "all 3 workers offer their GPU" '[ "$(J "print(len(d[\"sharing\"]))")" = 3 ]'
read -r w1 w2 w3 <<<"$(J 'print(" ".join(sorted(d["sharing"])))')"
pool() { J "p=[x for x in d['pools'] if x['name']=='$1'];print($2 if p else '')"; }
waitready() { for _ in $(seq 180); do [ "$(pool $1 'p[0]["state"]')" = ready ] && return 0; sleep 0.5; done; return 1; }
H pool start qwen --name a --context 2048 --slots 2 --workers $w1,$w2 --split local=1,$w1=1,$w2=2 --kv q8_0 --flash on >/dev/null
H pool start qwen --name b --context 1024 --workers $w3 --kv-on-host --output-layer host >/dev/null
ok "pool a ready" 'waitready a'
ok "pool b ready" 'waitready b'
for n in a b; do [ "$(pool $n 'p[0]["state"]')" = ready ] || pool $n '"%s: %s\n%s" % (p[0]["state"], p[0].get("detail"), "\n".join(p[0].get("log") or []))'; done
J 'import json;[print(x["name"],x["state"],x["workers"],x.get("parallel"),json.dumps(x["settings"]),[(v.get("name") or v.get("device"),) for v in x.get("devices",[])]) for x in d["pools"]]'
ok "two pools run at the same time (2 llama-servers)" '[ "$(servers)" = 2 ]'
ok "pool a uses exactly its chosen workers" '[ "$(pool a "\" \".join(sorted(p[0][\"workers\"]))")" = "$(echo $w1 $w2 | tr " " "\n" | sort | xargs)" ]'
ok "pool b uses only its worker" '[ "$(pool b "\" \".join(p[0][\"workers\"])")" = "$w3" ]'
ok "pool a has 2 request slots" '[ "$(pool a "p[0][\"parallel\"]")" = 2 ]'
ok "pool a: q8_0 KV and flash attention on llama-server" 'pgrep -xa llama-server | grep -- "-ctk q8_0\|--cache-type-k q8_0" | grep -q -- "-fa on\|--flash-attn on"'
if "$L/llama-server" --list-devices 2>/dev/null | grep -qiE "CUDA|Vulkan|Metal"; then want="--tensor-split 1,1,2"; else want="--device RPC0,RPC1 --tensor-split 1,2"; fi
ok "pool a: split local=1,w1=1,w2=2 reaches llama-server ($want)" 'pgrep -xa llama-server | grep -q -- "$want"'
if [ "$want" != "--tensor-split 1,1,2" ]; then ok "pool a says the host's share was ignored (no GPU here)" '[ -n "$(pool a "p[0][\"detail\"]" | grep "split share (local=1) is ignored")" ]'; fi
ok "pool b: KV cache kept in host RAM" 'pgrep -xa llama-server | grep -q -- "--no-kv-offload\|-nkvo"'
ok "pool b: output layer on the host" 'pgrep -xa llama-server | grep -- "--no-kv-offload" | grep -qF -- "--override-tensor ^(output|token_embd)\.weight\$=CPU"'
pgrep -xa llama-server | cut -c1-400 > "$R/servers.txt"
# 3 slots in total across the two replicas: 3 parallel requests all succeed and both pools serve.
for i in 1 2 3; do H run qwen --prompt "Count to five." --max-tokens 24 --context 512 >"$R/run$i.out" 2>&1 & done; wait $(jobs -p | tail -3) 2>/dev/null
sleep 1
ok "3 parallel requests (= free slots) all answered" '[ "$(grep -l "\"content\": \"." "$R"/run[123].out | wc -l)" = 3 ]'
ok "both replicas served requests" 'grep -q "event=inference pool=a outcome=" "$R/host.log" && grep -q "event=inference pool=b outcome=" "$R/host.log"'
# 6 at once: more than the 3 slots -> the extra ones get the clear busy message, nothing else.
for i in 4 5 6 7 8 9; do H run qwen --prompt "Write a long poem about rivers." --max-tokens 64 --context 512 >"$R/run$i.out" 2>&1 & done; wait $(jobs -p | tail -6) 2>/dev/null
sleep 1
nok=$(grep -l "\"content\": \"." "$R"/run[4-9].out | wc -l); nbusy=$(grep -l "slot of the pools serving qwen is busy" "$R"/run[4-9].out | wc -l)
echo "6 concurrent: answered=$nok busy=$nbusy"
ok "over-capacity requests are refused only with the busy message" '[ $((nok+nbusy)) = 6 ] && [ $nok -ge 3 ]'
ok "context bigger than any slot is refused with the reason" 'H run qwen --prompt hi --max-tokens 4 --context 4096 >"$R/ctx.out" 2>&1; grep -q "at most .* tokens per request\|more than one request slot holds there (1024); use --context 1024" "$R/ctx.out"'
# A request for a named pool goes there every time, never to the other replica.
before=$(grep -c "event=inference pool=b outcome=completed" "$R/host.log")
for i in 1 2 3; do H run qwen --pool b --prompt "Say hi." --max-tokens 8 --context 512 >"$R/named$i.out" 2>&1; done
after=$(grep -c "event=inference pool=b outcome=completed" "$R/host.log")
ok "3 runs with --pool b all ran on pool b" '[ $((after-before)) = 3 ] && [ "$(grep -l "\"content\": \"." "$R"/named[123].out | wc -l)" = 3 ]'
H run qwen --pool nope --prompt hi --max-tokens 4 --context 512 >"$R/nope.out" 2>&1
ok "an unknown pool is refused with the pool names" 'grep -q "no pool named nope there (pools: a, b)\|no pool named nope there (pools: b, a)" "$R/nope.out"'
H run qwen --pool a --prompt hi --max-tokens 4 --context 2048 >"$R/actx.out" 2>&1
ok "a context bigger than pool a's slots is refused for that pool" 'grep -q "one request slot of pool a holds (1024); use --context 1024" "$R/actx.out"'
H pool stop b >/dev/null; sleep 2
ok "stopping pool b leaves only pool a" '[ "$(servers)" = 1 ]'
H pool remove a >/dev/null; sleep 2
ok "removing pool a leaves no llama-server" '[ "$(servers)" = 0 ]'
H pool start qwen --name b --context 1024 --workers $w3 >/dev/null
ok "pool b restarts" 'waitready b'
kill -TERM $hostpid; for _ in $(seq 20); do kill -0 $hostpid 2>/dev/null || break; sleep 0.5; done
sleep 1
ok "host exits on SIGTERM" '! kill -0 $hostpid 2>/dev/null'
ok "no llama-server survives the host" '[ "$(servers)" = 0 ]'
echo; [ $fail = 0 ] && echo "ALL PASSED" || echo "SOME FAILED"
exit $fail
