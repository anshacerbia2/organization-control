# The restore drill's read of known data (scripts/dev-restore-drill.sh, STD-GLB-002 §Restore
# Evidence): the provider grants, one page of Organizations and one page of offboardings, read
# through the API as the bootstrap operator and written to -Out as the service answered them. The
# drill reads before the backup and again on the restored database, and requires the two to be
# identical.
#
# -Seed first begins an offboarding on a new Tenant, as a provider would: an Organization, its
# Tenant provisioned and activated, then POST /v1/offboardings. The wiring proof makes no
# offboarding, and SAD-004 §6.6 asks a restore test to include offboarding state.
#
# Environment and SECRETS: as dev-wiring-proof.ps1. Nothing read from a key file or the environment is
# printed.
#
#   pwsh ./scripts/dev-restore-read.ps1 -IdentityRepo /srv/identity-control -State state.json -Out read.json -Seed

param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $State,
    [Parameter(Mandatory = $true)] [string] $Out,
    [switch] $Seed
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL } else { "http://127.0.0.1:8083" }
$s = Get-Content -Raw $State | ConvertFrom-Json
$run = [Guid]::NewGuid().ToString("N").Substring(0, 8)

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
$operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp
$client = New-Object System.Net.Http.HttpClient

# Call sends one request with the administrative reason provider routes require. A command carries an
# Idempotency-Key unique to this run; a read carries none.
function Call([string] $method, [string] $path, $body, [string] $key) {
    $request = New-Object System.Net.Http.HttpRequestMessage($method, "$organizationApi$path")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $token)
    $request.Headers.Add("X-Administrative-Reason", "deploy-dev: the restore drill")
    if ($key) { $request.Headers.Add("Idempotency-Key", "$key-$run") }
    if ($null -ne $body) {
        $json = $body | ConvertTo-Json -Compress -Depth 6
        $request.Content = New-Object System.Net.Http.StringContent($json, [System.Text.Encoding]::UTF8, "application/json")
    }
    $response = $client.SendAsync($request).Result
    return @{ code = [int]$response.StatusCode; text = $response.Content.ReadAsStringAsync().Result }
}

function Require([string] $label, $response, [int] $want) {
    if ($response.code -ne $want) { throw "$label answered $($response.code), want $($want): $($response.text)" }
    Write-Host "  ok    $label"
    return $response.text | ConvertFrom-Json
}

if ($Seed) {
    $organization = Require "an Organization registered" (Call "POST" "/v1/organizations" @{
            display_name = "Restore drill $run"; classification = "customer" } "drill-organization") 201
    $requested = Require "a Tenant requested" (Call "POST" "/v1/tenants" @{
            organization_id = $organization.organization_id; display_name = "Restore drill $run"
            isolation_profile = "pooled" } "drill-tenant") 201
    $tenantId = $requested.tenant.tenant_id
    $null = Require "its provisioning dispatched" (Call "POST" "/v1/tenants/$tenantId/provisioning" @{
            expected_version = $requested.tenant.version } "drill-provision") 200
    $null = Require "its provisioning realized" (Call "POST" "/v1/provisioning/realized" @{
            correlation_id = $requested.correlation_id } "drill-realized") 200
    $current = Require "the Tenant read" (Call "GET" "/v1/tenants/$tenantId" $null $null) 200
    $null = Require "the Tenant activated" (Call "POST" "/v1/tenants/$tenantId/activate" @{
            expected_version = $current.version } "drill-activate") 200
    $current = Require "the Tenant read again" (Call "GET" "/v1/tenants/$tenantId" $null $null) 200
    $null = Require "its offboarding begun" (Call "POST" "/v1/offboardings" @{
            tenant_id = $tenantId; expected_version = $current.version } "drill-offboarding") 201
}

$grants = Call "GET" "/v1/provider-grants" $null $null
$organizations = Call "GET" "/v1/organizations?limit=100" $null $null
$offboardings = Call "GET" "/v1/offboardings?limit=100" $null $null
foreach ($read in @(@("provider grants", $grants), @("Organizations", $organizations), @("offboardings", $offboardings))) {
    if ($read[1].code -ne 200) { throw "reading the $($read[0]) answered $($read[1].code): $($read[1].text)" }
}
$counts = @(
    @(($grants.text | ConvertFrom-Json).grants).Count,
    @(($organizations.text | ConvertFrom-Json).organizations).Count,
    @(($offboardings.text | ConvertFrom-Json).offboardings).Count)
if ($counts -contains 0) { throw "a read came back empty ($($counts -join ', ')); the drill would compare nothing" }

# The three answers, byte for byte as the service sent them.
Set-Content -NoNewline -Path $Out -Value ('{"provider_grants":' + $grants.text + ',"organizations":' + $organizations.text + ',"offboardings":' + $offboardings.text + '}')
Write-Host "  read $($counts[0]) provider grants, $($counts[1]) Organizations, $($counts[2]) offboardings"
