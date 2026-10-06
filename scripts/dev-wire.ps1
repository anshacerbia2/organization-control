# Wires Organization Control to the Identity Control API on a development server: deploy/dev/README.md
# §Wiring to other services, steps 1 to 6, as one procedure. STD-GLB-009 1.3.0 asks that a procedure
# step be a script CI runs, so deploy-dev runs this against the three stacks it stands up, and the
# server's operator runs the same file.
#
#   1. two workload keys, and a key for the operator's caller of both APIs;
#   2. in identity-control: this API's resource, the two workloads, and dev-provider-caller, a
#      privileged provider-scope client whose tokens name both APIs;
#   3. the first provider here, by bootstrap-provider;
#   4. Organization's emergency provider:identity-control grant and identity-control's consumer;
#   5. delivery to identity-control;
#   6. identity-control pointed here, and its provider-bootstrap run.
#
# It is run once per pair of databases. Each creation is refused when it was made before, and the
# script stops there.
#
# SECRETS: read from the environment, as identity-control's dev-smoke.ps1 reads them, and never
# printed. Private keys stay in the keys directories; only public JWKs are sent.
#   $env:IDENTITY_CALLER_KEY_FILE      identity-control-caller's private key (identity-control's .env)
#   $env:IDENTITY_CALLER_PASSWORD      the bootstrap operator's password
#   $env:IDENTITY_OPERATOR_TOTP_FILE   optional, the operator's TOTP file (dev-token.ps1)
# Set these alone rather than sourcing identity-control's .env: its other values, POSTGRES_PASSWORD
# among them, would override this stack's own .env when the script runs docker compose here.
#
# Usage:
#   pwsh ./scripts/dev-wire.ps1 -IdentityRepo /srv/identity-control -KernelDeployDir /srv/identity-kernel/deploy/dev `
#       -Operator "you@example.com" -State ~/scnehaux-wiring/state.json
#
# The state file's directory receives dev-provider-caller's private key, so it is the operator's own,
# outside every directory a container mounts.

param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $KernelDeployDir,
    [Parameter(Mandatory = $true)] [string] $Operator,
    [Parameter(Mandatory = $true)] [string] $State,
    # The UID:GID the two services run as, and so the owner of their workload keys.
    [string] $ServiceKeysOwner = "65532:65532"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$identityApi     = if ($env:IDENTITY_API_URL) { $env:IDENTITY_API_URL } else { "http://127.0.0.1:8082" }
$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL } else { "http://127.0.0.1:8083" }
foreach ($name in @("IDENTITY_CALLER_KEY_FILE", "IDENTITY_CALLER_PASSWORD")) {
    if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name))) { throw "$name is required." }
}

$organizationDeploy = (Resolve-Path (Join-Path $PSScriptRoot "../deploy/dev")).Path
$identityDeploy     = (Resolve-Path (Join-Path $IdentityRepo "deploy/dev")).Path
$KernelDeployDir    = (Resolve-Path $KernelDeployDir).Path

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
$operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
$client = New-Object System.Net.Http.HttpClient
$run = [Guid]::NewGuid().ToString("N").Substring(0, 8)

function Send-Json($method, $url, $body, $bearer, $idempotencyKey) {
    $request = New-Object System.Net.Http.HttpRequestMessage($method, $url)
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $bearer)
    $request.Headers.Add("X-Administrative-Reason", "wiring Organization Control to the Identity Control API")
    if ($idempotencyKey) { $request.Headers.Add("Idempotency-Key", "$idempotencyKey-$run") }
    if ($body) {
        $request.Content = New-Object System.Net.Http.StringContent($body, [System.Text.Encoding]::UTF8, "application/json")
    }
    $response = $client.SendAsync($request).Result
    return @{ code = [int]$response.StatusCode; body = $response.Content.ReadAsStringAsync().Result }
}

function Require($label, $response, $want) {
    if ($response.code -ne $want) { throw "$label answered $($response.code), want $($want): $($response.body)" }
    Write-Host "  ok    $label"
    return $response.body | ConvertFrom-Json
}

function Decode-Claims([string] $jwt) {
    $s = $jwt.Split('.')[1].Replace('-', '+').Replace('_', '/')
    switch ($s.Length % 4) { 2 { $s += '==' } 3 { $s += '=' } }
    return [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($s)) | ConvertFrom-Json
}

# New-Key makes a key pair with the kernel's tool and returns the public JWK. The tool refuses to
# replace a key, which is a rotation.
function New-Key([string] $name, [string] $dir, [string] $owner) {
    & bash (Join-Path $KernelDeployDir "new-client-key.sh") $name $dir $owner | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "new-client-key.sh $name failed" }
    $jwk = Get-Content -Raw (Join-Path $dir "$name.jwk.json") | ConvertFrom-Json
    return @{ kty = $jwk.kty; n = $jwk.n; e = $jwk.e }
}

function Set-EnvLine([string] $file, [string] $key, [string] $value) {
    $lines = if (Test-Path $file) { @(Get-Content $file | Where-Object { $_ -notmatch "^$key=" }) } else { @() }
    $lines += "$key=$value"
    Set-Content -Path $file -Value $lines
}

function Compose([string] $dir) {
    & docker compose --project-directory $dir -f (Join-Path $dir "compose.yaml") @args
    if ($LASTEXITCODE -ne 0) { throw "docker compose $args in $dir failed" }
}

function Wait-Ready([string] $url) {
    for ($i = 0; $i -lt 60; $i++) {
        try { if ((Invoke-WebRequest -Uri "$url/readyz" -SkipHttpErrorCheck -TimeoutSec 2).StatusCode -eq 200) { return } } catch {}
        Start-Sleep -Seconds 2
    }
    throw "$url did not report ready"
}

Write-Host "1. the keys"
# The caller's key is a person's, so it stays beside the state file, never in a directory a service
# container mounts.
$callerKeys = Split-Path -Parent ([System.IO.Path]::GetFullPath($State))
$organizationJwk = New-Key "organization-control-workload" (Join-Path $organizationDeploy "keys") $ServiceKeysOwner
$identityJwk     = New-Key "identity-control-workload" (Join-Path $identityDeploy "keys") $ServiceKeysOwner
$callerJwk       = New-Key "dev-provider-caller" $callerKeys "$(id -u):$(id -g)"
$callerKey       = Join-Path $callerKeys "dev-provider-caller.pem"

Write-Host "2. in identity-control: the resource, the two workloads, and the caller of both APIs"
$identityToken = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -KeyFile $env:IDENTITY_CALLER_KEY_FILE @operatorTotp
$operatorPrincipal = (Decode-Claims $identityToken).principal_id

$null = Require "organization-control-api registered" (Send-Json "POST" "$identityApi/v1/registrations" (@{
            client_key = "organization-control-api"; profile = "resource"; audience_class = "privileged"
            application_ref = "organization-control"; lifetime_class = "L0" } | ConvertTo-Json -Compress) `
        $identityToken "wire-resource") 201
$organizationWorkload = Require "organization-control-workload created" (Send-Json "POST" "$identityApi/v1/workloads" (@{
            display_name = "organization-control-workload"; purpose = "Delivers provider grant events to the Identity Control API"
            workload_type = "service"; owner_principal_id = $operatorPrincipal; client_key = "organization-control-workload"
            application_ref = "organization-control"; audience = @("identity-control-api"); public_key = $organizationJwk
        } | ConvertTo-Json -Compress -Depth 4) $identityToken "wire-organization-workload") 201
$identityWorkload = Require "identity-control-workload created" (Send-Json "POST" "$identityApi/v1/workloads" (@{
            display_name = "identity-control-workload"; purpose = "Reads Organization Control's provider authority snapshot and frontier"
            workload_type = "service"; owner_principal_id = $operatorPrincipal; client_key = "identity-control-workload"
            application_ref = "identity-control"; audience = @("organization-control-api"); public_key = $identityJwk
        } | ConvertTo-Json -Compress -Depth 4) $identityToken "wire-identity-workload") 201
# A registered caller, so its audience is the registration's to change: identity-control-caller was
# made in the kernel before registration existed and is not one.
$null = Require "dev-provider-caller registered" (Send-Json "POST" "$identityApi/v1/registrations" (@{
            client_key = "dev-provider-caller"; profile = "confidential"; audience_class = "privileged"
            privileged_form = "provider-scope"; application_ref = "development-operator"
            audience = @("identity-control-api", "organization-control-api")
            redirect_uris = @("http://127.0.0.1:8099/callback"); public_key = $callerJwk
        } | ConvertTo-Json -Compress -Depth 4) $identityToken "wire-caller") 201

Write-Host "3. the first provider here"
Compose $organizationDeploy run --rm bootstrap-provider -principal-id $operatorPrincipal -operator $Operator `
    -reason "the first provider of Organization Control on the development server"

Write-Host "4. Organization's grant and consumer for the Identity Control API"
$token = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
    -ClientId "dev-provider-caller" -KeyFile $callerKey @operatorTotp
$audience = @((Decode-Claims $token).aud)
if (-not ($audience -contains "organization-control-api") -or -not ($audience -contains "identity-control-api")) {
    throw "dev-provider-caller's token names $($audience -join ', '), not both APIs"
}
$null = Require "emergency provider:identity-control grant" (Send-Json "POST" "$organizationApi/v1/provider-grants" (@{
            principal_id = $operatorPrincipal; scope = "provider:identity-control"; kind = "emergency" } | ConvertTo-Json -Compress) `
        $token "wire-grant") 201
$eventTypes = @(
    "com.scnehaux.organization.provider.lifecycle.granted", "com.scnehaux.organization.provider.lifecycle.activated",
    "com.scnehaux.organization.provider.security.ended", "com.scnehaux.organization.provider.security.revoked",
    "com.scnehaux.organization.membership.lifecycle.granted", "com.scnehaux.organization.membership.lifecycle.restored",
    "com.scnehaux.organization.membership.security.suspended", "com.scnehaux.organization.membership.security.revoked",
    "com.scnehaux.organization.tenant.lifecycle.activated", "com.scnehaux.organization.tenant.lifecycle.retired",
    "com.scnehaux.organization.tenant.security.suspended", "com.scnehaux.organization.tenant.security.restored",
    "com.scnehaux.organization.projection.repair.reconciled")
$null = Require "identity-control registered as a consumer" (Send-Json "POST" "$organizationApi/v1/projections/consumers" (@{
            consumer_id = "identity-control"; principal_id = $identityWorkload.principal_id; projection_version = "v1"
            max_accepted_age_seconds = 60; stale_behavior = "fail_closed"; event_types = $eventTypes
        } | ConvertTo-Json -Compress) $token "wire-consumer") 201

Write-Host "5. deliver to identity-control"
Set-EnvLine (Join-Path $organizationDeploy ".env") "ORGANIZATION_DELIVERY_TARGETS" "identity-control=http://identity-control:8090/v1/deliveries"
Compose $organizationDeploy up -d organization-control
Wait-Ready $organizationApi

Write-Host "6. point identity-control here"
Set-EnvLine (Join-Path $identityDeploy ".env") "IDENTITY_DELIVERY_PRINCIPAL_ID" $organizationWorkload.principal_id
Set-EnvLine (Join-Path $identityDeploy ".env") "IDENTITY_ORGANIZATION_BASE_URL" "http://organization-control:8080"
Compose $identityDeploy up -d identity-control
Wait-Ready $identityApi
$bootstrap = & docker compose --project-directory $identityDeploy -f (Join-Path $identityDeploy "compose.yaml") run --rm provider-bootstrap 2>&1
$bootstrap | ForEach-Object { Write-Host "        $_" }
if ($LASTEXITCODE -ne 0) { throw "provider-bootstrap failed" }
if (-not ($bootstrap -match "memberships\s+\d+")) { throw "provider-bootstrap did not bootstrap the Tenant context" }

@{ operator_principal_id = $operatorPrincipal; organization_workload_principal_id = $organizationWorkload.principal_id
    identity_workload_principal_id = $identityWorkload.principal_id; caller_client_id = "dev-provider-caller"
    caller_key_file = $callerKey } | ConvertTo-Json -Compress | Set-Content -NoNewline $State
Write-Host ""
Write-Host "wired: the state is in $State"
