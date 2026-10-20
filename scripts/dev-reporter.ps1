# The development reporters' tokens. Dot-source it after identity-control's scripts/dev-token.ps1, which
# loads New-ClientAssertion, Get-RealmIssuer and Get-DevKernelUrl, and call Get-ReporterToken.
#
# A reporter is a workload this service registered to report one kind of outcome (ADR-ORG-001 §5.11,
# amended 2026-10-20). On a development server the stand-ins of STD-GLB-009 rule 13 (1.10.0) report as
# development reporter workloads with their own keys, so development runs production's path:
# dev-provisioning.ps1 and dev-deprovisioning.ps1 as the provisioning reporter, dev-obligation.ps1 as
# the reporter of the obligation's domain. scripts/dev-wire.ps1 step 7 creates them and writes each one's
# client and key file into the wiring state file.
#
# The token comes from the client credentials grant, authenticated by a client assertion signed with
# the workload's own key: RFC 6749 §4.4, "The client credentials grant type MUST only be used by
# confidential clients", and RFC 7523 §2.2. The key never leaves its file and is never printed.

Set-StrictMode -Version Latest

# Get-ReporterEntry reads the state file's entry for what a reporter reports ("provisioning" or
# "obligation:<domain>"), or $null when dev-wire.ps1 registered none for it.
function Get-ReporterEntry($state, [string] $reports) {
    if (-not ($state.PSObject.Properties.Name -contains "reporters")) { return $null }
    $entry = $state.reporters.PSObject.Properties | Where-Object { $_.Name -ceq $reports } | Select-Object -First 1
    if ($null -eq $entry) { return $null }
    return $entry.Value
}

# Get-ReporterToken obtains an access token for the reporter registered for $reports.
function Get-ReporterToken($state, [string] $reports) {
    $entry = Get-ReporterEntry $state $reports
    if ($null -eq $entry) {
        throw "the wiring state names no development reporter for $reports; scripts/dev-wire.ps1 step 7 registers them"
    }
    $kcBase = if ($env:KC_BASE_URL) { $env:KC_BASE_URL.TrimEnd('/') } else { Get-DevKernelUrl }
    $realm = if ($env:KC_REALM) { $env:KC_REALM } else { "scnehaux" }
    $assertion = New-ClientAssertion -KeyFile $entry.key_file -ClientId $entry.client_id -Audience (Get-RealmIssuer $kcBase $realm)
    $grant = Invoke-RestMethod -Method Post -Uri "$kcBase/realms/$realm/protocol/openid-connect/token" -Body @{
        grant_type            = "client_credentials"
        client_id             = $entry.client_id
        client_assertion_type = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
        client_assertion      = $assertion
    }
    return $grant.access_token
}
