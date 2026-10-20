# Local dev build: identical linker flags to tools\release.ps1 so the exe keeps
# the GUI subsystem. A plain `go build` produces a CONSOLE exe, and Windows then
# attaches a black console window to every launch — which release builds never
# show, because they link with -H=windowsgui.
#
# Usage:  powershell -File tools\dev.ps1 -Out "D:\opencode zen\out\newbin\zen-gate.exe"
#         powershell -File tools\dev.ps1 -Version 1.9.0 -Out ..\tmp\zen-gate.exe
param(
    [string]$Version,
    [Parameter(Mandatory = $true)]
    [string]$Out
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$ld = "-H=windowsgui"
if ($Version) {
    # Same two -X targets as the release build; without them the exe reports the
    # compiled-in default (1.1.0) and nags about an update that is already older.
    $ld += " -X zen-gate/internal/gateway.Version=$Version -X zen-gate/internal/update.Current=$Version"
}

go build -trimpath -ldflags $ld -o $Out ./cmd/zen-gate
if ($LASTEXITCODE -ne 0) { throw "build failed" }

# Prove the subsystem instead of trusting the flags: 2 = GUI, 3 = console.
$fs = [IO.File]::OpenRead((Resolve-Path $Out).Path)
try {
    $br = New-Object IO.BinaryReader($fs)
    $fs.Position = 0x3C
    $pe = $br.ReadInt32()
    $fs.Position = $pe + 0x5C
    $sub = $br.ReadUInt16()
} finally {
    $fs.Close()
}
if ($sub -ne 2) { throw "$Out has subsystem $sub, expected 2 (GUI); a console window would pop up" }
Write-Host "built $Out (subsystem=GUI)"
