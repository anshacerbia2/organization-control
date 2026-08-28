// Declarative desired state for the Organization Database, per ADR-GLB-004.
//
// SCOPE: this file describes the schemas this service owns. The `platform` schema is NOT
// declared here, and that omission is deliberate rather than incomplete: it ships inside
// foundation-platform as versioned SQL applied by `cmd/organization-migrate`. Re-authoring
// it in HCL would fork the schema away from the Go code that queries it, which is the one
// failure the shared module exists to prevent.
//
// `atlas.hcl` scopes every command to the schemas below. Unscoped, Atlas reads `platform`
// as drift and plans to drop it — the same reasoning that produced a
// `DROP SCHEMA "public" CASCADE` the first time identity-control generated a plan.
//
// Atlas creates the schemas here, unlike identity-control where `-stage=pre` does. The
// difference is forced: Atlas rejects a multi-schema HCL source against a schema-scoped dev
// URL, so this project works in database scope, where a plan may create a schema. Creating them
// in the pre stage as well made Atlas refuse to apply at all -- an empty schema it did not
// create counts as an unclean database.
//
// # Why eight schemas rather than one
//
// `ADR-GLB-007` puts each bounded context behind its own boundary, and
// `TDD-organization-control-001` classifies RLS per schema. One schema per context makes the
// RLS set a property of the namespace rather than a list somebody maintains: the structural
// test asserts that every table in an RLS schema is protected, so a table added to
// `membership` without a policy fails the build instead of sitting unprotected.

// `public` is declared and left empty on purpose.
//
// Atlas works in database scope for a multi-schema HCL source, so a schema that exists in the
// database and is absent from this file reads as drift: the first plan generated here ended in
// `DROP SCHEMA "public" CASCADE`, and neither `schemas` nor `exclude` prevented it. Declaring it
// empty says "this exists and holds nothing", which is both true and the state we want -- a table
// appearing in `public` is drift, and now Atlas reports it as such.
//
// `platform` is handled the other way, by exclusion: it is real, it holds tables this file must
// not describe, and declaring it empty would plan to drop them.
schema "public" {}

schema "organization" {
  comment = "Enterprise parties. NOT tenant-scoped: an Organization sponsors many Tenants."
}
schema "tenant" {
  comment = "Tenant identity and lifecycle. RLS-protected."
}
schema "workspace" {
  comment = "Workspace identity and lifecycle inside one Tenant. RLS-protected."
}
schema "membership" {
  comment = "Principal-to-context relationships. RLS-protected."
}
schema "invitation" {
  comment = "Membership invitation intent. RLS-protected."
}
schema "operation" {
  comment = "Lifecycle operations and offboarding obligations. RLS-protected."
}
schema "projection" {
  comment = "Consumer registry and cursors. Operational state with no tenant column."
}

// ---------------------------------------------------------------------------------------------
// organization — NOT tenant-scoped, and the one classification most likely to be misread.
// ---------------------------------------------------------------------------------------------
//
// An Organization sponsors several Tenants, so scoping it to one would be wrong.
// TDD-organization-control-001 states this explicitly so a future reviewer does not "fix" it
// by adding a policy that breaks the model. Its protection is provider scope plus application
// authorization.

table "organization" {
  schema  = schema.organization
  comment = "Enterprise party: provider, customer, partner, or publisher. Not contained by a Tenant."

  column "organization_id" {
    null = false
    type = uuid
  }
  column "display_name" {
    null = false
    type = text
  }
  column "classification" {
    null = false
    type = text
  }
  column "status" {
    null = false
    type = text
  }
  // A provider Organization may hold customer Organizations beneath it. Self-referential
  // rather than a separate hierarchy table: the relationship is one parent at most.
  column "parent_id" {
    null = true
    type = uuid
  }
  column "version" {
    null    = false
    type    = bigint
    default = 1
  }
  column "created_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "updated_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }

  primary_key {
    columns = [column.organization_id]
  }

  foreign_key "organization_parent_fk" {
    columns     = [column.parent_id]
    ref_columns = [column.organization_id]
  }

  check "organization_classification_check" {
    expr = "classification IN ('provider', 'customer', 'partner', 'publisher')"
  }
  check "organization_status_check" {
    expr = "status IN ('active', 'suspended', 'retired')"
  }
}

table "external_reference" {
  schema  = schema.organization
  comment = "Bounded reference to an identifier another authority owns. Never a second source of truth."

  column "organization_id" {
    null = false
    type = uuid
  }
  column "authority" {
    null    = false
    type    = text
    comment = "The domain that owns the identifier: subscription, client-contract, hcm."
  }
  column "external_id" {
    null = false
    type = text
  }

  // One reference per authority. A second row for the same authority would mean two
  // answers to "which Subscriber Account is this", which PAD-PLT-002 §3.3 invariant 1
  // exists to prevent.
  primary_key {
    columns = [column.organization_id, column.authority]
  }

  foreign_key "external_reference_organization_fk" {
    columns     = [column.organization_id]
    ref_columns = [table.organization.column.organization_id]
  }
}

// ---------------------------------------------------------------------------------------------
// tenant — RLS. A tenant-scoped caller sees exactly one row: its own.
// ---------------------------------------------------------------------------------------------

table "tenant" {
  schema  = schema.tenant
  comment = "Technical isolation, configuration, data, and operating boundary. RLS-protected."

  // The primary key doubles as the RLS discriminator, so the policy predicate is
  // `tenant_id = current_setting('app.tenant_id')` on this table as on every other.
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "organization_id" {
    null = false
    type = uuid
  }
  column "display_name" {
    null = false
    type = text
  }
  column "status" {
    null = false
    type = text
  }
  column "isolation_profile" {
    null    = false
    type    = text
    comment = "Governed isolation requirement referenced by runtime and data systems, not enforced here."
  }
  column "residency_region" {
    null = true
    type = text
  }
  // The monotonic contextual-access version consumers compare a token against.
  // STD-IAM-002 §3.5 rule 8 rejects a token whose version is below the local projection's,
  // which is what lets a revocation take effect before the token expires.
  column "tenant_security_version" {
    null    = false
    type    = bigint
    default = 1
  }
  column "version" {
    null    = false
    type    = bigint
    default = 1
  }
  column "activated_at" {
    null = true
    type = timestamptz
  }
  column "suspended_at" {
    null = true
    type = timestamptz
  }
  column "offboarding_started_at" {
    null = true
    type = timestamptz
  }
  column "retired_at" {
    null = true
    type = timestamptz
  }
  column "created_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "updated_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }

  primary_key {
    columns = [column.tenant_id]
  }

  foreign_key "tenant_organization_fk" {
    columns     = [column.organization_id]
    ref_columns = [table.organization.column.organization_id]
  }

  check "tenant_status_check" {
    expr = "status IN ('requested','provisioning','active','failed','suspended','offboarding','retired')"
  }
  check "tenant_isolation_check" {
    expr = "isolation_profile IN ('pooled', 'bridge', 'silo', 'regional')"
  }

  // Provider administration lists Tenants by sponsoring Organization. Tenant-scoped traffic
  // never needs it, so it exists for the provider path alone.
  index "tenant_organization_idx" {
    columns = [column.organization_id]
  }
}

table "provisioning_request" {
  schema  = schema.tenant
  comment = "Desired provisioning state sent outward, and the realized state reported back. RLS-protected."

  column "request_id" {
    null = false
    type = uuid
  }
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "desired_profile" {
    null = false
    type = jsonb
  }
  column "state" {
    null = false
    type = text
  }
  column "correlation_id" {
    null = false
    type = uuid
  }
  column "requested_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "resolved_at" {
    null = true
    type = timestamptz
  }
  column "detail" {
    null = true
    type = text
  }

  primary_key {
    columns = [column.request_id]
  }

  foreign_key "provisioning_request_tenant_fk" {
    columns     = [column.tenant_id]
    ref_columns = [table.tenant.column.tenant_id]
  }

  check "provisioning_state_check" {
    expr = "state IN ('requested', 'realized', 'failed', 'unresolved')"
  }

  // tenant_id leads every access index on an RLS table, so the policy predicate is
  // satisfied by the index rather than by a filter after the scan.
  index "provisioning_request_tenant_idx" {
    columns = [column.tenant_id, column.requested_at]
  }
}

// ---------------------------------------------------------------------------------------------
// workspace — RLS.
// ---------------------------------------------------------------------------------------------

table "workspace" {
  schema  = schema.workspace
  comment = "Collaboration or operating context inside exactly one Tenant. RLS-protected."

  column "workspace_id" {
    null = false
    type = uuid
  }
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "display_name" {
    null = false
    type = text
  }
  column "workspace_type" {
    null = false
    type = text
  }
  column "status" {
    null = false
    type = text
  }
  column "version" {
    null    = false
    type    = bigint
    default = 1
  }
  column "created_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "updated_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }

  primary_key {
    columns = [column.workspace_id]
  }

  foreign_key "workspace_tenant_fk" {
    columns     = [column.tenant_id]
    ref_columns = [table.tenant.column.tenant_id]
  }

  check "workspace_status_check" {
    expr = "status IN ('active', 'archived', 'retired')"
  }

  // The target of the composite foreign keys on membership and invitation. Without it
  // those constraints cannot be created, and the same-Tenant invariant on a referenced
  // Workspace is unenforced. TDD-002 and TDD-003 both specify it.
  index "workspace_tenant_scope_unique" {
    unique  = true
    columns = [column.tenant_id, column.workspace_id]
  }
}

// ---------------------------------------------------------------------------------------------
// membership — RLS. The authority PAD-PLT-002 exists to hold.
// ---------------------------------------------------------------------------------------------

table "membership" {
  schema  = schema.membership
  comment = "Principal or workload relationship to a Tenant and optional Workspace. RLS-protected."

  column "membership_id" {
    null = false
    type = uuid
  }
  // The canonical identifier Identity owns. Stored as a plain uuid with no foreign key:
  // ADR-ORG-001 §5.3 keeps Principal authority in Identity, and a foreign key would need a
  // cross-domain table that EAD-003 prohibits.
  column "principal_id" {
    null = false
    type = uuid
  }
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "workspace_id" {
    null = true
    type = uuid
  }
  column "subject_type" {
    null = false
    type = text
  }
  column "status" {
    null = false
    type = text
  }
  // Monotonic per Membership. A consumer rejects a token carrying a lower value, which is
  // how a revocation reaches enforcement before the token expires.
  column "membership_version" {
    null    = false
    type    = bigint
    default = 1
  }
  column "valid_from" {
    null = false
    type = timestamptz
  }
  column "valid_until" {
    null = true
    type = timestamptz
  }
  column "provenance" {
    null    = false
    type    = text
    comment = "How this Membership came to exist: invitation, migration, provider grant."
  }
  column "created_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "updated_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }

  primary_key {
    columns = [column.membership_id]
  }

  foreign_key "membership_tenant_fk" {
    columns     = [column.tenant_id]
    ref_columns = [table.tenant.column.tenant_id]
  }

  // A referenced Workspace must belong to this Membership's Tenant. PostgreSQL's default
  // MATCH SIMPLE satisfies the constraint when workspace_id is NULL, which is the
  // tenant-only Membership case rather than an exception to the invariant.
  foreign_key "membership_workspace_in_tenant" {
    columns     = [column.tenant_id, column.workspace_id]
    ref_columns = [table.workspace.column.tenant_id, table.workspace.column.workspace_id]
  }

  check "membership_status_check" {
    expr = "status IN ('active', 'suspended', 'revoked')"
  }
  check "membership_subject_check" {
    expr = "subject_type IN ('human', 'workload')"
  }

  // One active Membership per subject, context, and type. COALESCE folds the tenant-only
  // case into the same index, so a Principal cannot hold both a tenant-wide and a
  // duplicate tenant-wide Membership.
  index "membership_active_unique" {
    unique = true
    on {
      column = column.principal_id
    }
    on {
      column = column.tenant_id
    }
    on {
      expr = "COALESCE(workspace_id, tenant_id)"
    }
    on {
      column = column.subject_type
    }
    where = "status = 'active'"
  }

  // The projection lookup: every consumer resolves a Principal's Membership inside one
  // Tenant, so tenant_id leads and principal_id follows.
  index "membership_tenant_principal_idx" {
    columns = [column.tenant_id, column.principal_id]
  }
}

// ---------------------------------------------------------------------------------------------
// invitation — RLS.
// ---------------------------------------------------------------------------------------------

table "invitation" {
  schema  = schema.invitation
  comment = "Intent to establish a future Membership. Not an identity proof. RLS-protected."

  column "invitation_id" {
    null = false
    type = uuid
  }
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "workspace_id" {
    null = true
    type = uuid
  }
  column "target_identifier" {
    null    = false
    type    = text
    comment = "Tier-2 identifiable PII under STD-GLB-007, encrypted at rest with the database."
  }
  // The correlation key, so acceptance does not require querying by the plaintext identifier.
  // It is not what the invitee presents: see token_hash below.
  column "target_hash" {
    null = false
    type = text
  }
  // The hash of the token the invitee carries. The token itself is returned once, at creation,
  // and is never stored.
  //
  // ADDITION to TDD-organization-control-004, whose table declared only target_hash while its
  // §"Acceptance, and Enumeration Resistance" states that "the token space is the only thing
  // protecting the flow". An identifier is not a token space, so without this column the thing
  // the invitee presents would have to be `invitation_id` — a UUIDv7, which is time-ordered.
  // An attacker who knows roughly when a Tenant issued invitations can narrow that space
  // sharply, and a uniform response closes the information leak without closing a successful
  // guess.
  //
  // Unique across the table rather than per Tenant: a token is resolved before any Tenant is
  // known, so it must identify at most one invitation on its own.
  column "token_hash" {
    null = false
    type = text
  }
  column "subject_type" {
    null = false
    type = text
  }
  column "invited_by" {
    null = false
    type = uuid
  }
  column "reason" {
    null = true
    type = text
  }
  column "state" {
    null = false
    type = text
  }
  column "correlation_id" {
    null = false
    type = uuid
  }
  // Null until the invited party has a Principal. Populated on identity verification, which
  // is why the state machine carries `identity_verified` between pending and accepted.
  column "principal_id" {
    null = true
    type = uuid
  }
  column "expires_at" {
    null = false
    type = timestamptz
  }
  column "created_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "accepted_at" {
    null = true
    type = timestamptz
  }
  column "revoked_at" {
    null = true
    type = timestamptz
  }

  primary_key {
    columns = [column.invitation_id]
  }

  foreign_key "invitation_tenant_fk" {
    columns     = [column.tenant_id]
    ref_columns = [table.tenant.column.tenant_id]
  }

  foreign_key "invitation_workspace_in_tenant" {
    columns     = [column.tenant_id, column.workspace_id]
    ref_columns = [table.workspace.column.tenant_id, table.workspace.column.workspace_id]
  }

  check "invitation_state_check" {
    expr = "state IN ('pending', 'identity_verified', 'accepted', 'expired', 'revoked')"
  }
  check "invitation_subject_check" {
    expr = "subject_type IN ('human', 'workload')"
  }

  // One outstanding invitation per target and context. Two pending invitations for the same
  // person would produce two Memberships from one intent.
  index "invitation_pending_unique" {
    unique = true
    on {
      column = column.tenant_id
    }
    on {
      column = column.target_hash
    }
    on {
      expr = "COALESCE(workspace_id, tenant_id)"
    }
    where = "state IN ('pending', 'identity_verified')"
  }

  // A token resolves to at most one invitation, and it is resolved before any Tenant is known.
  index "invitation_token_hash_unique" {
    unique  = true
    columns = [column.token_hash]
  }

  // The expiry sweep reads by state and expiry inside a Tenant.
  index "invitation_tenant_expiry_idx" {
    columns = [column.tenant_id, column.expires_at]
  }
}

// ---------------------------------------------------------------------------------------------
// operation — RLS.
// ---------------------------------------------------------------------------------------------

table "offboarding" {
  schema  = schema.operation
  comment = "Coordinated Tenant access freeze, obligation tracking, and retirement. RLS-protected."

  column "offboarding_id" {
    null = false
    type = uuid
  }
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "stage" {
    null = false
    type = text
  }
  column "initiated_by" {
    null = false
    type = uuid
  }
  column "reason" {
    null = false
    type = text
  }
  column "legal_hold" {
    null    = false
    type    = boolean
    default = false
  }
  column "correlation_id" {
    null = false
    type = uuid
  }
  column "started_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  column "frozen_at" {
    null = true
    type = timestamptz
  }
  column "retired_at" {
    null = true
    type = timestamptz
  }

  primary_key {
    columns = [column.offboarding_id]
  }

  foreign_key "offboarding_tenant_fk" {
    columns     = [column.tenant_id]
    ref_columns = [table.tenant.column.tenant_id]
  }

  check "offboarding_stage_check" {
    expr = "stage IN ('freeze', 'obligations', 'release', 'retired')"
  }

  // The target of the composite foreign key on offboarding_obligation, so a child row's
  // denormalized tenant_id cannot disagree with its parent's.
  index "offboarding_tenant_scope_unique" {
    unique  = true
    columns = [column.tenant_id, column.offboarding_id]
  }
}

table "offboarding_obligation" {
  schema  = schema.operation
  comment = "One domain's outstanding obligation during offboarding. RLS-protected."

  column "obligation_id" {
    null = false
    type = uuid
  }
  column "offboarding_id" {
    null = false
    type = uuid
  }
  // TDD-organization-control-004 §"Why the obligation carries tenant_id", from v1.2.0.
  //
  // Version 1.1.0 of that design declared this table without one, contradicting
  // TDD-organization-control-001, which requires a non-nullable tenant_id on every table in
  // an RLS schema and has its structural test reject one that does not. The rule was the one
  // that survived and the design was corrected: RLS evaluates a predicate per row and cannot
  // follow a join, so a policy on the parent protects nothing here. A child reachable only
  // through a protected path is not a protected child.
  //
  // Denormalized rather than joined, and the composite foreign key below is what keeps the
  // copy honest: the pair must exist on the parent, so a row cannot claim a Tenant its
  // offboarding does not belong to.
  column "tenant_id" {
    null = false
    type = uuid
  }
  column "domain" {
    null    = false
    type    = text
    comment = "The accountable domain: product, hcm, billing, audit."
  }
  column "obligation_type" {
    null = false
    type = text
  }
  column "state" {
    null = false
    type = text
  }
  column "due_at" {
    null = true
    type = timestamptz
  }
  column "completed_at" {
    null = true
    type = timestamptz
  }
  column "detail" {
    null = true
    type = text
  }

  primary_key {
    columns = [column.obligation_id]
  }

  foreign_key "offboarding_obligation_parent_fk" {
    columns     = [column.tenant_id, column.offboarding_id]
    ref_columns = [table.offboarding.column.tenant_id, table.offboarding.column.offboarding_id]
  }

  check "obligation_state_check" {
    expr = "state IN ('open', 'completed', 'waived', 'failed')"
  }

  index "offboarding_obligation_tenant_idx" {
    columns = [column.tenant_id, column.offboarding_id]
  }
}

// ---------------------------------------------------------------------------------------------
// projection — NOT tenant-scoped. Operational state with no tenant column.
// ---------------------------------------------------------------------------------------------

table "consumer" {
  schema  = schema.projection
  comment = "Consumer registry: declared projection version, freshness budget, and stale behavior."

  column "consumer_id" {
    null = false
    type = text
  }
  column "projection_version" {
    null = false
    type = text
  }
  // ADR-ORG-001 §5.7 requires each consumer to declare its freshness budget and what it does
  // when stale. Held here rather than in the consumer, because the publisher is what measures
  // whether the budget is being met.
  column "max_accepted_age" {
    null = false
    type = sql("interval")
  }
  column "stale_behavior" {
    null = false
    type = text
  }
  column "registered_at" {
    null    = false
    type    = timestamptz
    default = sql("now()")
  }
  // The high-water mark of the snapshot this consumer bootstrapped from.
  //
  // ADDITION to TDD-organization-control-002 §"Consumer Registry", which declares the table
  // without it while §"Bootstrap Contract" requires the registry to refuse a progress report
  // whose snapshot mark is absent. That rule needs somewhere to read the mark from, so the
  // column is the enforcement of a rule the design already states rather than a new one.
  //
  // NULL means no snapshot has been taken. A consumer in that state has subscribed and holds
  // everything that happened since it connected and nothing before, so accepting a position
  // for it would record an incomplete model as a current one.
  column "snapshot_mark" {
    null = true
    type = bigint
  }
  column "last_reported_at" {
    null = true
    type = timestamptz
  }
  column "last_reported_mark" {
    null = true
    type = bigint
  }

  // The three columns below carry the `:verify` misuse signal.
  //
  // ADDITION to TDD-organization-control-002, which requires the rate to be measured per
  // consumer and declares neither the counter nor the denominator. The signal is calls *per
  // request*, and that ratio cannot be computed by either side alone: this service is the only
  // party that knows how many fresh checks a consumer made, and the consumer is the only party
  // that knows how many requests it served. So the numerator is counted here and the
  // denominator arrives with the progress report the consumer already sends on its declared
  // cadence — no new mechanism, one number from each side.
  //
  // Both counters reset at every report, which makes the ratio per-interval rather than
  // lifetime. A lifetime ratio dilutes forever: a consumer that misused the path for a day and
  // then fixed it would read as healthy a month later, and one running for years could never
  // trip the threshold at all.
  column "verify_calls_since_report" {
    null    = false
    type    = bigint
    default = 0
  }
  column "last_reported_requests" {
    null    = true
    type    = bigint
    comment = "Requests the consumer served in the interval it last reported. The denominator."
  }
  column "last_verify_ratio" {
    null    = true
    type    = sql("double precision")
    comment = "verify_calls_since_report / last_reported_requests at the last report. NULL until a report supplies a denominator."
  }

  primary_key {
    columns = [column.consumer_id]
  }

  check "stale_behavior_check" {
    expr = "stale_behavior IN ('use_with_marker', 'revalidate', 'fail_closed')"
  }

  // A negative count is not a low ratio, it is a broken counter. Constrained here so a defect
  // surfaces as a refused write rather than as a consumer that looks well behaved.
  check "verify_counters_non_negative" {
    expr = "verify_calls_since_report >= 0 AND (last_reported_requests IS NULL OR last_reported_requests >= 0)"
  }
}
