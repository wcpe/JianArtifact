param (
    [Parameter(Mandatory = $false)] [string]$Version = "",
    [Parameter(Mandatory = $false)] [string]$Output = "dist/release"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$goArgs = @("run", "./apps/server/cmd/release-assets", "--root", $root, "--output", $Output)
if ($Version -ne "") { $goArgs += @("--version", $Version) }
& go @goArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
