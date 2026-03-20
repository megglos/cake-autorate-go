<#
.SYNOPSIS
    Generate sustained internet traffic to saturate a router's WAN link.

.DESCRIPTION
    Run this on a Windows PC connected to the router's LAN. Traffic flows
    through the router's WAN interfaces into the internet, triggering
    cake-autorate shaping.

    Downloads from / uploads to Cloudflare's speed test endpoints,
    which are fast, globally distributed, and don't require accounts.

.PARAMETER Duration
    Duration in seconds (default: 120)

.PARAMETER Mode
    Load mode: dl, ul, both (default: both)

.PARAMETER Workers
    Parallel workers per direction (default: 4)

.PARAMETER ChunkMB
    Download chunk size in MB (default: 100)

.EXAMPLE
    .\load-gen.ps1 -Duration 120 -Mode both -Workers 4
#>

param(
    [int]$Duration = 120,
    [ValidateSet("dl", "ul", "both")]
    [string]$Mode = "both",
    [int]$Workers = 4,
    [int]$ChunkMB = 100
)

$ErrorActionPreference = "Stop"

$ChunkBytes = $ChunkMB * 1000000
$DL_URL = "https://speed.cloudflare.com/__down?bytes=$ChunkBytes"
$UL_URL = "https://speed.cloudflare.com/__up"

function Log($msg) {
    Write-Host "[load-gen] [$(Get-Date -Format 'HH:mm:ss')] $msg"
}

Log "Mode: $Mode | Duration: ${Duration}s | Workers: $Workers per direction | Chunk: ${ChunkMB}MB"
Log "Download URL: $DL_URL"
Log "Upload URL:   $UL_URL"
Write-Host ""

# Generate a 1 MB random payload for upload workers
$uploadPayloadPath = [System.IO.Path]::GetTempFileName()
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$payloadBytes = New-Object byte[] (1024 * 1024)
$rng.GetBytes($payloadBytes)
[System.IO.File]::WriteAllBytes($uploadPayloadPath, $payloadBytes)
$rng.Dispose()

$jobs = @()

# Download worker script block
$dlBlock = {
    param($url, $durationSec)
    $deadline = [DateTime]::UtcNow.AddSeconds($durationSec + 30)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            Invoke-WebRequest -Uri $url -OutFile ([System.IO.Path]::GetTempFileName()) -UseBasicParsing -TimeoutSec ($durationSec + 30) -ErrorAction SilentlyContinue | Out-Null
        } catch {}
        # Clean up temp file
        Remove-Item ([System.IO.Path]::GetTempFileName()) -ErrorAction SilentlyContinue
    }
}

# Upload worker script block
$ulBlock = {
    param($url, $payloadPath, $durationSec)
    $deadline = [DateTime]::UtcNow.AddSeconds($durationSec + 30)
    $payload = [System.IO.File]::ReadAllBytes($payloadPath)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            Invoke-WebRequest -Uri $url -Method POST -Body $payload -ContentType "application/octet-stream" -UseBasicParsing -TimeoutSec ($durationSec + 30) -ErrorAction SilentlyContinue | Out-Null
        } catch {}
    }
}

# Start download workers
if ($Mode -eq "dl" -or $Mode -eq "both") {
    Log "Starting $Workers download workers..."
    for ($i = 1; $i -le $Workers; $i++) {
        $jobs += Start-Job -ScriptBlock $dlBlock -ArgumentList $DL_URL, $Duration
    }
}

# Start upload workers
if ($Mode -eq "ul" -or $Mode -eq "both") {
    Log "Starting $Workers upload workers..."
    for ($i = 1; $i -le $Workers; $i++) {
        $jobs += Start-Job -ScriptBlock $ulBlock -ArgumentList $UL_URL, $uploadPayloadPath, $Duration
    }
}

Log "Load generation running for ${Duration}s..."
Log "Press Ctrl+C to stop early."
Write-Host ""

# Register cleanup on Ctrl+C
$null = Register-EngineEvent -SourceIdentifier PowerShell.Exiting -Action {
    Get-Job | Stop-Job -PassThru | Remove-Job -Force
}

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
