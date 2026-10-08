# What deploy/dev/README.md §Wiring step 7 says an operator should see, asserted against the three
# stacks scripts/dev-wire.ps1 wired (TDD-identity-control-006, ADR-ORG-002 §5.2, §5.3):
#
#   1. Organization's provider:identity-control grant reaches identity-control's projection, and the
#      projection reads fresh;
#   2. the ceremony's grant is retired: the operator's requests to identity-control are now
#      authorized by the projected emergency grant, whose use identity-control records;
#   3. the operator's requests here were authorized by the bootstrap emergency grant, whose use this
#      service records;
#   4. a Membership a Tenant administrator grants here reaches identity-control: a provider makes a
#      Tenant and its first administrator (ADR-ORG-003), the administrator signs in for that Tenant
#      through a tenant-scoped privileged client, grants a Membership with that token, and the
#      Membership appears in identity-control's Tenant context report and in the kernel's
#      Organization for the Tenant (ADR-IAM-006);
#   5. the Membership survives a relink: the member's Keycloak user is deleted and relinked under the
#      same principal_id (identity-control ROADMAP §Proof B), the Membership here and in
#      identity-control's report is unchanged, and the new user joins the Tenant's Organization;
#   6. priority events are timed: 20 revocations, each from its accepted instant to its
#      acknowledgement, its publication and identity-control's applied receipt, against
#      TDD-organization-control-002 §Enforcement Budget;
#   7. with Organization Control stopped, identity-control reads the projection stale and still
#      honors the emergency grant; started again, the projection reads fresh.
#
# Environment and SECRETS: as dev-wire.ps1, plus the kernel's console administrator for steps 4 and 5,
# which read the kernel's Organizations and delete a user the way identity-control's Proof B does:
#   $env:KC_BOOTSTRAP_ADMIN_PASSWORD   the console administrator's password (the kernel's .env)
#   $env:KC_BOOTSTRAP_ADMIN_USERNAME   optional, "admin"
#   $env:KC_ADMIN_URL                  where /admin is reachable, http://localhost:8080 in CI
# Nothing read from a key file or the environment is printed. Step 5 deletes a Keycloak user, so the
# script is for a kernel a development server or a CI job owns.
#
#   pwsh ./scripts/dev-wiring-proof.ps1 -IdentityRepo /srv/identity-control `
#       -KernelDeployDir /srv/identity-kernel/deploy/dev -State ~/scnehaux-wiring/state.json

param(
    [Parameter(Mandatory = $true)] [string] $IdentityRepo,
    [Parameter(Mandatory = $true)] [string] $KernelDeployDir,
    [Parameter(Mandatory = $true)] [string] $State
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$identityApi     = if ($env:IDENTITY_API_URL) { $env:IDENTITY_API_URL } else { "http://127.0.0.1:8082" }
$organizationApi = if ($env:ORGANIZATION_API_URL) { $env:ORGANIZATION_API_URL } else { "http://127.0.0.1:8083" }
$organizationDeploy = (Resolve-Path (Join-Path $PSScriptRoot "../deploy/dev")).Path
$identityDeploy     = (Resolve-Path (Join-Path $IdentityRepo "deploy/dev")).Path
$KernelDeployDir    = (Resolve-Path $KernelDeployDir).Path
$kcAdmin   = if ($env:KC_ADMIN_URL) { $env:KC_ADMIN_URL } else { "http://localhost:8080" }
$kcRealm   = if ($env:KC_REALM) { $env:KC_REALM } else { "scnehaux" }
$adminUser = if ($env:KC_BOOTSTRAP_ADMIN_USERNAME) { $env:KC_BOOTSTRAP_ADMIN_USERNAME } else { "admin" }
if ([string]::IsNullOrWhiteSpace($env:KC_BOOTSTRAP_ADMIN_PASSWORD)) { throw "KC_BOOTSTRAP_ADMIN_PASSWORD is required." }
$s = Get-Content -Raw $State | ConvertFrom-Json
$run = [Guid]::NewGuid().ToString("N").Substring(0, 8)

Add-Type -AssemblyName System.Net.Http
. (Join-Path $IdentityRepo "scripts/dev-token.ps1")
$operatorTotp = if ($env:IDENTITY_OPERATOR_TOTP_FILE) { @{ OperatorTotpFile = $env:IDENTITY_OPERATOR_TOTP_FILE } } else { @{} }
$client = New-Object System.Net.Http.HttpClient

# A token naming organization-control-api lives 240 seconds (L0), and steps 4 to 6 take longer, so
# both of the operator's tokens are renewed well before that, as identity-control's Proof B renews
# its own. Each sign-in is at aal2 and may wait for the next TOTP step.
$script:providerToken = $null
$script:providerTokenAt = [datetime]::MinValue
function Provider-Token {
    if (((Get-Date) - $script:providerTokenAt).TotalSeconds -gt 150) {
        $script:providerToken = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
            -ClientId $s.caller_client_id -KeyFile $s.caller_key_file @operatorTotp
        $script:providerTokenAt = Get-Date
    }
    return $script:providerToken
}

function Get-Report([string] $api, [string] $reason) {
    $request = New-Object System.Net.Http.HttpRequestMessage("GET", "$api/v1/provider-grants:emergency-validation")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", (Provider-Token))
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

# --- steps 4 to 6: a Tenant administrator's Membership, a relink, and the priority-event delay ---

# Call sends one request. Every one carries the administrative reason, which provider routes require
# and tenant routes ignore. A command carries an Idempotency-Key unique to this run; a read carries
# none, since both services refuse a key on a read.
function Call([string] $method, [string] $url, $body, [string] $bearer, [string] $key) {
    $request = New-Object System.Net.Http.HttpRequestMessage($method, $url)
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $bearer)
    $request.Headers.Add("X-Administrative-Reason", "deploy-dev: the wiring proof")
    if ($key) { $request.Headers.Add("Idempotency-Key", "$key-$run") }
    if ($null -ne $body) {
        $json = if ($body -is [string]) { $body } else { $body | ConvertTo-Json -Compress -Depth 6 }
        $request.Content = New-Object System.Net.Http.StringContent($json, [System.Text.Encoding]::UTF8, "application/json")
    }
    $response = $client.SendAsync($request).Result
    $text = $response.Content.ReadAsStringAsync().Result
    $parsed = $null
    if ($text) { try { $parsed = $text | ConvertFrom-Json } catch { $parsed = $null } }
    return @{ code = [int]$response.StatusCode; json = $parsed; text = $text; at = [DateTimeOffset]::UtcNow }
}

# Require stops steps 4 to 6 on a setup call that failed: nothing after it could be meaningful.
function Require([string] $label, $response, [int] $want) {
    if ($response.code -ne $want) { throw "$label answered $($response.code), want $($want): $($response.text)" }
    Write-Host "  ok    $label"
    return $response.json
}

# Get-Prop reads a property that may be absent, which strict mode would otherwise throw on.
function Get-Prop($object, [string] $name) {
    if ($null -ne $object -and $object.PSObject.Properties.Name -contains $name) { return $object.$name }
    return $null
}

function Decode-Claims([string] $jwt) {
    $p = $jwt.Split('.')[1].Replace('-', '+').Replace('_', '/')
    switch ($p.Length % 4) { 2 { $p += '==' } 3 { $p += '=' } }
    return [System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($p)) | ConvertFrom-Json
}

# New-UuidV7 is an RFC 9562 version 7 identifier, the form every identifier here takes.
function New-UuidV7 {
    $bytes = New-Object byte[] 16
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    $ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    for ($i = 5; $i -ge 0; $i--) { $bytes[$i] = [byte]($ms -band 0xff); $ms = $ms -shr 8 }
    $bytes[6] = ($bytes[6] -band 0x0f) -bor 0x70
    $bytes[8] = ($bytes[8] -band 0x3f) -bor 0x80
    $hex = -join ($bytes | ForEach-Object { $_.ToString("x2") })
    return "$($hex.Substring(0,8))-$($hex.Substring(8,4))-$($hex.Substring(12,4))-$($hex.Substring(16,4))-$($hex.Substring(20,12))"
}

# The kernel's console administrator, on the Admin API. Master-realm admin tokens live a minute.
$script:adminToken = $null
$script:adminTokenAt = [datetime]::MinValue
function Kc([string] $method, [string] $path) {
    if (((Get-Date) - $script:adminTokenAt).TotalSeconds -gt 30) {
        $form = New-Object 'System.Collections.Generic.Dictionary[string,string]'
        $form["grant_type"] = "password"
        $form["client_id"] = "admin-cli"
        $form["username"] = $adminUser
        $form["password"] = $env:KC_BOOTSTRAP_ADMIN_PASSWORD
        $response = $client.PostAsync("$kcAdmin/realms/master/protocol/openid-connect/token",
            (New-Object System.Net.Http.FormUrlEncodedContent($form))).Result
        if (-not $response.IsSuccessStatusCode) { throw "the console administrator could not log in: $([int]$response.StatusCode)" }
        $script:adminToken = ($response.Content.ReadAsStringAsync().Result | ConvertFrom-Json).access_token
        $script:adminTokenAt = Get-Date
    }
    $request = New-Object System.Net.Http.HttpRequestMessage($method, "$kcAdmin/admin/realms/$kcRealm$path")
    $request.Headers.Authorization = New-Object System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", $script:adminToken)
    $response = $client.SendAsync($request).Result
    $text = $response.Content.ReadAsStringAsync().Result
    $parsed = $null
    if ($text) { try { $parsed = $text | ConvertFrom-Json } catch { $parsed = $null } }
    return @{ code = [int]$response.StatusCode; json = $parsed }
}

# The Keycloak users carrying a Principal's identifier, as identity-control's Proof B finds them.
function Users-Carrying([string] $principal) {
    return @((Kc "GET" "/users?q=scnehaux_principal_id:$($principal)&exact=true").json | Where-Object { $null -ne $_ })
}

# The Keycloak user ids that are members of the Tenant's Organization, whose name and alias are the
# tenant_id (ADR-IAM-006). Empty until identity-control has created it.
function Organization-Members([string] $tenant) {
    $organization = @((Kc "GET" "/organizations?search=$($tenant)&exact=true").json |
            Where-Object { $null -ne $_ -and (Get-Prop $_ "alias") -eq $tenant }) | Select-Object -First 1
    if ($null -eq $organization) { return @() }
    return @((Kc "GET" "/organizations/$($organization.id)/members?first=0&max=1000").json |
            Where-Object { $null -ne $_ } | ForEach-Object { $_.id })
}

# New-Key makes a key pair with the kernel's tool, as dev-wire.ps1 does, and returns the public JWK.
# The private half stays beside the state file, the operator's own directory.
function New-Key([string] $name, [string] $dir) {
    & bash (Join-Path $KernelDeployDir "new-client-key.sh") $name $dir "$(id -u):$(id -g)" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "new-client-key.sh $name failed" }
    $jwk = Get-Content -Raw (Join-Path $dir "$name.jwk.json") | ConvertFrom-Json
    return @{ kty = $jwk.kty; n = $jwk.n; e = $jwk.e }
}

# Instant reads a timestamp as an instant. Go writes up to nine fractional digits, and .NET reads
# seven, so the rest are dropped: a hundred nanoseconds is far below anything measured here.
function Instant([string] $text) {
    return [DateTimeOffset]::Parse(($text -replace '(\.\d{7})\d+', '$1'), [Globalization.CultureInfo]::InvariantCulture)
}

# Rank is the nearest-rank percentile of a list of milliseconds.
function Rank([double[]] $values, [double] $p) {
    $sorted = @($values | Sort-Object)
    return $sorted[[math]::Max(0, [int][math]::Ceiling($p / 100 * $sorted.Count) - 1)]
}

$keyDir = Split-Path -Parent ([System.IO.Path]::GetFullPath($State))
$tenantId = $null
$tenantClient = "dev-tenant-admin-$run"
$tenantKey = Join-Path $keyDir "$tenantClient.pem"
$script:tenantToken = $null
$script:tenantTokenAt = [datetime]::MinValue
function Tenant-Token {
    if (((Get-Date) - $script:tenantTokenAt).TotalSeconds -gt 150) {
        $script:tenantToken = Get-ScnehauxToken -Username "bootstrap-operator" -Password $env:IDENTITY_CALLER_PASSWORD `
            -ClientId $tenantClient -KeyFile $tenantKey -Scope "openid organization:$tenantId" @operatorTotp
        $script:tenantTokenAt = Get-Date
    }
    return $script:tenantToken
}

# Grant-Member grants a Tenant-wide Membership as the Tenant administrator (POST /v1/memberships).
function Grant-Member([string] $principal, [string] $key) {
    return Call "POST" "$organizationApi/v1/memberships" @{ principal_id = $principal; subject_type = "human"
        provenance = "deploy-dev wiring proof"
        valid_from = [DateTimeOffset]::UtcNow.AddMinutes(-1).ToString("yyyy-MM-ddTHH:mm:ssZ") } (Tenant-Token) $key
}

# Report-Version is the version identity-control's Tenant context report holds for a Membership, or
# $null when the report does not list it. The report lists active Memberships only.
function Report-Version([string] $membershipId) {
    $r = Call "GET" "$identityApi/v1/projections/tenant-context/report" $null (Provider-Token) $null
    if ($r.code -ne 200) { return $null }
    $row = @((Get-Prop $r.json "rows") | Where-Object { $null -ne $_ -and $_.membership_id -eq $membershipId }) | Select-Object -First 1
    if ($null -eq $row) { return $null }
    return [long]$row.membership_version
}

$memberPrincipal = $null
$memberId = $null
try {
    Write-Host ""
    Write-Host "4. a Membership a Tenant administrator grants here reaches identity-control"
    $organization = Require "an Organization registered" (Call "POST" "$organizationApi/v1/organizations" @{
            display_name = "Wiring proof $run"; classification = "customer" } (Provider-Token) "proof-organization") 201
    $requested = Require "a Tenant requested" (Call "POST" "$organizationApi/v1/tenants" @{
            organization_id = $organization.organization_id; display_name = "Wiring proof $run"
            isolation_profile = "pooled" } (Provider-Token) "proof-tenant") 201
    $tenantId = $requested.tenant.tenant_id
    $null = Require "its provisioning dispatched" (Call "POST" "$organizationApi/v1/tenants/$tenantId/provisioning" @{
            expected_version = $requested.tenant.version } (Provider-Token) "proof-provision") 200
    $null = Require "its provisioning realized" (Call "POST" "$organizationApi/v1/provisioning/realized" @{
            correlation_id = $requested.correlation_id } (Provider-Token) "proof-realized") 200
    $current = Require "the Tenant read" (Call "GET" "$organizationApi/v1/tenants/$tenantId" $null (Provider-Token) $null) 200
    $null = Require "the Tenant activated" (Call "POST" "$organizationApi/v1/tenants/$tenantId/activate" @{
            expected_version = $current.version } (Provider-Token) "proof-activate") 200
    $administrator = Require "the operator granted its first administrator" (Call "POST" "$organizationApi/v1/tenants/$tenantId/administrators" @{
            principal_id = $s.operator_principal_id } (Provider-Token) "proof-administrator") 201
    Expect "with a Tenant-wide Membership" $administrator.membership_created $true

    # The Tenant administrator's client: tenant-scoped privileged, which holds identity-kernel's
    # scnehaux-privileged scope and organization as an optional one (TDD-identity-control-003 1.29.0),
    # naming this API alone. A provider's to register.
    $jwk = New-Key $tenantClient $keyDir
    $null = Require "a tenant-scoped privileged client registered in identity-control" (Call "POST" "$identityApi/v1/registrations" @{
            client_key = $tenantClient; profile = "confidential"; audience_class = "privileged"
            privileged_form = "tenant-scoped"; application_ref = "development-operator"
            audience = @("organization-control-api"); redirect_uris = @("http://127.0.0.1:8099/callback")
            public_key = $jwk } (Provider-Token) "proof-tenant-client") 201

    # The operator's own Membership has to reach the kernel's Organization before the kernel will
    # issue a token for the Tenant. Waited on through the Admin API rather than by signing in, because
    # every refused sign-in spends a TOTP step.
    $operatorUser = @(Users-Carrying $s.operator_principal_id)[0].id
    $null = Wait-For "the administrator's Membership reaches the Tenant's Organization in the kernel" 120 {
        @(Organization-Members $tenantId) -contains $operatorUser
    }
    $claims = $null
    for ($attempt = 1; $attempt -le 3 -and $null -eq $claims; $attempt++) {
        try { $claims = Decode-Claims (Tenant-Token) } catch {
            Write-Host "        sign-in for the Tenant refused (attempt $attempt): $($_.Exception.Message)"
            Start-Sleep -Seconds 5
        }
    }
    if ($null -eq $claims) { throw "the Tenant administrator could not sign in for the Tenant" }
    Expect "the administrator's token names the Tenant" (Get-Prop $claims "tenant_id") $tenantId
    Expect "at aal2" (Get-Prop $claims "acr") "aal2"

    $person = Require "a second Principal created in identity-control" (Call "POST" "$identityApi/v1/principals" @{
            username = "wiring.member.$run"; email = "wiring.member.$run@scnehaux.local"; subject_type = "human"
        } (Provider-Token) "proof-member") 201
    $memberPrincipal = $person.principal_id
    $granted = Require "the Tenant administrator grants it a Membership here" (Grant-Member $memberPrincipal "proof-grant") 201
    $memberId = $granted.membership.membership_id
    Expect "the Membership is in the administrator's Tenant" $granted.membership.tenant_id $tenantId
    $null = Wait-For "identity-control's Tenant context report lists the Membership at version 1" 60 {
        (Report-Version $memberId) -eq 1
    }
    $memberUser = @(Users-Carrying $memberPrincipal)[0].id
    $null = Wait-For "and the member is in the Tenant's Organization in the kernel" 90 {
        @(Organization-Members $tenantId) -contains $memberUser
    }

    Write-Host ""
    Write-Host "5. the Membership survives a relink of its Principal"
    Expect "the console administrator deletes the member's Keycloak user" (Kc "DELETE" "/users/$memberUser").code 204
    Expect "identity-control's Principal sweep runs" (Call "POST" "$identityApi/v1/principals:reconcile" $null (Provider-Token) $null).code 200
    $relinked = Call "POST" "$identityApi/v1/principals/$($memberPrincipal):relink" $null (Provider-Token) "proof-relink"
    Expect "an operator relinks the Principal" $relinked.code 200
    Expect "active again" (Get-Prop $relinked.json "state") "active"
    $carriers = @(Users-Carrying $memberPrincipal)
    Expect "exactly one user carries the same principal_id" $carriers.Count 1
    $newUser = if ($carriers.Count -gt 0) { $carriers[0].id } else { $null }
    Expect "and it is a new user" ($null -ne $newUser -and $newUser -ne $memberUser) $true
    $after = Call "GET" "$organizationApi/v1/memberships/$memberId" $null (Tenant-Token) $null
    Expect "the Membership here still answers" $after.code 200
    Expect "for the same principal_id" (Get-Prop $after.json "principal_id") $memberPrincipal
    Expect "still active" (Get-Prop $after.json "status") "active"
    Expect "at the same version" (Get-Prop $after.json "version") 1
    Expect "identity-control's report still lists it at the same version" (Report-Version $memberId) 1
    # A relink writes identity tables only and marks no Tenant, so the Organization takes the new user
    # at the Tenant's next convergence. A further grant is the next event for the Tenant.
    $null = Require "a further grant marks the Tenant for convergence" (Grant-Member (New-UuidV7) "proof-trigger") 201
    $null = Wait-For "the relinked user joins the Tenant's Organization" 90 {
        $null -ne $newUser -and (@(Organization-Members $tenantId) -contains $newUser)
    }
    Expect "and the deleted user is not a member" (@(Organization-Members $tenantId) -contains $memberUser) $false

    Write-Host ""
    Write-Host "6. the delay of priority events, on one host's clock"
    # Each revocation is a priority event (membership.security.revoked). Measured from the accepted
    # instant the service stamps inside the transaction (membership_event.recorded_at), on the host
    # clock the containers and this runner share:
    #   - to the response's arrival here, which follows the commit, so it bounds accept to outbox
    #     commit from above. No timestamp here is the commit's own: outbox and receipt columns default
    #     to statement or transaction time, not commit time;
    #   - to publication, platform.outbox_delivery.published_at, which this service's dispatcher
    #     stamps after identity-control acknowledged the delivery as applied;
    #   - to identity-control's consumer_applied receipt, platform.delivery_receipt.recorded_at.
    $count = 20
    $budgetCommitMs = 100       # accept to outbox commit, TDD-organization-control-002 §Enforcement Budget
    $budgetPropagationMs = 10000 # the propagation subtotal, the enforcement read's budget_seconds
    $targets = @()
    for ($i = 0; $i -lt $count; $i++) {
        $g = Grant-Member (New-UuidV7) "proof-timed-grant-$i"
        if ($g.code -ne 201) { throw "a timed grant answered $($g.code): $($g.text)" }
        $targets += $g.json.membership.membership_id
    }
    $ack = @(); $published = @(); $applied = @(); $unenforced = 0
    $revoked = @()
    foreach ($target in $targets) {
        $r = Call "POST" "$organizationApi/v1/memberships/$target/revoke" @{ expected_version = 1 } (Tenant-Token) "proof-timed-revoke-$target"
        if ($r.code -ne 200) { throw "a timed revocation answered $($r.code): $($r.text)" }
        $acceptedText = [regex]::Match($r.text, '"accepted_at":"([^"]+)"').Groups[1].Value
        $ack += ($r.at - (Instant $acceptedText)).TotalMilliseconds
        $revoked += $target
    }
    foreach ($target in $revoked) {
        $evidence = $null
        $deadline = (Get-Date).AddSeconds(30)
        while ((Get-Date) -lt $deadline) {
            $e = Call "GET" "$organizationApi/v1/memberships/$target/enforcement" $null (Tenant-Token) $null
            if ($e.code -eq 200 -and (Get-Prop $e.json "state") -eq "enforced") { $evidence = $e; break }
            Start-Sleep -Milliseconds 500
        }
        if ($null -eq $evidence) { $unenforced++; continue }
        $accepted = Instant ([regex]::Match($evidence.text, '"accepted_at":"([^"]+)"').Groups[1].Value)
        $publication = [regex]::Match($evidence.text, '"published_at":"([^"]+)"').Groups[1].Value
        if ($publication) { $published += ((Instant $publication) - $accepted).TotalMilliseconds }
        $receipt = [regex]::Match($evidence.text,
            '"consumer_id":"identity-control","evidence":"consumer_applied","recorded_at":"([^"]+)"').Groups[1].Value
        if ($receipt) { $applied += ((Instant $receipt) - $accepted).TotalMilliseconds }
    }
    Expect "every revocation enforced at identity-control within 30 s" $unenforced 0
    Expect "every enforced revocation carries identity-control's applied receipt" $applied.Count ($count - $unenforced)
    function Line([string] $label, [double[]] $values) {
        if ($values.Count -eq 0) { return "$label n/a" }
        return "$label p50 $([math]::Round((Rank $values 50))) ms, p95 $([math]::Round((Rank $values 95))) ms, max $([math]::Round((Rank $values 100))) ms"
    }
    $summary = "priority-event delay over $count revocations: " +
        (Line "accept to acknowledgement (bounds accept to commit, budget $budgetCommitMs ms)" $ack) + "; " +
        (Line "accept to publication" $published) + "; " +
        (Line "accept to identity-control applied (budget $budgetPropagationMs ms)" $applied)
    Write-Host "        $summary"
    if ($env:GITHUB_STEP_SUMMARY) { Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value "- $summary" }
    # The acknowledgement adds the reply's encoding and a loopback round trip through docker-proxy
    # to the commit it bounds, so the 95th percentile is held to the budget and the maximum is
    # reported beside it. The propagation subtotal is held at the maximum: nothing is added to it.
    Expect "accept to acknowledgement within the 100 ms accept-to-commit budget at p95" ((Rank $ack 95) -le $budgetCommitMs) $true
    Expect "accept to identity-control applied within the 10 s propagation budget" `
        ($applied.Count -gt 0 -and (Rank $applied 100) -le $budgetPropagationMs) $true
} catch {
    Write-Host "  FAIL  $($_.Exception.Message)"
    $failures++
}

Write-Host ""
Write-Host "7. an Organization outage: stale, and the emergency grant still honored"
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
