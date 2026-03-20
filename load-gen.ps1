<#
.SYNOPSIS
    Generate sustained internet traffic to saturate a router's WAN link.

.DESCRIPTION
    Run this on a Windows PC connected to the router's LAN. Traffic flows
    through the router's WAN interfaces into the internet, triggering
    cake-autorate shaping.

    Download workers fetch large test files from well-known speed test servers
    (Hetzner, OVH, Tele2) that serve at full speed without throttling.
    Upload workers POST to Cloudflare's speed test upload endpoint.

    Uses curl.exe (shipped with Windows 10+) for maximum throughput.

.PARAMETER Duration
    Duration in seconds (default: 120)

.PARAMETER Mode
    Load mode: dl, ul, both (default: both)

.PARAMETER Workers
    Parallel workers per direction (default: 4)

.PARAMETER DlUrls
    Override download URLs (array of strings)

.PARAMETER UlUrl
    Override upload URL

.EXAMPLE
    .\load-gen.ps1 -Duration 120 -Mode both -Workers 4
#>

param(
    [int]$Duration = 120,
    [ValidateSet("dl", "ul", "both")]
    [string]$Mode = "both",
    [int]$Workers = 8,
    [string[]]$DlUrls = @(
        "https://speed.hetzner.de/1GB.bin",
        "https://ash-speed.hetzner.com/1GB.bin",
        "http://speedtest.tele2.net/1GB.zip",
        "http://proof.ovh.net/files/1Gio.dat",
        "http://speedtest.serverius.net/files/1000mb.bin",
        "http://fra-de-ping.vultr.com/vultr.com.1000MB.bin",
        "http://ams-nl-ping.vultr.com/vultr.com.1000MB.bin",
        "http://par-fr-ping.vultr.com/vultr.com.1000MB.bin"
    ),
    [string]$UlUrl = "https://speed.cloudflare.com/__up"
)

$ErrorActionPreference = "Stop"

# Verify curl.exe is available (ships with Windows 10 1803+)
$curl = Get-Command curl.exe -ErrorAction SilentlyContinue
if (-not $curl) {
    Write-Error "curl.exe not found. Install curl or upgrade to Windows 10 1803+."
    exit 1
}

function Log($msg) {
    Write-Host "[load-gen] [$(Get-Date -Format 'HH:mm:ss')] $msg"
}

Log "Mode: $Mode | Duration: ${Duration}s | Workers: $Workers per direction"
Log "Download URLs:"
foreach ($u in $DlUrls) { Log "  $u" }
Log "Upload URL: $UlUrl"
Write-Host ""

# Generate a 1 MB random payload file for upload workers
$uploadPayloadPath = Join-Path $env:TEMP "load-gen-payload.bin"
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$payloadBytes = New-Object byte[] (1024 * 1024)
$rng.GetBytes($payloadBytes)
[System.IO.File]::WriteAllBytes($uploadPayloadPath, $payloadBytes)
$rng.Dispose()

$jobs = @()
$maxTime = $Duration + 30

# Download worker: curl.exe fetching a large file in a loop
$dlBlock = {
    param($url, $durationSec, $maxTime)
    $deadline = [DateTime]::UtcNow.AddSeconds($durationSec)
    while ([DateTime]::UtcNow -lt $deadline) {
        & curl.exe -s -L -o NUL --max-time $maxTime $url 2>$null
    }
}

# Upload worker: curl.exe POSTing random payload in a loop
$ulBlock = {
    param($url, $payloadPath, $durationSec, $maxTime)
    $deadline = [DateTime]::UtcNow.AddSeconds($durationSec)
    while ([DateTime]::UtcNow -lt $deadline) {
        & curl.exe -s -o NUL --max-time $maxTime -X POST -H "Content-Type: application/octet-stream" --data-binary "@$payloadPath" $url 2>$null
    }
}

# Start download workers (round-robin across URLs)
if ($Mode -eq "dl" -or $Mode -eq "both") {
    Log "Starting $Workers download workers..."
    for ($i = 0; $i -lt $Workers; $i++) {
        $url = $DlUrls[$i % $DlUrls.Length]
        $jobs += Start-Job -ScriptBlock $dlBlock -ArgumentList $url, $Duration, $maxTime
    }
}

# Start upload workers
if ($Mode -eq "ul" -or $Mode -eq "both") {
    Log "Starting $Workers upload workers..."
    for ($i = 1; $i -le $Workers; $i++) {
        $jobs += Start-Job -ScriptBlock $ulBlock -ArgumentList $UlUrl, $uploadPayloadPath, $Duration, $maxTime
    }
}

Log "Load generation running for ${Duration}s..."
Log "Press Ctrl+C to stop early."
Write-Host ""

try {
    $elapsed = 0
    $interval = 10
    while ($elapsed -lt $Duration) {
        $remaining = $Duration - $elapsed
        $sleepTime = [Math]::Min($interval, $remaining)
        Start-Sleep -Seconds $sleepTime
        $elapsed += $sleepTime

        $active = ($jobs | Where-Object { $_.State -eq "Running" }).Count
        Log "${elapsed}/${Duration}s elapsed, $active workers active"
    }
} finally {
    Write-Host ""
    Log "Stopping all workers..."
    $jobs | Stop-Job -PassThru | Remove-Job -Force -ErrorAction SilentlyContinue
    Remove-Item $uploadPayloadPath -ErrorAction SilentlyContinue
    Log "Done."
}
