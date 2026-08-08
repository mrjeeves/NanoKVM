# CEC Support installer launcher — ships on the KVM's USB drive.
#
# Fetches and runs the latest CEC Support installer instead of carrying it. The
# .exe is ~37 MB and, on the drive, would ride inside every over-the-air update
# for a file used once; this is a couple of KB. Pinning a version here would buy
# nothing either — the app updates itself on first run, so a pinned install is
# a stale install for about a minute.
#
# Run it with:
#   powershell -ExecutionPolicy Bypass -File D:\cecsupport.ps1
# or double-click CEC-Support.cmd next to it, which does exactly that. Double-
# clicking a .ps1 opens it in Notepad, which is not what anyone wants.

$ErrorActionPreference = 'Stop'

$Repo = 'mrjeeves/CECSupport'

Write-Host ""
Write-Host "  CEC Support" -ForegroundColor Cyan
Write-Host "  ----------------------------------------"
Write-Host ""

# Windows 8.1 and older Windows 10 default to TLS 1.0, which GitHub refuses.
# Without this every call below fails with a bare "underlying connection was
# closed" that says nothing about the real cause.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.SecurityProtocolType]::Tls12 -bor [Net.ServicePointManager]::SecurityProtocol
} catch { }

# Progress rendering makes Invoke-WebRequest roughly an order of magnitude
# slower on Windows PowerShell 5.1 for a download this size.
$ProgressPreference = 'SilentlyContinue'

function Fail($msg, $detail) {
    Write-Host ""
    Write-Host "  $msg" -ForegroundColor Red
    if ($detail) { Write-Host "  $detail" }
    Write-Host ""
    Write-Host "  This machine needs internet access to install CEC Support."
    Write-Host "  If it has none, install it on another machine and copy it over."
    Write-Host ""
    Read-Host "  Press Enter to close"
    exit 1
}

Write-Host "  Looking up the latest release..."
try {
    # The asset filename carries the version (CEC.Support_0.2.41_x64-setup.exe),
    # so GitHub's /releases/latest/download/<name> shortcut cannot address it —
    # the name is not knowable ahead of time. Hence the API.
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                                 -Headers @{ 'User-Agent' = 'CEC-KVM' } -UseBasicParsing
} catch {
    Fail "Could not reach GitHub to find the installer." $_.Exception.Message
}

$asset = $release.assets | Where-Object { $_.name -like '*_x64-setup.exe' } | Select-Object -First 1
if (-not $asset) {
    Fail "Release $($release.tag_name) has no x64 installer." "Assets: $(($release.assets | ForEach-Object { $_.name }) -join ', ')"
}

Write-Host "  Found $($release.tag_name)"
Write-Host "  Downloading $($asset.name)..."

$dest = Join-Path $env:TEMP $asset.name
try {
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $dest -UseBasicParsing
} catch {
    Fail "Could not download the installer." $_.Exception.Message
}

# GitHub reports each asset's digest. This is an integrity check, not a
# provenance one — the hash comes from the same place as the file, so it proves
# the download was not truncated or corrupted in transit, and nothing more. That
# is still the failure that actually happens on a flaky connection, and running
# a half-downloaded installer is worth refusing.
if ($asset.digest -and $asset.digest -like 'sha256:*') {
    Write-Host "  Verifying..."
    $expected = ($asset.digest -replace '^sha256:', '').ToUpper()
    $actual = (Get-FileHash -Path $dest -Algorithm SHA256).Hash
    if ($actual -ne $expected) {
        Remove-Item $dest -Force -ErrorAction SilentlyContinue
        Write-Host ""
        Write-Host "  Download is corrupted - refusing to run it." -ForegroundColor Red
        Write-Host "  expected $expected"
        Write-Host "  actual   $actual"
        Write-Host ""
        Read-Host "  Press Enter to close"
        exit 1
    }
}

Write-Host "  Starting the installer..."
Write-Host ""
$proc = Start-Process -FilePath $dest -PassThru -Wait
Remove-Item $dest -Force -ErrorAction SilentlyContinue

if ($proc.ExitCode -ne 0) {
    Write-Host "  Installer exited with code $($proc.ExitCode)." -ForegroundColor Yellow
    Read-Host "  Press Enter to close"
    exit $proc.ExitCode
}

Write-Host "  Done." -ForegroundColor Green
