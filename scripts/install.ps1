<#
.SYNOPSIS
  Universal Windows installer for svc.plus / ai-workspace-lab applications.

.DESCRIPTION
  Installs XConnect or XWorkmate on Windows via Scoop, WinGet, MSI, EXE, or ZIP
  with automatic CN vs Global mirror selection and SHA-256 verification.

.EXAMPLE
  .\install.ps1 -Product xconnect -Mirror auto -Method auto

.EXAMPLE
  .\install.ps1 -Product xworkmate -Mirror cn -Method scoop -SetupRepo -DryRun
#>
[CmdletBinding()]
param(
    [ValidateSet("xconnect", "xconnect-app", "xconnect-one", "xworkmate", "xworkmate-remote-desktop")]
    [string]$Product = "xconnect",

    [ValidateSet("auto", "cn", "global")]
    [string]$Mirror = "auto",

    [ValidateSet("auto", "scoop", "msi", "exe", "winget", "zip")]
    [string]$Method = "auto",

    [ValidateSet("latest", "stable")]
    [string]$Channel = "latest",

    [switch]$SetupRepo,

    [switch]$DryRun,

    [switch]$Help
)

$ErrorActionPreference = "Stop"

function Show-Usage {
    @"
svc.plus Universal Windows Installer (PowerShell)

Usage:
  install.ps1 [-Product <xconnect|xworkmate>] [-Mirror <auto|cn|global>]
              [-Method <auto|scoop|msi|exe|winget|zip>] [-Channel <latest|stable>]
              [-SetupRepo] [-DryRun] [-Help]

Options:
  -Product    Product to install (xconnect | xworkmate, default: xconnect)
  -Mirror     Mirror selection (auto | cn | global, default: auto)
  -Method     Install method (auto | scoop | msi | exe | winget | zip, default: auto)
  -Channel    Release channel (latest | stable, default: latest)
  -SetupRepo  Configure Scoop bucket (svc-plus) without forcing install when -Method auto
  -DryRun     Print planned actions without making system changes
  -Help       Show this help message
"@ | Write-Host
}

if ($Help) {
    Show-Usage
    exit 0
}

function Test-MirrorHealth {
    param([string]$Url)
    try {
        $resp = Invoke-WebRequest -Uri $Url -Method Get -TimeoutSec 3 -UseBasicParsing
        return ($resp.StatusCode -ge 200 -and $resp.StatusCode -lt 300)
    } catch {
        return $false
    }
}

function Resolve-Mirror {
    param([string]$Requested)
    if ($Requested -ne "auto") {
        return $Requested
    }
    if ($DryRun) {
        return "global"
    }
    $tz = (Get-TimeZone).Id
    if ($tz -match "China Standard Time|Asia/Shanghai" -and (Test-MirrorHealth "https://cn-repo.svc.plus/repo/_health/probe.json")) {
        return "cn"
    }
    if (Test-MirrorHealth "https://repo.svc.plus/repo/_health/probe.json") {
        return "global"
    }
    if (Test-MirrorHealth "https://cn-repo.svc.plus/repo/_health/probe.json") {
        return "cn"
    }
    return "global"
}

$ResolvedMirror = Resolve-Mirror -Requested $Mirror
if ($ResolvedMirror -eq "cn") {
    $RepoBase = "https://cn-repo.svc.plus"
    $DlBase = "https://cn-dl.svc.plus"
    $ScoopBucketUrl = "https://cn-repo.svc.plus/scoop/bucket.git"
} else {
    $RepoBase = "https://repo.svc.plus"
    $DlBase = "https://dl.svc.plus"
    $ScoopBucketUrl = "https://github.com/ai-workspace-lab/scoop-bucket"
}

Write-Host "[svc-plus] product=$Product channel=$Channel mirror=$ResolvedMirror repo=$RepoBase dl=$DlBase"

function Invoke-Action {
    param([string]$Description, [scriptblock]$Script)
    if ($DryRun) {
        Write-Host "[dry-run] $Description"
    } else {
        & $Script
    }
}

function Configure-ScoopBucket {
    Invoke-Action "scoop bucket add svc-plus $ScoopBucketUrl" {
        $buckets = (scoop bucket list 2>$null | Out-String)
        if ($buckets -notmatch "svc-plus") {
            scoop bucket add svc-plus $ScoopBucketUrl
        } else {
            scoop bucket rm svc-plus
            scoop bucket add svc-plus $ScoopBucketUrl
        }
    }
}

if ($SetupRepo) {
    if (Get-Command scoop -ErrorAction SilentlyContinue) {
        Configure-ScoopBucket
    } else {
        Write-Host "[svc-plus] Scoop not found; WinGet manifests are available under $RepoBase/winget/"
    }
    if ($Method -eq "auto") {
        Write-Host "[svc-plus] Repository setup complete."
        exit 0
    }
}

$EffectiveMethod = $Method
if ($EffectiveMethod -eq "auto") {
    if (Get-Command scoop -ErrorAction SilentlyContinue) {
        $EffectiveMethod = "scoop"
    } elseif (Get-Command winget -ErrorAction SilentlyContinue) {
        $EffectiveMethod = "winget"
    } else {
        $EffectiveMethod = "msi"
    }
}

function Get-MatrixAsset {
    param([string[]]$PreferredFormats)
    $candidates = @(
        "$DlBase/releases/$Product/release-matrix.json",
        "https://dl.svc.plus/releases/$Product/release-matrix.json"
    )
    $matrix = $null
    foreach ($url in $candidates) {
        try {
            $matrix = Invoke-RestMethod -Uri $url -TimeoutSec 10
            if ($matrix) { break }
        } catch {
            continue
        }
    }
    if (-not $matrix) {
        throw "Failed to fetch release-matrix.json for $Product."
    }
    $entry = $matrix.channels.$Channel
    if (-not $entry) { $entry = $matrix.channels.($matrix.defaultChannel) }
    if (-not $entry) { $entry = $matrix.channels.latest }
    if (-not $entry) { $entry = $matrix.channels.stable }
    if (-not $entry) { throw "No release entry found for channel '$Channel'." }

    $win = $entry.platforms | Where-Object { $_.id -eq "windows" } | Select-Object -First 1
    if (-not $win) { throw "No Windows platform entries found for $Product." }

    foreach ($fmt in $PreferredFormats) {
        $pkg = $win.packages | Where-Object { $_.format -eq $fmt } | Select-Object -First 1
        if ($pkg) {
            $cnSynced = [bool]$entry.mirrors.cn.synced
            $globalSynced = [bool]$entry.mirrors.global.synced
            $downloadUrl = if ($ResolvedMirror -eq "cn" -and $cnSynced -and $pkg.path) {
                "$DlBase$($pkg.path)"
            } elseif ($globalSynced -and $pkg.path) {
                "https://dl.svc.plus$($pkg.path)"
            } else {
                $pkg.githubUrl
            }
            return [pscustomobject]@{
                Name   = $pkg.name
                Format = $pkg.format
                Sha256 = $pkg.sha256
                Url    = $downloadUrl
            }
        }
    }
    throw "No matching Windows package format ($($PreferredFormats -join ', ')) found for $Product."
}

switch ($EffectiveMethod) {
    "scoop" {
        Configure-ScoopBucket
        Invoke-Action "scoop install svc-plus/$Product" {
            scoop install "svc-plus/$Product"
        }
    }
    "winget" {
        $wingetId = if ($Product -eq "xconnect") { "SvcPlus.XConnect" } else { "SvcPlus.XWorkmate" }
        Invoke-Action "winget install --id $wingetId --accept-source-agreements --accept-package-agreements" {
            winget install --id $wingetId --accept-source-agreements --accept-package-agreements
        }
    }
    { $_ -in @("msi", "exe", "zip") } {
        if ($DryRun) {
            Write-Host "[dry-run] Would query $DlBase/releases/$Product/release-matrix.json for format '$EffectiveMethod', verify SHA-256, and install."
            break
        }
        $formats = if ($EffectiveMethod -eq "msi") { @("msi", "exe", "zip") } elseif ($EffectiveMethod -eq "exe") { @("exe", "msi", "zip") } else { @("zip") }
        $asset = Get-MatrixAsset -PreferredFormats $formats
        $tmpFile = Join-Path $env:TEMP $asset.Name
        Write-Host "[svc-plus] Downloading $($asset.Url)"
        Invoke-WebRequest -Uri $asset.Url -OutFile $tmpFile -UseBasicParsing
        if ($asset.Sha256) {
            $actual = (Get-FileHash -Path $tmpFile -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actual -ne $asset.Sha256.ToLowerInvariant()) {
                throw "SHA-256 mismatch for $($asset.Name): expected $($asset.Sha256), got $actual"
            }
            Write-Host "[svc-plus] SHA-256 verified: $actual"
        }
        if ($asset.Format -eq "msi") {
            Start-Process -FilePath "msiexec.exe" -ArgumentList "/i `"$tmpFile`" /qn /norestart" -Wait
        } elseif ($asset.Format -eq "exe") {
            Start-Process -FilePath $tmpFile -ArgumentList "/S" -Wait
        } elseif ($asset.Format -eq "zip") {
            $destDir = Join-Path $env:LOCALAPPDATA "Programs\$Product"
            Expand-Archive -Path $tmpFile -DestinationPath $destDir -Force
            Write-Host "[svc-plus] Extracted to $destDir"
        }
    }
}

Write-Host "[svc-plus] Done."
