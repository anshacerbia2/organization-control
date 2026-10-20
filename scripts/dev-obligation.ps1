# The development server's stand-in for the domain that owns an offboarding obligation
# (TDD-organization-control-004 1.13.0 §Offboarding on a Development Server). An obligation is raised
# against a domain (product, hcm, billing, audit, ...) and only that domain resolves it, through
# POST /v1/obligations/{obligation_id}/resolve naming itself. On a development server no such domain
# runs, so an obligation raised there stays open and Release stays refused. This script reports the
# domain's outcome through the same route, as the development reporter registered for
# obligation:<domain>: a workload with its own key, admitted from its record as production's domain is
# (ADR-ORG-001 §5.11, STD-GLB-009 rule 13, 1.10.0). No provider resolves an obligation, so the route
# refuses the operator's own token. It reads the board first as the operator, a provider. Production's
# domains report their own outcomes and nothing runs this. Organization Experience offers no control for it on purpose: an
# operator there cannot resolve another domain's obligation (TDD-organization-experience-003 §As Served).
#
# deploy/dev/README.md §Offboarding a Tenant on the development server is the procedure:
#   1. Raise the obligation in Organization Experience (or POST /v1/offboardings/{id}/obligations);
#   2. run this script for it, -Completed, -Waived -Reason or -Failed -Reason;
#   3. once none is open or failed, Release in Organization Experience.
#
# It reads the obligation board first and refuses, changing nothing, when the obligation is not on it,
# when -Domain is not the domain it was raised against, when it is already completed, waived or
# cancelled (final: the first resolution records who decided), or when the offboarding is retired or
# cancelled. A failed obligation may still be completed or waived. It prints what it found and what it
# did.
#
# Exit codes: 0 reported; 2 refused, nothing sent; 1 any other failure.
#
# Environment and SECRETS: as dev-provisioning.ps1. The reads use the operator's dev-provider-caller, and
# the resolution the domain reporter's key, both from the wiring state file dev-wire.ps1 wrote.
#   $env:IDENTITY_CALLER_PASSWORD      the bootstrap operator's password
#   $env:IDENTITY_OPERATOR_TOTP_FILE   optional, the operator's TOTP file
#   $env:ORGANIZATION_API_URL          optional, http://127.0.0.1:8083
#
#   pwsh ./scripts/dev-obligation.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -OffboardingId <offboarding_id> -ObligationId <obligation_id> -Domain product -Completed
#   pwsh ./scripts/dev-obligation.ps1 -IdentityRepo /srv/identity-control -State ~/scnehaux-wiring/state.json `
#       -OffboardingId <offboarding_id> -ObligationId <obligation_id> -Domain billing -Waived -Reason "no invoice is owed"

[CmdletBinding(DefaultParameterSetName = "Completed")]
param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $State,
    [Parameter(Mandatory = $true)] [string] $OffboardingId,
    [Parameter(Mandatory = $true)] [string] $ObligationId,

    # The domain the operator stands in for. The API checks it against the obligation's own.
    [Parameter(Mandatory = $true)] [string] $Domain,

    [Parameter(Mandatory = $true, ParameterSetName = "Completed")] [switch] $Completed,
    [Parameter(Mandatory = $true, ParameterSetName = "Waived")] [switch] $Waived,
    [Parameter(Mandatory = $true, ParameterSetName = "Failed")] [switch] $Failed,

    # The outcome's detail. Required for a waiver, whose reason is the audit's half, and a failure;
    # an optional note with -Completed.
    [Parameter(Mandatory = $true, ParameterSetName = "Waived")]
    [Parameter(Mandatory = $true, ParameterSetName = "Failed")]
    [Parameter(ParameterSetName = "Completed")]
    [string] $Reason,

    # The X-Administrative-Reason: required on the operator's reads, and recorded on the reporter's
# access row. Plain ASCII.
    [string] $AdministrativeReason = "development server: no owning domain, obligation outcome reported by dev-obligation.ps1"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL.TrimEnd('/') } else { "http://127.0.0.1:8083" }
$uuid = '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
$outcome = if ($Waived) { "waived" } elseif ($Failed) { "failed" } else { "completed" }

function Refuse([string] $why) {
    Write-Host "  refused  $why"
    Write-Host "           nothing was reported"
    exit 2
}

if ($OffboardingId -notmatch $uuid) { Refuse "-OffboardingId '$OffboardingId' is not a UUID" }
if ($ObligationId -notmatch $uuid) { Refuse "-ObligationId '$ObligationId' is not a UUID" }
if ([string]::IsNullOrWhiteSpace($Domain)) { Refuse "-Domain names the domain you stand in for" }
if (($Waived -or $Failed) -and [string]::IsNullOrWhiteSpace($Reason)) { Refuse "-$($outcome.Substring(0,1).ToUpper())$($outcome.Substring(1)) needs -Reason: it is the record of what was decided and why" }
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
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp
$client = New-Object System.Net.Http.HttpClient

# Call sends one request with the administrative reason. The resolution is a command and requires an
# Idempotency-Key (TDD-organization-control-004 §API): one per run, so a run is one command.
$idempotencyKey = [Guid]::NewGuid().ToString()
function Call([string] $method, [string] $path, $body, [string] $bearer = $token) {
    $request = New-Object System.Net.Http.HttpRequestMessage($method, "$organizationApi$path")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $bearer)
    $request.Headers.Add("X-Administrative-Reason", $AdministrativeReason)
    if ($method -eq "POST") { $request.Headers.Add("Idempotency-Key", $idempotencyKey) }
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

function Read-Board {
    $r = Call "GET" "/v1/offboardings/$OffboardingId/obligations" $null
    if ($r.code -eq 404) { Refuse "no offboarding $OffboardingId" }
    if ($r.code -ne 200) { throw "reading the obligation board answered $($r.code): $($r.text)" }
    return $r.json
}

$read = Call "GET" "/v1/offboardings/$OffboardingId" $null
if ($read.code -eq 404) { Refuse "no offboarding $OffboardingId" }
if ($read.code -ne 200) { throw "reading offboarding $OffboardingId answered $($read.code): $($read.text)" }
$offboarding = $read.json
Write-Host "offboarding $($offboarding.offboarding_id), Tenant $($offboarding.tenant_id), stage $($offboarding.stage)"
if ($offboarding.stage -eq "retired" -or $offboarding.stage -eq "cancelled") {
    Refuse "the offboarding is $($offboarding.stage): its obligations are closed"
}

$board = Read-Board
$obligation = @($board.obligations | Where-Object { $null -ne $_ -and $_.obligation_id -eq $ObligationId.ToLowerInvariant() }) | Select-Object -First 1
if ($null -eq $obligation) { Refuse "obligation $ObligationId is not on this offboarding's board" }
Write-Host "obligation  $($obligation.obligation_id) $($obligation.domain)/$($obligation.type), $($obligation.state)$(if (Get-Prop $obligation 'detail') { ": $($obligation.detail)" })"
if ($obligation.domain -cne $Domain.Trim()) {
    Refuse "it was raised against $($obligation.domain), not $($Domain.Trim()): only its own domain resolves it. Run with -Domain $($obligation.domain) if you stand in for that domain"
}
if ($obligation.state -eq "completed" -or $obligation.state -eq "waived" -or $obligation.state -eq "cancelled") {
    Refuse "it is already $($obligation.state): the first resolution is final"
}

if ($null -eq (Get-ReporterEntry $s "obligation:$($obligation.domain)")) {
    Refuse "the wiring state names no development reporter for obligation:$($obligation.domain); scripts/dev-wire.ps1 step 6b registers product, hcm, billing and audit"
}
$body = @{ domain = $obligation.domain; state = $outcome }
if (-not [string]::IsNullOrWhiteSpace($Reason)) { $body.detail = $Reason.Trim() }
# The resolution itself, as the domain's reporter workload.
$r = Call "POST" "/v1/obligations/$($obligation.obligation_id)/resolve" $body (Get-ReporterToken $s "obligation:$($obligation.domain)")
if ($r.code -ne 200) { throw "POST /v1/obligations/$($obligation.obligation_id)/resolve answered $($r.code): $($r.text)" }
Write-Host "reported    $outcome$(if ($body.Contains('detail')) { ": $($body.detail)" }) for $($obligation.domain)"
Write-Host "now         obligation $($r.json.state)"

$outstanding = @((Read-Board).outstanding | Where-Object { $null -ne $_ })
if ($outstanding.Count -eq 0) {
    Write-Host "next        nothing is outstanding: Organization Experience, the offboarding's page, Release (POST /v1/offboardings/$OffboardingId/release)"
} else {
    Write-Host "next        still outstanding: $($outstanding -join '; ')"
}
