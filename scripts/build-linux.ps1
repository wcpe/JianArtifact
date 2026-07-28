# 交叉编译 linux/amd64 服务端二进制（含 embed 前端资源）
# 用法：pwsh -File scripts/build-linux.ps1 -Version "0.6.0-dev+xxx" -AssetHash "xxxx"
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$AssetHash
)
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$env:GOCACHE = Join-Path $root ".gocache"
$env:GOMODCACHE = Join-Path $root ".gomodcache"
$env:GOFLAGS = ""
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o dist\jianartifact-linux-amd64 ./apps/server/cmd/jianartifact
if ($LASTEXITCODE -ne 0) { exit 1 }
# 验证二进制包含本次前端资源哈希（防止 embed 资源未更新）
$ok = Select-String -Path dist\jianartifact-linux-amd64 -Pattern $AssetHash -Quiet
Write-Host "asset-hash-embedded: $ok"
if (-not $ok) { exit 2 }
