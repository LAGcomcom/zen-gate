# Local code signing helper — stops the "Windows 已保护你的电脑" SmartScreen prompt.
#
# Why this exists: tools\release.ps1 produces an UNSIGNED exe. Launched from a shell
# (CreateProcess) it runs fine, but a double-click in Explorer goes through
# ShellExecute -> AppRep/SmartScreen, and a 0-reputation unsigned binary gets the
# "Windows 已保护你的电脑" wall. A valid Authenticode signature gives the file a
# publisher identity, so SmartScreen stops treating it as an anonymous download.
#
# One-time trust setup (per user, no admin required):
#   certutil -addstore -user -f Root "work\signtest\leaf.cer"        # chain terminates here
#   Import-Certificate -FilePath leaf.cer -CertStoreLocation Cert:\CurrentUser\TrustedPublisher
#
# Usage:
#   powershell -File tools\sign-local.ps1
#   powershell -File tools\sign-local.ps1 -Path dist\zen-gate.exe, dist\zenstats.exe
param(
    [string[]]$Path = @("dist\zen-gate.exe"),
    [string]$Subject = "Zen Gate Local Dev",
    [string]$TimestampServer = "http://timestamp.digicert.com"
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
    Where-Object { $_.Subject -match [regex]::Escape($Subject) } |
    Select-Object -First 1

if (-not $cert) {
    Write-Host "creating self-signed code signing cert '$Subject'"
    $cert = New-SelfSignedCertificate `
        -Type CodeSigningCert `
        -Subject "CN=$Subject, O=Zen Gate, C=CN" `
        -KeyUsage DigitalSignature `
        -KeyAlgorithm RSA `
        -KeyLength 3072 `
        -CertStoreLocation Cert:\CurrentUser\My `
        -NotAfter (Get-Date).AddYears(5) `
        -TextExtension @(
            '2.5.29.37={text}1.3.6.1.5.5.7.3.3',   # EKU: code signing
            '2.5.29.19={text}'                      # basic constraints: end entity
        )
    $cerPath = Join-Path $env:TEMP "zen-gate-signer.cer"
    Export-Certificate -Cert $cert -FilePath $cerPath -Force | Out-Null
    # Trust it locally so the chain validates; otherwise Authenticode reports
    # "a certificate chain processed, but terminated in a root certificate which
    # is not trusted" and SmartScreen still sees an unverifiable publisher.
    certutil -addstore -user -f Root $cerPath | Out-Null
    Import-Certificate -FilePath $cerPath -CertStoreLocation 'Cert:\CurrentUser\TrustedPublisher' | Out-Null
    Remove-Item $cerPath -Force -ErrorAction SilentlyContinue
}

Write-Host "signer: $($cert.Subject)  thumbprint=$($cert.Thumbprint)  expires=$($cert.NotAfter)"

$failed = 0
foreach ($p in $Path) {
    $full = (Resolve-Path $p).Path
    $sig = Set-AuthenticodeSignature -FilePath $full -Certificate $cert -HashAlgorithm SHA256 -TimestampServer $TimestampServer
    # NON-ZERO exit is the whole point: an untrusted chain must not pass silently,
    # because that is exactly the state that leaves SmartScreen blocking the exe.
    if ($sig.Status -ne "Valid") {
        Write-Host "FAIL $full -> $($sig.Status): $($sig.StatusMessage)"
        $failed++
    } else {
        Write-Host "OK   $full -> Valid"
    }
}

if ($failed -gt 0) { throw "$failed file(s) failed to sign" }
Write-Host "signed $($Path.Count) file(s)"
