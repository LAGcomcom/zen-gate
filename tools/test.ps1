# Run package tests with a FIXED test-binary path.
#
# Why this exists: `go test` compiles each package's test binary into
# %TEMP%\go-build<random>\b<NNN>\<pkg>.test.exe — a different path on every run.
# When that binary calls listen(2), Windows Firewall sees an unknown program and
# shows the "allow this app on these networks" dialog again, recording one more
# rule each time. Unchecked it reached 70 stale rules pointing at temp files
# that no longer existed, and every single test run cost the user a click.
# Compiling to build\test\ first pins the path: the dialog appears at most once
# per package, and the rule it creates stays valid across rebuilds.
#
# The binary is then run from the package's own directory, which is what
# `go test` does for you — tests that read relative fixtures depend on it.
#
# Usage:
#   powershell -File tools\test.ps1 -Packages ./internal/subs
#   powershell -File tools\test.ps1 -Packages ./internal/lane,./internal/gateway
#   powershell -File tools\test.ps1 -Packages ./internal/subs -Args "-count=1 -run Sticky"
#
# -Args is passed to the test binary verbatim (it is the equivalent of the
# flags you would hand `go test`, without the build flags).

param(
    [string[]]$Packages = @("./internal/gateway"),
    [string]$Args = ""
)

# Continue, not Stop: a Go test binary writes its log lines to stderr, and
# Windows PowerShell 5.1 turns every stderr line of a native command into a
# terminating ErrorRecord under "Stop". Failures are reported through
# $LASTEXITCODE below instead.
$ErrorActionPreference = "Continue"
$Root = Split-Path -Parent $PSScriptRoot
$OutDir = Join-Path $Root "build\test"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$argv = @()
if ($Args.Trim().Length -gt 0) { $argv = $Args -split "\s+" }

$failed = @()
foreach ($pkg in $Packages) {
    if ($pkg -match "\.\.\.") {
        Write-Host "skip: -o takes one package at a time, list them out: $pkg" -ForegroundColor Yellow
        continue
    }
    $rel = $pkg -replace "^\./", ""
    $safe = $rel -replace "[\\/]", "_"
    $bin = Join-Path $OutDir ($safe + ".test.exe")
    $dir = Join-Path $Root ($rel -replace "/", "\")

    Write-Host "==> $pkg"
    Push-Location $Root
    & go test -c -o $bin $pkg
    $built = $LASTEXITCODE
    Pop-Location
    if ($built -ne 0) {
        $failed += $pkg
        continue
    }
    Push-Location $dir
    & $bin @argv
    $code = $LASTEXITCODE
    Pop-Location
    if ($code -ne 0) { $failed += $pkg }
}

if ($failed.Count -gt 0) {
    Write-Host ("FAILED: " + ($failed -join ", ")) -ForegroundColor Red
    exit 1
}
Write-Host "all packages passed" -ForegroundColor Green
