param([switch]$UpdateReference)

$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
$taskOldUpdate = $env:TOROUTER_UPDATE_MCP_REFERENCE
Push-Location $taskRoot
try {
    if ($UpdateReference) { $env:TOROUTER_UPDATE_MCP_REFERENCE = '1' }
    else { Remove-Item Env:TOROUTER_UPDATE_MCP_REFERENCE -ErrorAction SilentlyContinue }
    go test ./internal/console -run '^TestMCPCoverage' -count=1
    if ($LASTEXITCODE -ne 0) { throw "MCP coverage/reference check failed (exit $LASTEXITCODE)." }
} finally {
    if ($null -eq $taskOldUpdate) { Remove-Item Env:TOROUTER_UPDATE_MCP_REFERENCE -ErrorAction SilentlyContinue }
    else { $env:TOROUTER_UPDATE_MCP_REFERENCE = $taskOldUpdate }
    Pop-Location
}
