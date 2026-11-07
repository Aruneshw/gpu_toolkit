$ErrorActionPreference = 'Stop'

$root     = Join-Path $env:LOCALAPPDATA 'meshcompute'
$llamaDir = Join-Path $root 'llama'
$models   = Join-Path $root 'models'
$mc       = Join-Path $root 'bin\meshcompute.exe'

New-Item -ItemType Directory -Force -Path $llamaDir, $models | Out-Null

if (-not (Test-Path $mc)) {
    Write-Host "meshcompute.exe not found at $mc" -ForegroundColor Red
    Write-Host "Run join.cmd from the meshcompute ZIP folder first, then run this again." -ForegroundColor Yellow
    return
}

function Get-BigFile([string]$url, [string]$dest, [string]$label) {
    if ((Test-Path $dest) -and ((Get-Item $dest).Length -gt 50MB)) {
        Write-Host "$label already downloaded" -ForegroundColor DarkGray
        return
    }
    Write-Host "Downloading $label ..." -ForegroundColor Cyan
    & curl.exe -L --fail --retry 5 --retry-delay 3 -o $dest $url
    if ($LASTEXITCODE -ne 0) { throw "download failed: $url" }
}

$tag = 'b12182'
$base = "https://github.com/ggml-org/llama.cpp/releases/download/$tag"
$zipLlama  = Join-Path $env:TEMP 'llama-cuda.zip'
$zipCudart = Join-Path $env:TEMP 'cudart.zip'
$gguf      = Join-Path $models 'Qwen2.5-1.5B-Instruct-Q4_K_M.gguf'

Get-BigFile "$base/llama-b12182-bin-win-cuda-12.4-x64.zip"        $zipLlama  'llama.cpp CUDA build (275 MB)'
Get-BigFile "$base/cudart-llama-bin-win-cuda-12.4-x64.zip"        $zipCudart 'CUDA runtime DLLs (374 MB)'

Write-Host "Extracting ..." -ForegroundColor Cyan
Expand-Archive -Path $zipLlama  -DestinationPath $llamaDir -Force
Expand-Archive -Path $zipCudart -DestinationPath $llamaDir -Force

$server = Join-Path $llamaDir 'llama-server.exe'
if (-not (Test-Path $server)) { throw 'llama-server.exe missing after extraction' }

Get-BigFile 'https://huggingface.co/bartowski/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/Qwen2.5-1.5B-Instruct-Q4_K_M.gguf' $gguf 'Qwen2.5-1.5B model (986 MB)'

Remove-Item (Join-Path $models 'qwen.json') -ErrorAction SilentlyContinue
Write-Host "Registering model as 'qwen' (hashing 986 MB, please wait) ..." -ForegroundColor Cyan
& $mc model add qwen $gguf
if ($LASTEXITCODE -ne 0) { throw 'model registration failed' }

Write-Host ''
Write-Host 'Starting the GPU backend. Keep this window open.' -ForegroundColor Green
Write-Host ''
& $mc backend serve qwen --binary $server --context 2048 --gpu-layers 99 --kv-bytes-per-token 28672
