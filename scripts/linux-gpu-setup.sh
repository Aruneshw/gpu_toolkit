#!/usr/bin/env bash
# Install llama.cpp (llama-server + its RPC server) for MeshCompute on Linux.
#
#   on a friend's laptop:  it can then lend its GPU to the group's pooled model
#   on the group host:     it can then run one model across every shared GPU
#
# Nothing is installed system-wide: files go to ~/.local/share/meshcompute/llama.
# The same llama.cpp release must be used on every computer of a group (the
# RPC protocol is version-checked); this script pins one.
set -euo pipefail

TAG="b12182"
DEST="${MESHCOMPUTE_LLAMA_DIR:-$HOME/.local/share/meshcompute/llama}"
MODELS="$HOME/.local/share/meshcompute/models"
BIN="${MESHCOMPUTE_BIN:-meshcompute}"
HOST_HOME="${MESHCOMPUTE_HOST_HOME:-$HOME/.meshcompute-auto}"

say() { printf '%s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) ARCH=x64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

# Variant: Vulkan drives NVIDIA, AMD and Intel GPUs with the normal graphics
# driver and needs no CUDA toolkit. Override with MESHCOMPUTE_LLAMA_VARIANT=cpu.
VARIANT="${MESHCOMPUTE_LLAMA_VARIANT:-}"
if [ -z "$VARIANT" ]; then
  if ldconfig -p 2>/dev/null | grep -q 'libvulkan\.so\.1'; then VARIANT=vulkan; else VARIANT=cpu; fi
fi
case "$VARIANT-$ARCH" in
  vulkan-x64)   FILE="llama-$TAG-bin-ubuntu-vulkan-x64.tar.gz";   SUM=48e5be53921e4213991b6210727941f8a0645c75099d6e1594a126d6d5428fcf ;;
  vulkan-arm64) FILE="llama-$TAG-bin-ubuntu-vulkan-arm64.tar.gz"; SUM=def1bcedc0b6c57fc081b450d2c0c7dc0013dd1187e0963190f96850b986c5ab ;;
  cpu-x64)      FILE="llama-$TAG-bin-ubuntu-x64.tar.gz";          SUM=c1dbfdb9cbef7ee50b6ab08142d6424184d68b242906837770439ed118c57d99 ;;
  cpu-arm64)    FILE="llama-$TAG-bin-ubuntu-arm64.tar.gz";        SUM=57a4c7f59d2e8c790872141f0f9299cd5cc53b20e3ef3fcf2a7f3344eb8f1e72 ;;
  *) die "unknown variant '$VARIANT' (use vulkan or cpu)" ;;
esac
if [ "$VARIANT" = cpu ]; then
  say "!!  No Vulkan library found: installing the CPU build. Install your GPU's Vulkan driver"
  say "    (package libvulkan1 plus the NVIDIA/AMD/Intel driver) and run this again to use the GPU."
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
say "==> Downloading llama.cpp $TAG ($VARIANT, $ARCH)..."
curl -fL --retry 5 --retry-delay 3 -o "$tmp/llama.tgz" "https://github.com/ggml-org/llama.cpp/releases/download/$TAG/$FILE" ||
  die "download failed; check your internet connection"
[ "$(sha256sum "$tmp/llama.tgz" | awk '{print $1}')" = "$SUM" ] || die "checksum mismatch for $FILE; not installing"
mkdir -p "$DEST"
tar -xzf "$tmp/llama.tgz" -C "$DEST" --strip-components=1
[ -x "$DEST/llama-server" ] || die "llama-server missing after extraction"
[ -x "$DEST/ggml-rpc-server" ] || [ -x "$DEST/rpc-server" ] || die "the RPC server is missing after extraction"
say "==> Installed in $DEST (checksum verified)."
say ""
say "This computer can now lend its GPU: run ./linux.sh join <KEY> (or ./linux.sh reconnect)."
say "MeshCompute starts the RPC server itself, on loopback only."
say ""

# Optional: a small model for the group host's pool.
answer="${MESHCOMPUTE_DOWNLOAD_MODEL:-}"
if [ -z "$answer" ] && [ -t 0 ]; then
  say "Hosting a group? The host needs a model file to pool across everyone's GPUs."
  read -r -p "Download a small test model (Qwen2.5 1.5B, 986 MB) and register it as 'qwen'? [y/N]: " answer || answer=""
fi
case "$answer" in
  y|Y|yes|1)
    mkdir -p "$MODELS"
    gguf="$MODELS/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf"
    if [ ! -s "$gguf" ]; then
      curl -fL --retry 5 --retry-delay 3 -o "$gguf.part" \
        "https://huggingface.co/bartowski/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf" || die "model download failed"
      mv "$gguf.part" "$gguf"
    fi
    "$BIN" --home "$HOST_HOME" install >/dev/null
    rm -f "$HOST_HOME/models/qwen.json"
    "$BIN" --home "$HOST_HOME" model add qwen "$gguf" >/dev/null || die "model registration failed"
    say "==> Registered model 'qwen'. Start hosting (./linux.sh host) and click Start pool on the dashboard,"
    say "    or run: $BIN --home $HOST_HOME pool start qwen"
    ;;
  *)
    say "To pool your own model on the host: $BIN --home $HOST_HOME model add NAME /path/model.gguf"
    ;;
esac
