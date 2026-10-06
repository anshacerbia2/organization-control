# What deploy/dev/README.md §Wiring step 7 says an operator should see, asserted against the three
# stacks scripts/dev-wire.ps1 wired (TDD-identity-control-006, ADR-ORG-002 §5.2, §5.3):
#
#   1. Organization's provider:identity-control grant reaches identity-control's projection, and the
#      projection reads fresh;
#   2. the ceremony's grant is retired: the operator's requests to identity-control are now
#      authorized by the projected emergency grant, whose use identity-control records;
#   3. the operator's requests here were authorized by the bootstrap emergency grant, whose use this
#      service records;
#   4. with Organization Control stopped, identity-control reads the projection stale and still
#      honors the emergency grant; started again, the projection reads fresh.
#
# Environment and SECRETS: as dev-wire.ps1. Nothing read from a key file is printed.
#
#   pwsh ./scripts/dev-wiring-proof.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json

param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $State
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$identityApi     = if ($env:IDENTITY_API_URL) { $env:IDENTITY_API_URL } else { "http://127.0.0.1:8082" }
$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL } else { "http://127.0.0.1:8083" }
$organizationDeploy = (Resolve-Path (Join-Path $PSScriptRoot "../deploy/dev")).Path
$identityDeploy     = (Resolve-Path (Join-Path $IdentityRepo "deploy/dev")).Path
$s = Get-Content -Raw $State | ConvertFrom-Json

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
$operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
$client = New-Object System.Net.Http.HttpClient
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp

function Get-Report([string] $api, [string] $reason) {
    $request = New-Object System.Net.Http.HttpRequestMessage("GET", "$api/v1/provider-grants:emergency-validation")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $token)
    $request.Headers.Add("X-Administrative-Reason", $reason)
    $response = $client.SendAsync($request).Result
    $body = $response.Content.ReadAsStringAsync().Result
    return @{ code = [int]$response.StatusCode; report = $(if ($response.IsSuccessStatusCode) { $body | ConvertFrom-Json } else { $null }); body = $body }
}

function OperatorGrant($report) {
    return @($report.grants | Where-Object { $_.principal_id -eq $s.operator_principal_id }) | Select-Object -First 1
}

function Logs([string] $dir, [string] $service, [string] $since) {
    $arguments = @("compose", "--project-directory", $dir, "-f", (Join-Path $dir "compose.yaml"), "logs", "--no-color")
    if ($since) { $arguments += @("--since", $since) }
    return (& docker @arguments $service 2>&1) -join "`n"
}

function Wait-For([string] $label, [int] $seconds, [scriptblock] $condition) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) {
        if (& $condition) { Write-Host "  ok    $label"; return $true }
        Start-Sleep -Seconds 3
    }
    Write-Host "  FAIL  $label (not within $seconds s)"
    $script:failures++
    return $false
}

$failures = 0
function Expect($label, $got, $want) {
    if ($got -eq $want) { Write-Host "  ok    $label ($got)" } else { Write-Host "  FAIL  $label (got $got, want $want)"; $script:failures++ }
}

Write-Host "1. the grant reaches identity-control, and its projection reads fresh"
$null = Wait-For "identity-control holds the operator's emergency grant" 90 {
    $r = Get-Report $identityApi "deploy-dev: the wiring proof"
    $r.code -eq 200 -and $null -ne (OperatorGrant $r.report)
}
$null = Wait-For "identity-control logs the provider projection fresh" 60 {
    (Logs $identityDeploy "identity-control" "") -match "the provider projection is fresh"
}

Write-Host ""
Write-Host "2. the ceremony's grant is retired: the projected emergency grant authorizes, and is recorded"
$r = Get-Report $identityApi "deploy-dev: a drill of the emergency grant"
$grant = if ($r.code -eq 200) { OperatorGrant $r.report } else { $null }
Expect "identity-control answers the report" $r.code 200
Expect "the projected grant has been used" ($null -ne $grant -and $grant.uses -ge 1) $true
Expect "and is not overdue" ($null -ne $grant -and -not $grant.overdue) $true
Expect "identity-control logs the use with basis emergency" `
    ((Logs $identityDeploy "identity-control" "") -match '"basis":"emergency"|basis=emergency') $true

Write-Host ""
Write-Host "3. the bootstrap grant here authorized the wiring, and is recorded"
$r = Get-Report $organizationApi "deploy-dev: the wiring proof"
$grant = if ($r.code -eq 200) { OperatorGrant $r.report } else { $null }
Expect "Organization Control answers the report" $r.code 200
Expect "the bootstrap grant has been used" ($null -ne $grant -and $grant.uses -ge 2) $true

Write-Host ""
Write-Host "4. an Organization outage: stale, and the emergency grant still honored"
$stoppedAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
& docker compose --project-directory $organizationDeploy -f (Join-Path $organizationDeploy "compose.yaml") stop organization-control | Out-Null
try {
    $null = Wait-For "identity-control logs the provider projection stale" 120 {
        (Logs $identityDeploy "identity-control" $stoppedAt) -match "the provider projection is stale"
    }
    $r = Get-Report $identityApi "deploy-dev: break glass during an Organization outage"
    Expect "the emergency grant still authorizes" $r.code 200
} finally {
    $startedAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    & docker compose --project-directory $organizationDeploy -f (Join-Path $organizationDeploy "compose.yaml") start organization-control | Out-Null
}
$null = Wait-For "started again, the projection reads fresh" 90 {
    (Logs $identityDeploy "identity-control" $startedAt) -match "the provider projection is fresh"
}

if ($failures -gt 0) {
    Write-Host ""
    Write-Host "$failures check(s) failed"
    exit 1
}
Write-Host ""
Write-Host "the wiring holds"
