# Isolated orchestration regression. Tool doubles prove ordering/failure handling;
# they do not prove that npm/Go can build a working binary.
$ErrorActionPreference = 'Stop'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('torouter-build-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $testRoot | Out-Null
$buildScript = Join-Path (Split-Path $PSScriptRoot -Parent) 'build.ps1'
$shellPath = (Get-Process -Id $PID).Path
$passed = 0
try {
    foreach ($scenario in @('default', 'prebuilt', 'install-failure', 'web-failure', 'go-failure', 'missing-prebuilt', 'mcp-failure', 'notice-generation-failure', 'notice-check-failure', 'prebuilt-notice-drift', 'prebuilt-notice-missing', 'prebuilt-notice-extra', 'prebuilt-notice-path')) {
        $caseDir = Join-Path $testRoot $scenario
        New-Item -ItemType Directory -Path "$caseDir/web/dist/assets" -Force | Out-Null
        Copy-Item -LiteralPath $buildScript -Destination "$caseDir/build.ps1"
        Set-Content -LiteralPath "$caseDir/web/dist/index.html" -Value '<script src="/assets/old.js"></script>'
        Set-Content -LiteralPath "$caseDir/web/dist/assets/old.js" -Value 'OLD_UI'
        Set-Content -LiteralPath "$caseDir/web/package-lock.json" -Value '{}'
        New-Item -ItemType Directory -Path "$caseDir/scripts", "$caseDir/web/public/third-party/nested", "$caseDir/web/dist/third-party/nested" -Force | Out-Null
        Set-Content -LiteralPath "$caseDir/web/public/third-party/index.txt" -Value 'SYNTHETIC_NOTICE'
        Set-Content -LiteralPath "$caseDir/web/public/third-party/nested/license.txt" -Value 'SYNTHETIC_LICENSE'
        Copy-Item -Path "$caseDir/web/public/third-party/*" -Destination "$caseDir/web/dist/third-party" -Recurse -Force
        Set-Content -LiteralPath "$caseDir/scripts/check-third-party.py" -Value '# Synthetic fixture; the python function records orchestration only.'
        $mcpCheck = @'
Add-Content -LiteralPath $global:TestTrace -Value 'mcp check'
if ($global:TestScenario -eq 'mcp-failure') { throw 'Synthetic MCP check failure' }
'@
        Set-Content -LiteralPath "$caseDir/scripts/check-mcp.ps1" -Value $mcpCheck
        if ($scenario -ne 'install-failure') { New-Item -ItemType Directory -Path "$caseDir/web/node_modules" | Out-Null }
        if ($scenario -eq 'missing-prebuilt') { Remove-Item -LiteralPath "$caseDir/web/dist/index.html" }
        if ($scenario -eq 'prebuilt-notice-drift') { Set-Content -LiteralPath "$caseDir/web/dist/third-party/nested/license.txt" -Value 'STALE_SYNTHETIC_LICENSE' }
        if ($scenario -eq 'prebuilt-notice-missing') { Remove-Item -LiteralPath "$caseDir/web/dist/third-party/nested/license.txt" }
        if ($scenario -eq 'prebuilt-notice-extra') { Set-Content -LiteralPath "$caseDir/web/dist/third-party/extra.txt" -Value 'EXTRA_SYNTHETIC_NOTICE' }
        if ($scenario -eq 'prebuilt-notice-path') { Move-Item -LiteralPath "$caseDir/web/dist/third-party/nested/license.txt" -Destination "$caseDir/web/dist/third-party/renamed.txt" }
        $harness = @'
param([string]$Scenario)
$global:TestScenario = $Scenario
$global:TestTrace = Join-Path $PSScriptRoot 'trace.txt'
function global:npm {
    Add-Content -LiteralPath $global:TestTrace -Value ('npm ' + ($args -join ' '))
    $global:LASTEXITCODE = 0
    if ($global:TestScenario -eq 'install-failure') { $global:LASTEXITCODE = 17; return }
    if ($args[0] -eq 'run') {
        if ($global:TestScenario -eq 'web-failure') { $global:LASTEXITCODE = 18; return }
        Set-Content -LiteralPath (Join-Path $PWD 'dist/assets/old.js') -Value 'CURRENT_UI'
        Copy-Item -Path (Join-Path $PWD 'public/third-party/*') -Destination (Join-Path $PWD 'dist/third-party') -Recurse -Force
    }
}
function global:python {
    Add-Content -LiteralPath $global:TestTrace -Value ('python ' + ($args -join ' '))
    $global:LASTEXITCODE = 0
    if ($global:TestScenario -eq 'notice-generation-failure') { $global:LASTEXITCODE = 20; return }
    if ($global:TestScenario -eq 'notice-check-failure') { $global:LASTEXITCODE = 21; return }
    if ($args -notcontains '--check') {
        Set-Content -LiteralPath (Join-Path $PSScriptRoot 'web/public/third-party/index.txt') -Value 'CURRENT_SYNTHETIC_NOTICE'
    }
}
function global:go {
    Add-Content -LiteralPath $global:TestTrace -Value ('go ' + ($args -join ' '))
    $global:LASTEXITCODE = 0
    if ($global:TestScenario -eq 'go-failure') { $global:LASTEXITCODE = 19; return }
    $outputIndex = [array]::IndexOf($args, '-o') + 1
    Set-Content -LiteralPath $args[$outputIndex] -Value (Get-Content -LiteralPath (Join-Path $PSScriptRoot 'web/dist/assets/old.js') -Raw)
}
if ($Scenario -in @('prebuilt', 'missing-prebuilt', 'notice-check-failure', 'prebuilt-notice-drift', 'prebuilt-notice-missing', 'prebuilt-notice-extra', 'prebuilt-notice-path')) {
    & "$PSScriptRoot/build.ps1" -UsePrebuiltWeb -OutputDir (Join-Path $PSScriptRoot 'out')
} else {
    & "$PSScriptRoot/build.ps1" -OutputDir (Join-Path $PSScriptRoot 'out')
}
'@
        Set-Content -LiteralPath "$caseDir/harness.ps1" -Value $harness
        # Windows PowerShell converts child stderr to ErrorRecord objects;
        # expected failures must be captured, not terminate the test runner.
        $ErrorActionPreference = 'Continue'
        try {
            $output = (& $shellPath -NoProfile -ExecutionPolicy Bypass -File "$caseDir/harness.ps1" $scenario 2>&1 | Out-String)
            $exitCode = $LASTEXITCODE
        } finally { $ErrorActionPreference = 'Stop' }
        $trace = @(if (Test-Path "$caseDir/trace.txt") { Get-Content -LiteralPath "$caseDir/trace.txt" })
        if ($trace.Count -lt 1 -or $trace[0] -ne 'mcp check') { throw "$scenario must check MCP before any build tool: $trace" }
        $pythonTrace = @($trace | Where-Object { $_ -like 'python *' })
        $prebuilt = $scenario -in @('prebuilt', 'missing-prebuilt', 'notice-check-failure', 'prebuilt-notice-drift', 'prebuilt-notice-missing', 'prebuilt-notice-extra', 'prebuilt-notice-path')
        if ($scenario -in @('mcp-failure', 'install-failure')) {
            if ($pythonTrace.Count -ne 0) { throw "$scenario must stop before notice generation." }
        } else {
            $expectedPython = if ($prebuilt) { 'python scripts/check-third-party.py --check' } else { 'python ../scripts/check-third-party.py' }
            if ($pythonTrace.Count -ne 1 -or $pythonTrace[0] -ne $expectedPython) { throw "$scenario used an unexpected notice command: $pythonTrace" }
            if ($trace[1] -ne $expectedPython) { throw "$scenario must check or generate notices immediately after MCP when dependencies exist: $trace" }
        }
        if ($prebuilt -and @($trace | Where-Object { $_ -like 'npm *' }).Count -ne 0) { throw "$scenario must skip npm in prebuilt mode." }
        if ($scenario -in @('default', 'prebuilt')) {
            if ($exitCode -ne 0) { throw "$scenario unexpectedly failed: $output" }
            if ($scenario -eq 'default' -and ($trace[2] -ne 'npm run build' -or $trace[3] -notlike 'go *')) { throw 'Default build must generate notices and rebuild existing dist before Go.' }
            if ($scenario -eq 'prebuilt' -and $trace[2] -notlike 'go *') { throw 'Prebuilt build must check notices before Go.' }
            $goTrace = @($trace | Where-Object { $_ -like 'go *' })
            if ($goTrace.Count -ne 1 -or $goTrace[0] -notlike 'go build -tags production -trimpath -ldflags *-H windowsgui* ./cmd/torouter') { throw 'Build must compile one production WindowsGUI executable with trimpath.' }
            $executables = @(Get-ChildItem -LiteralPath "$caseDir/out" -File -Filter '*.exe')
            if ($executables.Count -ne 1 -or $executables[0].Name -ne 'ToRouter.exe') { throw 'Build must emit only ToRouter.exe.' }
            $expected = if ($scenario -eq 'default') { 'CURRENT_UI' } else { 'OLD_UI' }
            foreach ($binary in @('ToRouter.exe')) {
                $path = "$caseDir/out/$binary"
                if (-not (Test-Path $path) -or (Get-Content -LiteralPath $path -Raw).Trim() -ne $expected) { throw "$scenario did not package $expected into isolated $binary; trace: $trace" }
            }
            if ($scenario -eq 'prebuilt' -and @($trace | Where-Object { $_ -like 'npm *' }).Count -ne 0) { throw 'Explicit prebuilt mode must skip npm.' }
            $expectedNotice = if ($scenario -eq 'default') { 'CURRENT_SYNTHETIC_NOTICE' } else { 'SYNTHETIC_NOTICE' }
            if ((Get-Content -LiteralPath "$caseDir/web/dist/third-party/index.txt" -Raw).Trim() -ne $expectedNotice -or (Get-Content -LiteralPath "$caseDir/web/dist/third-party/nested/license.txt" -Raw).Trim() -ne 'SYNTHETIC_LICENSE') { throw "$scenario did not bundle the expected synthetic notices." }
            if ($output -notmatch 'sha256=') { throw 'Build must identify its embedded web asset hashes.' }
        } else {
            if ($exitCode -eq 0 -or $output -match '==> done:') { throw "$scenario falsely reported success: $output" }
            if ($scenario -ne 'go-failure' -and @($trace | Where-Object { $_ -like 'go *' }).Count -ne 0) { throw "$scenario continued into Go after invalid frontend inputs." }
            if ($scenario -eq 'go-failure' -and @($trace | Where-Object { $_ -like 'go *' }).Count -ne 1) { throw 'Unified Go build failure must stop immediately.' }
            if ($scenario -eq 'install-failure' -and @($trace | Where-Object { $_ -eq 'npm run build' }).Count -ne 0) { throw 'Install failure must stop before frontend build.' }
            if ($scenario -eq 'mcp-failure' -and $trace.Count -ne 1) { throw 'MCP failure must stop before all build tools.' }
            if ($scenario -eq 'notice-generation-failure' -and @($trace | Where-Object { $_ -like 'npm *' }).Count -ne 0) { throw 'Notice generation failure must stop before frontend build.' }
            $expectedFailure = switch ($scenario) {
                'notice-generation-failure' { 'Third-party notice generation failed' }
                'notice-check-failure' { 'Third-party notices are stale' }
                'prebuilt-notice-drift' { 'Bundled third-party notices are stale' }
                'prebuilt-notice-missing' { 'Bundled third-party notice files differ' }
                'prebuilt-notice-extra' { 'Bundled third-party notice files differ' }
                'prebuilt-notice-path' { 'Bundled third-party notices are stale' }
            }
            if ($expectedFailure -and $output -notmatch [regex]::Escape($expectedFailure)) { throw "$scenario failed at an unexpected gate: $output" }
        }
        $passed++
        Write-Host "PASS $scenario"
    }
    Write-Host "$passed build-script scenarios passed."
} finally {
    # The absolute root is a dedicated directory created by this test only.
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if ($resolvedRoot.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $resolvedRoot -Leaf) -like 'torouter-build-test-*') {
        Remove-Item -LiteralPath $resolvedRoot -Recurse -Force
    } else { throw 'Refusing cleanup outside the dedicated absolute test directory.' }
}
