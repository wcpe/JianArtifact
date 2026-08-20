[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repoRoot

function Require-Command {
  param([Parameter(Mandatory)][string]$Name)

  if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
    throw "缺少工具：$Name。请先安装并确保它位于 PATH 中。"
  }
}

function Invoke-Checked {
  param(
    [Parameter(Mandatory)][string]$Label,
    [Parameter(Mandatory)][scriptblock]$Action
  )

  Write-Host "==> $Label"
  & $Action
  if ($LASTEXITCODE -ne 0) {
    throw "步骤失败：$Label（退出码 $LASTEXITCODE）"
  }
}

foreach ($tool in @("pnpm", "go", "gofmt", "golangci-lint")) {
  Require-Command $tool
}

Invoke-Checked "安装前端依赖" { pnpm install --frozen-lockfile }
Invoke-Checked "前端格式检查" { pnpm format:check }
Invoke-Checked "前端静态检查" { pnpm lint }
Invoke-Checked "前端类型检查" { pnpm typecheck }
Invoke-Checked "前端测试与契约检查" { pnpm test }
Invoke-Checked "前端构建" { pnpm build }

$webDist = Join-Path $repoRoot "apps/web/dist"
$embedDist = Join-Path $repoRoot "apps/server/web/dist"
if (-not (Test-Path -LiteralPath $webDist -PathType Container)) {
  throw "前端构建产物不存在：$webDist"
}

Write-Host "==> 同步前端产物到后端 embed 目录"
if (Test-Path -LiteralPath $embedDist) {
  Remove-Item -LiteralPath $embedDist -Recurse -Force
}
New-Item -ItemType Directory -Path $embedDist -Force | Out-Null
Copy-Item -Path (Join-Path $webDist "*") -Destination $embedDist -Recurse -Force

$serverRoot = Join-Path $repoRoot "apps/server"
$serverBin = Join-Path $serverRoot "bin/jianartifact.exe"
Push-Location -LiteralPath $serverRoot
try {
  $formatFiles = @(gofmt -l .)
  if ($LASTEXITCODE -ne 0) {
    throw "gofmt 执行失败（退出码 $LASTEXITCODE）"
  }
  if ($formatFiles.Count -gt 0) {
    Write-Host "以下 Go 文件未格式化：" -ForegroundColor Red
    $formatFiles | ForEach-Object { Write-Host $_ }
    throw "Go 格式检查失败"
  }

  Invoke-Checked "Go 静态分析" { go vet ./... }
  Invoke-Checked "Go lint" { golangci-lint run }

  $oldCgo = $env:CGO_ENABLED
  try {
    $env:CGO_ENABLED = "1"
    Invoke-Checked "Go race 测试" { go test -race -count=1 ./... }

    $toolchainLine = Get-Content -LiteralPath (Join-Path $serverRoot "go.mod") |
      Where-Object { $_ -match '^toolchain\s+(\S+)' } |
      Select-Object -First 1
    if (-not $toolchainLine -or $toolchainLine -notmatch '^toolchain\s+(\S+)') {
      throw "go.mod 未声明 toolchain，无法选择安全的 Go 漏洞扫描工具链"
    }
    $oldToolchain = $env:GOTOOLCHAIN
    try {
      $env:GOTOOLCHAIN = $Matches[1]
      Invoke-Checked "Go 漏洞扫描" { go run golang.org/x/vuln/cmd/govulncheck@latest ./... }
    }
    finally {
      if ($null -eq $oldToolchain) {
        Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue
      }
      else {
        $env:GOTOOLCHAIN = $oldToolchain
      }
    }

    $env:CGO_ENABLED = "0"
    New-Item -ItemType Directory -Path (Split-Path -Parent $serverBin) -Force | Out-Null
    Invoke-Checked "Go 静态构建" { go build -trimpath -o $serverBin ./cmd/jianartifact }
  }
  finally {
    if ($null -eq $oldCgo) {
      Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
    else {
      $env:CGO_ENABLED = $oldCgo
    }
  }
}
finally {
  Pop-Location
}

Write-Host "==> 质量门全绿。" -ForegroundColor Green
