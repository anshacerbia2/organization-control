# The development server's stand-in for the provisioning system's deprovisioning report
# (TDD-organization-control-003 1.16.0 §The Provisioning System on a Development Server,
# TDD-organization-control-004 1.13.0 §Offboarding on a Development Server). Releasing an offboarding
# records a deprovisioning command and publishes it; on a development server no provisioning system
# receives it, so nothing reports its outcome and Retire stays refused. This script reports it through
# the route the system itself calls, POST /v1/offboardings/{offboarding_id}/deprovisioning, as the
# development provisioning reporter: a workload registered as production's provisioning system is,
# with its own key (ADR-ORG-001 §5.11, STD-GLB-009 rule 13, 1.10.0). It reads the offboarding first as
# the operator, a provider, because a reporter reaches its report routes alone. Production has a real
# provisioning system and never runs this.
#
# deploy/dev/README.md §Offboarding a Tenant on the development server is the procedure:
#   1. Release the offboarding in Organization Experience (or POST /v1/offboardings/{id}/release);
#   2. run this script, -Realized or -Failed -Reason;
#   3. Retire in Organization Experience (or POST /v1/offboardings/{id}/retire), or, after a failure,
#      Send deprovisioning again (POST /v1/offboardings/{id}/deprovisioning/resend) and report again.
#
# It reads the offboarding first and refuses, changing nothing, unless it is at `release` and its latest
# deprovisioning request still has no outcome: `requested`, or `unresolved` (an unresolved request is
# resolved by reporting its outcome, never by sending the command again: TDD-organization-control-004
# §Sending a Failed Deprovisioning Again). It never retires and never resends. It prints what it found
# and what it did.
#
# Exit codes: 0 reported; 2 refused, nothing sent; 1 any other failure.
#
# Environment and SECRETS: as dev-provisioning.ps1. The reads use the operator's dev-provider-caller,
# and the report the provisioning reporter's key, both from the wiring state file dev-wire.ps1 wrote.
# Nothing read from a key file or the environment is printed.
#   $env:IDENTITY_CALLER_PASSWORD      the bootstrap operator's password
#   $env:IDENTITY_OPERATOR_TOTP_FILE   optional, the operator's TOTP file (identity-control's dev-env.ps1
#                                      finds deploy/dev/keys/operator-totp.json in its checkout)
#   $env:ORGANIZATION_API_URL          optional, http://127.0.0.1:8083
#
#   pwsh ./scripts/dev-deprovisioning.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -OffboardingId <offboarding_id> -Realized
#   pwsh ./scripts/dev-deprovisioning.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -OffboardingId <offboarding_id> -Failed -Reason "the storage account is locked"

[CmdletBinding(DefaultParameterSetName = "Realized")]
param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $State,
    [Parameter(Mandatory = $true)] [string] $OffboardingId,

    [Parameter(Mandatory = $true, ParameterSetName = "Realized")] [switch] $Realized,
    [Parameter(Mandatory = $true, ParameterSetName = "Failed")] [switch] $Failed,

    # The failure's detail, which the operator reads before sending the command again. Required with
    # -Failed: the API refuses a failure without one. With -Realized it is an optional note.
    [Parameter(Mandatory = $true, ParameterSetName = "Failed")]
    [Parameter(ParameterSetName = "Realized")]
    [string] $Reason,

    # The X-Administrative-Reason recorded as provider-access evidence. Plain ASCII: anything else is
    # refused 400 (STD-GLB-001 §Request Header Values).
    [string] $AdministrativeReason = "development server: no provisioning system, deprovisioning outcome reported by dev-deprovisioning.ps1"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL.TrimEnd('/') } else { "http://127.0.0.1:8083" }
$uuid = '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
$outcome = if ($Failed) { "failed" } else { "realized" }

# Refuse ends the run having sent nothing that changes state.
function Refuse([string] $why) {
    Write-Host "  refused  $why"
    Write-Host "           nothing was reported"
    exit 2
}

if ($OffboardingId -notmatch $uuid) { Refuse "-OffboardingId '$OffboardingId' is not a UUID" }
if ($Failed -and [string]::IsNullOrWhiteSpace($Reason)) { Refuse "-Failed needs -Reason: the operator reads it before sending the command again" }
if ([string]::IsNullOrWhiteSpace($AdministrativeReason) -or $AdministrativeReason -cmatch '[^\x20-\x7E]') {
    Refuse "-AdministrativeReason must be plain printable ASCII"
}
if ([string]::IsNullOrWhiteSpace($env:IDENTITY_CALLER_PASSWORD)) { throw "IDENTITY_CALLER_PASSWORD is required." }

$s = Get-Content -Raw $State | ConvertFrom-Json

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
if (Get-Command Get-DevOperatorTotp -ErrorAction SilentlyContinue) {
    $operatorTotp = Get-DevOperatorTotp
} else {
    $operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
}
. (Join-Path $PSScriptRoot "dev-reporter.ps1")
if ($null -eq (Get-ReporterEntry $s "provisioning")) {
    Refuse "the wiring state names no development provisioning reporter; run scripts/dev-wire.ps1 (step 6b registers it)"
}
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp
$client = New-Object System.Net.Http.HttpClient

# Call sends one request with the administrative reason every provider route requires. The report
# honours an Idempotency-Key and does not require one (TDD-organization-control-004 §API: the same
# outcome sent again records the same state), so none is sent.
function Call([string] $method, [string] $path, $body, [string] $bearer = $token) {
    $request = New-Object System.Net.Http.HttpRequestMessage($method, "$organizationApi$path")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $bearer)
    $request.Headers.Add("X-Administrative-Reason", $AdministrativeReason)
    if ($null -ne $body) {
        $json = $body | ConvertTo-Json -Compress -Depth 4
        $request.Content = New-Object System.Net.Http.StringContent($json, [System.Text.Encoding]::UTF8, "application/json")
    }
    $response = $client.SendAsync($request).Result
    $text = $response.Content.ReadAsStringAsync().Result
    $parsed = $null
    if ($text) { try { $parsed = $text | ConvertFrom-Json } catch { $parsed = $null } }
    return @{ code = [int]$response.StatusCode; json = $parsed; text = $text }
}

function Get-Prop($object, [string] $name) {
    if ($null -ne $object -and $object.PSObject.Properties.Name -contains $name) { return $object.$name }
    return $null
}

function Read-Offboarding {
    $r = Call "GET" "/v1/offboardings/$OffboardingId" $null
    if ($r.code -eq 404) { Refuse "no offboarding $OffboardingId" }
    if ($r.code -ne 200) { throw "reading offboarding $OffboardingId answered $($r.code): $($r.text)" }
    return $r.json
}

$offboarding = Read-Offboarding
$deprovisioning = Get-Prop $offboarding "deprovisioning"
Write-Host "offboarding $($offboarding.offboarding_id), Tenant $($offboarding.tenant_id)"
Write-Host "stage       $($offboarding.stage)$(if ($offboarding.legal_hold) { ', legal hold set' })"
if ($null -ne $deprovisioning) {
    Write-Host "deprovision $($deprovisioning.state), requested $($deprovisioning.requested_at)$(if (Get-Prop $deprovisioning 'detail') { ": $($deprovisioning.detail)" })"
}

switch ($offboarding.stage) {
    "release" { }
    "freeze" { Refuse "the offboarding is at freeze: nothing was released yet. Complete the freeze, resolve the obligations, then Release in Organization Experience (POST /v1/offboardings/$OffboardingId/release)" }
    "obligations" { Refuse "the offboarding is at obligations: nothing was released yet. Release it first in Organization Experience (POST /v1/offboardings/$OffboardingId/release)" }
    default { Refuse "the offboarding is $($offboarding.stage): there is no deprovisioning outcome to report" }
}
if ($null -eq $deprovisioning) { Refuse "the offboarding is at release with no deprovisioning command recorded: see docs/runbooks/stuck-offboarding.md" }
switch ($deprovisioning.state) {
    "requested" { }
    "unresolved" { }
    "failed" { Refuse "the latest deprovisioning failed: Send deprovisioning again in Organization Experience (POST /v1/offboardings/$OffboardingId/deprovisioning/resend), then report the new attempt" }
    "realized" { Refuse "the latest deprovisioning is already realized: Retire in Organization Experience" }
    default { Refuse "the latest deprovisioning is $($deprovisioning.state): an attempt has one outcome" }
}

$body = @{ state = $outcome }
if (-not [string]::IsNullOrWhiteSpace($Reason)) { $body.detail = $Reason.Trim() }
# The report itself, as the provisioning reporter workload.
$r = Call "POST" "/v1/offboardings/$OffboardingId/deprovisioning" $body (Get-ReporterToken $s "provisioning")
if ($r.code -ne 204) { throw "POST /v1/offboardings/$OffboardingId/deprovisioning answered $($r.code): $($r.text)" }
Write-Host "reported    $outcome$(if ($body.Contains('detail')) { ": $($body.detail)" })"
$now = Get-Prop (Read-Offboarding) "deprovisioning"
Write-Host "now         deprovisioning $(Get-Prop $now 'state'), stage release"
if ($outcome -eq "realized") {
    Write-Host "next        retire it: Organization Experience, the offboarding's page, Retire (POST /v1/offboardings/$OffboardingId/retire)"
} else {
    Write-Host "next        fix the cause, then Organization Experience, the offboarding's page, Send deprovisioning again (POST /v1/offboardings/$OffboardingId/deprovisioning/resend)"
}
