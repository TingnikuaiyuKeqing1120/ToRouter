# to-router build script: current frontend and one full-featured ToRouter.exe.
param(
    [switch]$UsePrebuiltWeb,
    [string]$OutputDir = $PSScriptRoot,
    [string]$Version = 'dev',
    [string]$Commit = '',
    [string]$BuildTime = '',
    [string]$UpdateFeedURL = '',
    [string]$UpdatePublicKey = ''
)

$ErrorActionPreference = "Stop"
if ($Version -ne 'dev' -and $Version -notmatch '^v?\d+\.\d+\.\d+$') { throw 'Version must be dev or a numeric major.minor.patch version.' }
if ($Commit -and $Commit -notmatch '^[0-9a-fA-F]{7,64}$') { throw 'Commit must be an explicit public source revision.' }
if (-not $BuildTime) { $BuildTime = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ') }
if ($BuildTime -match '[\s"''\x00-\x1f]') { throw 'BuildTime must not contain whitespace or quoting characters.' }
if ([bool]$UpdateFeedURL -ne [bool]$UpdatePublicKey) { throw 'UpdateFeedURL and UpdatePublicKey must be supplied together.' }
if ($UpdateFeedURL) {
    if ($Version -eq 'dev') { throw 'An update-enabled build requires a numeric Version.' }
    $feedUri = $null
    if (-not [Uri]::TryCreate($UpdateFeedURL, [UriKind]::Absolute, [ref]$feedUri) -or
        $UpdateFeedURL -match '[\s"''\x00-\x1f]' -or $feedUri.UserInfo -or $feedUri.Fragment -or
        ($feedUri.Scheme -ne 'https' -and -not ($feedUri.Scheme -eq 'http' -and $feedUri.IsLoopback))) {
        throw 'UpdateFeedURL must be HTTPS; HTTP loopback is allowed only for an isolated test feed.'
    }
    try { $verificationBytes = [Convert]::FromBase64String($UpdatePublicKey) } catch { throw 'Invalid update verification key.' }
    if ($verificationBytes.Length -ne 32 -or $UpdatePublicKey -match '\s') { throw 'UpdatePublicKey must be a base64 Ed25519 public key.' }
}
Push-Location $PSScriptRoot
try {
    Write-Host "==> checking MCP coverage and generated reference..."
    & (Join-Path $PSScriptRoot 'scripts/check-mcp.ps1')
    if (-not $UsePrebuiltWeb) {
        Write-Host "==> building web console..."
        Push-Location web
        try {
            if (-not (Test-Path "node_modules")) {
                npm ci
                if ($LASTEXITCODE -ne 0) { throw "npm ci failed (exit $LASTEXITCODE)" }
            }
            if (-not (Test-Path "dist/index.html")) {
                New-Item -ItemType Directory -Force "dist" | Out-Null
                Set-Content "dist/index.html" "<!-- temporary stub for go embed -->"
            }
            python ../scripts/check-third-party.py
            if ($LASTEXITCODE -ne 0) { throw "Third-party notice generation failed (exit $LASTEXITCODE)" }
            npm run build
            if ($LASTEXITCODE -ne 0) { throw "Frontend build failed (exit $LASTEXITCODE)" }
        } finally { Pop-Location }
    } else {
        python scripts/check-third-party.py --check
        if ($LASTEXITCODE -ne 0) { throw "Third-party notices are stale (exit $LASTEXITCODE)" }
        Write-Host "==> explicitly reusing prebuilt web/dist"
    }

    if (-not (Test-Path "web/dist/index.html" -PathType Leaf) -or
        -not (Get-ChildItem "web/dist/assets" -Filter '*.js' -File -ErrorAction SilentlyContinue)) {
        throw "web/dist is incomplete; run the default build to regenerate frontend assets."
    }
    $noticesSource = Join-Path $PSScriptRoot 'web/public/third-party'
    $noticesDist = Join-Path $PSScriptRoot 'web/dist/third-party'
    $noticesPrefix = $noticesSource + [IO.Path]::DirectorySeparatorChar
    $expectedNotices = @(Get-ChildItem -LiteralPath $noticesSource -File -Recurse)
    $actualNotices = @(Get-ChildItem -LiteralPath $noticesDist -File -Recurse -ErrorAction SilentlyContinue)
    if ($expectedNotices.Count -ne $actualNotices.Count) { throw 'Bundled third-party notice files differ from current source; rebuild the frontend.' }
    foreach ($notice in $expectedNotices) {
        if (-not $notice.FullName.StartsWith($noticesPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unexpected notice source path.' }
        $relativeNotice = $notice.FullName.Substring($noticesPrefix.Length)
        $bundledNotice = Join-Path $noticesDist $relativeNotice
        if (-not (Test-Path -LiteralPath $bundledNotice -PathType Leaf) -or
            (Get-FileHash -LiteralPath $notice.FullName -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath $bundledNotice -Algorithm SHA256).Hash) {
            throw 'Bundled third-party notices are stale; rebuild the frontend.'
        }
    }
    Get-ChildItem "web/dist" -Recurse -File | Sort-Object FullName | ForEach-Object {
        $assetPath = $_.FullName.Substring($PSScriptRoot.Length + 1)
        $assetHash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        Write-Host "==> frontend asset: $assetPath sha256=$assetHash"
    }

    $resolvedOutput = if ([IO.Path]::IsPathRooted($OutputDir)) {
        [IO.Path]::GetFullPath($OutputDir)
    } else {
        [IO.Path]::GetFullPath((Join-Path $PWD $OutputDir))
    }
    New-Item -ItemType Directory -Path $resolvedOutput -Force | Out-Null
    $appOutput = Join-Path $resolvedOutput 'ToRouter.exe'

    # Ensure Windows executable icon resources exist in cmd directories
    $iconSource = Join-Path $PSScriptRoot "web/public/favicon.ico"
    $cmdDirs = @("cmd/torouter")
    if (Test-Path $iconSource -PathType Leaf) {
        $windresCmd = Get-Command windres -ErrorAction SilentlyContinue
        foreach ($dir in $cmdDirs) {
            $sysoPath = Join-Path $PSScriptRoot "$dir/icon_windows_amd64.syso"
            if ((-not (Test-Path $sysoPath -PathType Leaf)) -and $windresCmd) {
                Write-Host "==> generating Windows icon resource for $dir..."
                $tempRc = [IO.Path]::GetTempFileName() + ".rc"
                try {
                    $icoEscaped = $iconSource -replace '\\', '/'
                    Set-Content -Path $tempRc -Value "1 ICON `"$icoEscaped`"" -Encoding ASCII
                    & $windresCmd.Source -i $tempRc -O coff -o $sysoPath
                } finally {
                    Remove-Item $tempRc -ErrorAction SilentlyContinue
                }
            }
        }
    }

    Write-Host "==> building ToRouter.exe (Web, Desktop, CLI, pet and MCP)..."
    $buildFlags = "-s -w -H windowsgui -X torouter/internal/buildinfo.Version=$Version -X torouter/internal/buildinfo.Commit=$Commit -X torouter/internal/buildinfo.BuildTime=$BuildTime -X torouter/internal/buildinfo.UpdateFeedURL=$UpdateFeedURL -X torouter/internal/buildinfo.UpdatePublicKey=$UpdatePublicKey"
    go build -tags production -trimpath -ldflags $buildFlags -o $appOutput ./cmd/torouter
    if ($LASTEXITCODE -ne 0) { throw "ToRouter build failed (exit $LASTEXITCODE)" }

    Write-Host "==> done: ToRouter.exe"

} finally { Pop-Location }
