# The development server's stand-in for the provisioning system (TDD-organization-control-003 1.15.0
# §The Provisioning System on a Development Server). No provisioning system runs on a development
# server, so nothing reports a Tenant's outcome there. This script reports it through the two routes
# the system itself calls, POST /v1/provisioning/realized and POST /v1/provisioning/failed, as the
# development provisioning reporter: a workload registered as production's provisioning system is,
# with its own key (ADR-ORG-001 §5.11, STD-GLB-009 rule 13, 1.10.0). Same routes, same authentication,
# same correlation identifier. It reads the Tenant first as the operator, a provider, because a
# reporter reaches its report routes alone. Production has a real provisioning system and never runs
# this.
#
# deploy/dev/README.md §Provisioning a Tenant on the development server is the procedure:
#   1. Start provisioning in Organization Experience (or POST /v1/tenants/{id}/provisioning);
#   2. run this script, -Realized or -Failed -Reason;
#   3. Activate in Organization Experience (or POST /v1/tenants/{id}/activate), or retry a failure.
#
# It reads the Tenant first and refuses, changing nothing, unless the Tenant is `provisioning` and its
# latest provisioning request still has no outcome: `requested`, or `unresolved` (TDD-003 §Scheduled
# Sweeps: an unresolved request is resolved by reporting its outcome). It never activates and never
# retries. It prints what it found and what it did.
#
# Exit codes: 0 reported (or the same outcome was already recorded: a replay); 2 refused, nothing sent;
# 1 any other failure.
#
# Environment and SECRETS: as dev-wiring-proof.ps1. The reads use the operator's dev-provider-caller,
# and the report the provisioning reporter's key, both from the wiring state file dev-wire.ps1 wrote.
# Nothing read from a key file or the environment is printed.
#   $env:IDENTITY_CALLER_PASSWORD      the bootstrap operator's password
#   $env:IDENTITY_OPERATOR_TOTP_FILE   optional, the operator's TOTP file (identity-control's dev-env.ps1
#                                      finds deploy/dev/keys/operator-totp.json in its checkout)
#   $env:ORGANIZATION_API_URL          optional, http://127.0.0.1:8083
#
#   pwsh ./scripts/dev-provisioning.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -TenantId <tenant_id> -Realized
#   pwsh ./scripts/dev-provisioning.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -CorrelationId <correlation_id> -Failed -Reason "the silo region is at capacity"

[CmdletBinding(DefaultParameterSetName = "RealizedByTenant")]
param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $State,

    [Parameter(Mandatory = $true, ParameterSetName = "RealizedByTenant")]
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByTenant")]
    [string] $TenantId,
    [Parameter(Mandatory = $true, ParameterSetName = "RealizedByCorrelation")]
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByCorrelation")]
    [string] $CorrelationId,

    [Parameter(Mandatory = $true, ParameterSetName = "RealizedByTenant")]
    [Parameter(Mandatory = $true, ParameterSetName = "RealizedByCorrelation")]
    [switch] $Realized,
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByTenant")]
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByCorrelation")]
    [switch] $Failed,

    # The failure's detail, which the operator reads before a retry. Required with -Failed: the API
    # refuses a failure without one. With -Realized it is an optional note.
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByTenant")]
    [Parameter(Mandatory = $true, ParameterSetName = "FailedByCorrelation")]
    [Parameter(ParameterSetName = "RealizedByTenant")]
    [Parameter(ParameterSetName = "RealizedByCorrelation")]
    [string] $Reason,

    # The X-Administrative-Reason recorded as provider-access evidence. Plain ASCII: anything else is
    # refused 400 (STD-GLB-001 §Request Header Values).
    [string] $AdministrativeReason = "development server: no provisioning system, outcome reported by dev-provisioning.ps1 as the development provisioning reporter"
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

if ($PSBoundParameters.ContainsKey("TenantId") -and $TenantId -notmatch $uuid) { Refuse "-TenantId '$TenantId' is not a UUID" }
if ($PSBoundParameters.ContainsKey("CorrelationId") -and $CorrelationId -notmatch $uuid) { Refuse "-CorrelationId '$CorrelationId' is not a UUID" }
if ($Failed -and [string]::IsNullOrWhiteSpace($Reason)) { Refuse "-Failed needs -Reason: the operator reads it before a retry" }
if ([string]::IsNullOrWhiteSpace($AdministrativeReason) -or $AdministrativeReason -cmatch '[^\x20-\x7E]') {
    Refuse "-AdministrativeReason must be plain printable ASCII"
}
if ([string]::IsNullOrWhiteSpace($env:IDENTITY_CALLER_PASSWORD)) { throw "IDENTITY_CALLER_PASSWORD is required." }

$s = Get-Content -Raw $State | ConvertFrom-Json

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
# identity-control's dev-env.ps1, loaded by dev-token.ps1, derives the operator's TOTP file from its own
# checkout; an older checkout without it falls back to the variable alone.
if (Get-Command Get-DevOperatorTotp -ErrorAction SilentlyContinue) {
    $operatorTotp = Get-DevOperatorTotp
} else {
    $operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
}
. (Join-Path $PSScriptRoot "dev-reporter.ps1")
if ($null -eq (Get-ReporterEntry $s "provisioning")) {
    Refuse "the wiring state names no development provisioning reporter; run scripts/dev-wire.ps1 (step 7 registers it)"
}
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp
$client = New-Object System.Net.Http.HttpClient

# Call sends one request with the administrative reason: every provider route requires it, and the
# reporter's access record carries it. The two reports honour an Idempotency-Key and do not require one
# (TDD-003 §The Idempotency-Key Is Required on Commands): a duplicate is answered as a replay by its
# correlation identifier, so none is sent. $bearer is the operator's token unless given.
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

function Read-Tenant([string] $id) {
    $r = Call "GET" "/v1/tenants/$id" $null
    if ($r.code -eq 404) { Refuse "no Tenant $id" }
    if ($r.code -ne 200) { throw "reading Tenant $id answered $($r.code): $($r.text)" }
    return $r.json
}

function Get-Prop($object, [string] $name) {
    if ($null -ne $object -and $object.PSObject.Properties.Name -contains $name) { return $object.$name }
    return $null
}

# By correlation identifier: the Tenant whose latest provisioning request carries it. Only a Tenant in
# `provisioning` can be the one, so only those are read; a development server holds few.
if ($PSBoundParameters.ContainsKey("CorrelationId")) {
    $TenantId = ""
    $after = $null
    do {
        $page = Call "GET" ("/v1/tenants?status=provisioning&limit=100" + $(if ($after) { "&after=$after" } else { "" })) $null
        if ($page.code -ne 200) { throw "listing Tenants in provisioning answered $($page.code): $($page.text)" }
        foreach ($candidate in @($page.json.tenants)) {
            if ($null -eq $candidate) { continue }
            $latest = Get-Prop (Read-Tenant $candidate.tenant_id) "provisioning"
            if ($null -ne $latest -and $latest.correlation_id -eq $CorrelationId.ToLowerInvariant()) { $TenantId = $candidate.tenant_id; break }
        }
        $after = Get-Prop $page.json "next"
    } while ([string]::IsNullOrEmpty($TenantId) -and $after)
    if ([string]::IsNullOrEmpty($TenantId)) {
        Refuse "no Tenant in provisioning has a latest request with correlation $CorrelationId; start provisioning first, or check the identifier"
    }
}

$tenant = Read-Tenant $TenantId
$request = Get-Prop $tenant "provisioning"
Write-Host "Tenant     $($tenant.tenant_id) `"$($tenant.display_name)`""
Write-Host "status     $($tenant.status), version $($tenant.version)"
if ($null -eq $request) { Refuse "the Tenant has no provisioning request" }
Write-Host "request    $($request.request_id), correlation $($request.correlation_id), state $($request.state)"

switch ($tenant.status) {
    "provisioning" { }
    "requested" { Refuse "the Tenant is requested: start provisioning first (Organization Experience, Tenants, Start provisioning; or POST /v1/tenants/$TenantId/provisioning)" }
    "failed" { Refuse "the Tenant is failed: retry provisioning first (Organization Experience, Tenants, Retry provisioning), then report the new attempt" }
    default { Refuse "the Tenant is $($tenant.status): there is no provisioning outcome to report" }
}
if ($request.state -ne "requested" -and $request.state -ne "unresolved") {
    Refuse "the latest request is already $($request.state): an attempt has one outcome"
}

$body = @{ correlation_id = $request.correlation_id }
if (-not [string]::IsNullOrWhiteSpace($Reason)) { $body.detail = $Reason.Trim() }
# The report itself, as the provisioning reporter workload.
$r = Call "POST" "/v1/provisioning/$outcome" $body (Get-ReporterToken $s "provisioning")
if ($r.code -ne 200) { throw "POST /v1/provisioning/$outcome answered $($r.code): $($r.text)" }
$now = Get-Prop $r.json "tenant"
Write-Host "reported   $outcome$(if ($body.Contains('detail')) { ": $($body.detail)" })"
Write-Host "now        request $($r.json.state), Tenant $($now.status), version $($now.version)$(if ($r.json.replay) { ' (a replay: already recorded, nothing changed)' })"
if ($outcome -eq "realized") {
    Write-Host "next       activate it: Organization Experience, Tenants, Activate (POST /v1/tenants/$TenantId/activate)"
} else {
    Write-Host "next       fix the cause, then Organization Experience, Tenants, Retry provisioning (POST /v1/tenants/$TenantId/provisioning)"
}
