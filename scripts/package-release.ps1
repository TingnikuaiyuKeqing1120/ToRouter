# scripts/package-release.ps1: Build, sign and package official ToRouter release artifacts.
param(
    [Parameter(Mandatory = $true)]
    [string]$Version,

    [string]$Repo = "TingnikuaiyuKeqing1120/ToRouter",
    [string]$PrivateKeyFile = ".local/release/update-private.key",
    [string]$PublicKey = "",
    [string]$ReleaseNotesFile = "",
    [string]$OutputDir = "",
    [switch]$UsePrebuiltWeb
)

$ErrorActionPreference = "Stop"

# 1. Normalize version
$cleanVersion = $Version.Trim()
if ($cleanVersion -notmatch '^v?\d+\.\d+\.\d+$') {
    throw "Version must be a numeric major.minor.patch (e.g. 1.0.0 or v1.0.0)."
}
$tag = if ($cleanVersion.StartsWith("v")) { $cleanVersion } else { "v$cleanVersion" }

# 2. Check and load keys
$root = $PSScriptRoot | Split-Path -Parent
if (-not [IO.Path]::IsPathRooted($PrivateKeyFile)) {
    $PrivateKeyFile = Join-Path $root $PrivateKeyFile
}
if (-not (Test-Path $PrivateKeyFile -PathType Leaf)) {
    throw "Private key file not found: $PrivateKeyFile. Run 'go run ./scripts/release-keygen' to generate one."
}

if (-not $PublicKey) {
    $pubPath = Join-Path (Split-Path $PrivateKeyFile) "update-public.key"
    if (Test-Path $pubPath -PathType Leaf) {
        $PublicKey = (Get-Content $pubPath -Raw).Trim()
    } else {
        throw "UpdatePublicKey not provided and not found at $pubPath."
    }
}

# 3. Resolve output directory
if (-not $OutputDir) {
    $OutputDir = Join-Path $root ".local/dist/$tag"
} elseif (-not [IO.Path]::IsPathRooted($OutputDir)) {
    $OutputDir = Join-Path $root $OutputDir
}
New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

$feedUrl = "https://github.com/$Repo/releases/latest/download/manifest.json"
$artifactUrl = "https://github.com/$Repo/releases/download/$tag/ToRouter.exe"

Write-Host "=========================================="
Write-Host " Packaging ToRouter Release $tag"
Write-Host " Output Directory: $OutputDir"
Write-Host " Feed URL:         $feedUrl"
Write-Host " Artifact URL:     $artifactUrl"
Write-Host " Public Key:       $PublicKey"
Write-Host "=========================================="

# 4. Build executable
Push-Location $root
try {
    $buildArgs = @(
        "-ExecutionPolicy", "Bypass",
        "-File", (Join-Path $root "build.ps1"),
        "-OutputDir", $OutputDir,
        "-Version", $cleanVersion,
        "-UpdateFeedURL", $feedUrl,
        "-UpdatePublicKey", $PublicKey
    )
    if ($UsePrebuiltWeb) {
        $buildArgs += "-UsePrebuiltWeb"
    }

    Write-Host "==> Compiling production ToRouter.exe with update feed configured..."
    & powershell @buildArgs
    if ($LASTEXITCODE -ne 0) { throw "Build failed (exit $LASTEXITCODE)." }

    $exePath = Join-Path $OutputDir "ToRouter.exe"
    if (-not (Test-Path $exePath -PathType Leaf)) {
        throw "Expected binary missing at $exePath."
    }

    # 5. Sign manifest
    $manifestPath = Join-Path $OutputDir "manifest.json"
    if (Test-Path $manifestPath) { Remove-Item $manifestPath -Force }

    $signArgs = @(
        "run", "./scripts/release-manifest",
        "-exe", $exePath,
        "-version", $cleanVersion,
        "-os", "windows",
        "-arch", "amd64",
        "-url", $artifactUrl,
        "-key", $PrivateKeyFile,
        "-output", $manifestPath
    )
    if ($ReleaseNotesFile -and (Test-Path $ReleaseNotesFile)) {
        $signArgs += @("-notes", $ReleaseNotesFile)
    }

    Write-Host "==> Signing release manifest..."
    & go @signArgs
    if ($LASTEXITCODE -ne 0) { throw "Manifest signing failed (exit $LASTEXITCODE)." }

    # 6. Copy documentation and licenses
    Copy-Item (Join-Path $root "LICENSE") -Destination $OutputDir -Force
    Copy-Item (Join-Path $root "THIRD_PARTY_NOTICES.md") -Destination $OutputDir -Force

    # 7. Compute SHA256 checksums
    $sumsFile = Join-Path $OutputDir "SHA256SUMS.txt"
    $filesToHash = @("ToRouter.exe", "manifest.json", "LICENSE", "THIRD_PARTY_NOTICES.md")
    $sumLines = @()
    foreach ($item in $filesToHash) {
        $filePath = Join-Path $OutputDir $item
        if (Test-Path $filePath) {
            $hash = (Get-FileHash -Algorithm SHA256 -Path $filePath).Hash.ToLowerInvariant()
            $sumLines += "$hash  $item"
        }
    }
    Set-Content -Path $sumsFile -Value ($sumLines -join "`n") -Encoding utf8

    Write-Host "`n==> Release packaging completed successfully!"
    Write-Host "Artifacts in $($OutputDir):"
    Get-ChildItem $OutputDir | Select-Object Name, Length, LastWriteTime | Format-Table -AutoSize

    Write-Host "Next steps for GitHub Release:"
    Write-Host "  1. Create tag and release: $tag"
    Write-Host "  2. Upload all files from $OutputDir as Release Assets:"
    Write-Host "     - ToRouter.exe"
    Write-Host "     - manifest.json (Signed update feed)"
    Write-Host "     - SHA256SUMS.txt"
    Write-Host "     - LICENSE & THIRD_PARTY_NOTICES.md"
} finally {
    Pop-Location
}
