-- Add new schema named "invitation"
CREATE SCHEMA "invitation";
-- Set comment to schema: "invitation"
COMMENT ON SCHEMA "invitation" IS 'Membership invitation intent. RLS-protected.';
-- Add new schema named "membership"
CREATE SCHEMA "membership";
-- Set comment to schema: "membership"
COMMENT ON SCHEMA "membership" IS 'Principal-to-context relationships. RLS-protected.';
-- Add new schema named "operation"
CREATE SCHEMA "operation";
-- Set comment to schema: "operation"
COMMENT ON SCHEMA "operation" IS 'Lifecycle operations and offboarding obligations. RLS-protected.';
-- Add new schema named "organization"
CREATE SCHEMA "organization";
-- Set comment to schema: "organization"
COMMENT ON SCHEMA "organization" IS 'Enterprise parties. NOT tenant-scoped: an Organization sponsors many Tenants.';
-- Add new schema named "projection"
CREATE SCHEMA "projection";
-- Set comment to schema: "projection"
COMMENT ON SCHEMA "projection" IS 'Consumer registry and cursors. Operational state with no tenant column.';
-- Add new schema named "tenant"
CREATE SCHEMA "tenant";
-- Set comment to schema: "tenant"
COMMENT ON SCHEMA "tenant" IS 'Tenant identity and lifecycle. RLS-protected.';
-- Add new schema named "workspace"
CREATE SCHEMA "workspace";
-- Set comment to schema: "workspace"
COMMENT ON SCHEMA "workspace" IS 'Workspace identity and lifecycle inside one Tenant. RLS-protected.';
-- Create "organization" table
CREATE TABLE "organization"."organization" (
  "organization_id" uuid NOT NULL,
  "display_name" text NOT NULL,
  "classification" text NOT NULL,
  "status" text NOT NULL,
  "parent_id" uuid NULL,
  "version" bigint NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("organization_id"),
  CONSTRAINT "organization_parent_fk" FOREIGN KEY ("parent_id") REFERENCES "organization"."organization" ("organization_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "organization_classification_check" CHECK (classification = ANY (ARRAY['provider'::text, 'customer'::text, 'partner'::text, 'publisher'::text])),
  CONSTRAINT "organization_status_check" CHECK (status = ANY (ARRAY['active'::text, 'suspended'::text, 'retired'::text]))
);
-- Set comment to table: "organization"
COMMENT ON TABLE "organization"."organization" IS 'Enterprise party: provider, customer, partner, or publisher. Not contained by a Tenant.';
-- Create "consumer" table
CREATE TABLE "projection"."consumer" (
  "consumer_id" text NOT NULL,
  "projection_version" text NOT NULL,
  "max_accepted_age" interval NOT NULL,
  "stale_behavior" text NOT NULL,
  "registered_at" timestamptz NOT NULL DEFAULT now(),
  "last_reported_at" timestamptz NULL,
  "last_reported_mark" bigint NULL,
  PRIMARY KEY ("consumer_id"),
  CONSTRAINT "stale_behavior_check" CHECK (stale_behavior = ANY (ARRAY['use_with_marker'::text, 'revalidate'::text, 'fail_closed'::text]))
);
-- Set comment to table: "consumer"
COMMENT ON TABLE "projection"."consumer" IS 'Consumer registry: declared projection version, freshness budget, and stale behavior.';
-- Create "external_reference" table
CREATE TABLE "organization"."external_reference" (
  "organization_id" uuid NOT NULL,
  "authority" text NOT NULL,
  "external_id" text NOT NULL,
  PRIMARY KEY ("organization_id", "authority"),
  CONSTRAINT "external_reference_organization_fk" FOREIGN KEY ("organization_id") REFERENCES "organization"."organization" ("organization_id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Set comment to table: "external_reference"
COMMENT ON TABLE "organization"."external_reference" IS 'Bounded reference to an identifier another authority owns. Never a second source of truth.';
-- Set comment to column: "authority" on table: "external_reference"
COMMENT ON COLUMN "organization"."external_reference"."authority" IS 'The domain that owns the identifier: subscription, client-contract, hcm.';
-- Create "tenant" table
CREATE TABLE "tenant"."tenant" (
  "tenant_id" uuid NOT NULL,
  "organization_id" uuid NOT NULL,
  "display_name" text NOT NULL,
  "status" text NOT NULL,
  "isolation_profile" text NOT NULL,
  "residency_region" text NULL,
  "tenant_security_version" bigint NOT NULL DEFAULT 1,
  "version" bigint NOT NULL DEFAULT 1,
  "activated_at" timestamptz NULL,
  "suspended_at" timestamptz NULL,
  "offboarding_started_at" timestamptz NULL,
  "retired_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("tenant_id"),
  CONSTRAINT "tenant_organization_fk" FOREIGN KEY ("organization_id") REFERENCES "organization"."organization" ("organization_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "tenant_isolation_check" CHECK (isolation_profile = ANY (ARRAY['pooled'::text, 'bridge'::text, 'silo'::text, 'regional'::text])),
  CONSTRAINT "tenant_status_check" CHECK (status = ANY (ARRAY['requested'::text, 'provisioning'::text, 'active'::text, 'failed'::text, 'suspended'::text, 'offboarding'::text, 'retired'::text]))
);
-- Create index "tenant_organization_idx" to table: "tenant"
CREATE INDEX "tenant_organization_idx" ON "tenant"."tenant" ("organization_id");
-- Set comment to table: "tenant"
COMMENT ON TABLE "tenant"."tenant" IS 'Technical isolation, configuration, data, and operating boundary. RLS-protected.';
-- Set comment to column: "isolation_profile" on table: "tenant"
COMMENT ON COLUMN "tenant"."tenant"."isolation_profile" IS 'Governed isolation requirement referenced by runtime and data systems, not enforced here.';
-- Create "workspace" table
CREATE TABLE "workspace"."workspace" (
  "workspace_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "display_name" text NOT NULL,
  "workspace_type" text NOT NULL,
  "status" text NOT NULL,
  "version" bigint NOT NULL DEFAULT 1,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("workspace_id"),
  CONSTRAINT "workspace_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "workspace_status_check" CHECK (status = ANY (ARRAY['active'::text, 'archived'::text, 'retired'::text]))
);
-- Create index "workspace_tenant_scope_unique" to table: "workspace"
CREATE UNIQUE INDEX "workspace_tenant_scope_unique" ON "workspace"."workspace" ("tenant_id", "workspace_id");
-- Set comment to table: "workspace"
COMMENT ON TABLE "workspace"."workspace" IS 'Collaboration or operating context inside exactly one Tenant. RLS-protected.';
-- Create "invitation" table
CREATE TABLE "invitation"."invitation" (
  "invitation_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "workspace_id" uuid NULL,
  "target_identifier" text NOT NULL,
  "target_hash" text NOT NULL,
  "subject_type" text NOT NULL,
  "invited_by" uuid NOT NULL,
  "reason" text NULL,
  "state" text NOT NULL,
  "correlation_id" uuid NOT NULL,
  "principal_id" uuid NULL,
  "expires_at" timestamptz NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "accepted_at" timestamptz NULL,
  "revoked_at" timestamptz NULL,
  PRIMARY KEY ("invitation_id"),
  CONSTRAINT "invitation_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "invitation_workspace_in_tenant" FOREIGN KEY ("tenant_id", "workspace_id") REFERENCES "workspace"."workspace" ("tenant_id", "workspace_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "invitation_state_check" CHECK (state = ANY (ARRAY['pending'::text, 'identity_verified'::text, 'accepted'::text, 'expired'::text, 'revoked'::text])),
  CONSTRAINT "invitation_subject_check" CHECK (subject_type = ANY (ARRAY['human'::text, 'workload'::text]))
);
-- Create index "invitation_pending_unique" to table: "invitation"
CREATE UNIQUE INDEX "invitation_pending_unique" ON "invitation"."invitation" ("tenant_id", "target_hash", (COALESCE(workspace_id, tenant_id))) WHERE (state = ANY (ARRAY['pending'::text, 'identity_verified'::text]));
-- Create index "invitation_tenant_expiry_idx" to table: "invitation"
CREATE INDEX "invitation_tenant_expiry_idx" ON "invitation"."invitation" ("tenant_id", "expires_at");
-- Set comment to table: "invitation"
COMMENT ON TABLE "invitation"."invitation" IS 'Intent to establish a future Membership. Not an identity proof. RLS-protected.';
-- Set comment to column: "target_identifier" on table: "invitation"
COMMENT ON COLUMN "invitation"."invitation"."target_identifier" IS 'Tier-2 identifiable PII under STD-GLB-007, encrypted at rest with the database.';
-- Create "membership" table
CREATE TABLE "membership"."membership" (
  "membership_id" uuid NOT NULL,
  "principal_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "workspace_id" uuid NULL,
  "subject_type" text NOT NULL,
  "status" text NOT NULL,
  "membership_version" bigint NOT NULL DEFAULT 1,
  "valid_from" timestamptz NOT NULL,
  "valid_until" timestamptz NULL,
  "provenance" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("membership_id"),
  CONSTRAINT "membership_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "membership_workspace_in_tenant" FOREIGN KEY ("tenant_id", "workspace_id") REFERENCES "workspace"."workspace" ("tenant_id", "workspace_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "membership_status_check" CHECK (status = ANY (ARRAY['active'::text, 'suspended'::text, 'revoked'::text])),
  CONSTRAINT "membership_subject_check" CHECK (subject_type = ANY (ARRAY['human'::text, 'workload'::text]))
);
-- Create index "membership_active_unique" to table: "membership"
CREATE UNIQUE INDEX "membership_active_unique" ON "membership"."membership" ("principal_id", "tenant_id", (COALESCE(workspace_id, tenant_id)), "subject_type") WHERE (status = 'active'::text);
-- Create index "membership_tenant_principal_idx" to table: "membership"
CREATE INDEX "membership_tenant_principal_idx" ON "membership"."membership" ("tenant_id", "principal_id");
-- Set comment to table: "membership"
COMMENT ON TABLE "membership"."membership" IS 'Principal or workload relationship to a Tenant and optional Workspace. RLS-protected.';
-- Set comment to column: "provenance" on table: "membership"
COMMENT ON COLUMN "membership"."membership"."provenance" IS 'How this Membership came to exist: invitation, migration, provider grant.';
-- Create "offboarding" table
CREATE TABLE "operation"."offboarding" (
  "offboarding_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "stage" text NOT NULL,
  "initiated_by" uuid NOT NULL,
  "reason" text NOT NULL,
  "legal_hold" boolean NOT NULL DEFAULT false,
  "correlation_id" uuid NOT NULL,
  "started_at" timestamptz NOT NULL DEFAULT now(),
  "frozen_at" timestamptz NULL,
  "retired_at" timestamptz NULL,
  PRIMARY KEY ("offboarding_id"),
  CONSTRAINT "offboarding_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "offboarding_stage_check" CHECK (stage = ANY (ARRAY['freeze'::text, 'obligations'::text, 'release'::text, 'retired'::text]))
);
-- Create index "offboarding_tenant_scope_unique" to table: "offboarding"
CREATE UNIQUE INDEX "offboarding_tenant_scope_unique" ON "operation"."offboarding" ("tenant_id", "offboarding_id");
-- Set comment to table: "offboarding"
COMMENT ON TABLE "operation"."offboarding" IS 'Coordinated Tenant access freeze, obligation tracking, and retirement. RLS-protected.';
-- Create "offboarding_obligation" table
CREATE TABLE "operation"."offboarding_obligation" (
  "obligation_id" uuid NOT NULL,
  "offboarding_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "domain" text NOT NULL,
  "obligation_type" text NOT NULL,
  "state" text NOT NULL,
  "due_at" timestamptz NULL,
  "completed_at" timestamptz NULL,
  "detail" text NULL,
  PRIMARY KEY ("obligation_id"),
  CONSTRAINT "offboarding_obligation_parent_fk" FOREIGN KEY ("tenant_id", "offboarding_id") REFERENCES "operation"."offboarding" ("tenant_id", "offboarding_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "obligation_state_check" CHECK (state = ANY (ARRAY['open'::text, 'completed'::text, 'waived'::text, 'failed'::text]))
);
-- Create index "offboarding_obligation_tenant_idx" to table: "offboarding_obligation"
CREATE INDEX "offboarding_obligation_tenant_idx" ON "operation"."offboarding_obligation" ("tenant_id", "offboarding_id");
-- Set comment to table: "offboarding_obligation"
COMMENT ON TABLE "operation"."offboarding_obligation" IS 'One domain''s outstanding obligation during offboarding. RLS-protected.';
-- Set comment to column: "domain" on table: "offboarding_obligation"
COMMENT ON COLUMN "operation"."offboarding_obligation"."domain" IS 'The accountable domain: product, hcm, billing, audit.';
-- Create "provisioning_request" table
CREATE TABLE "tenant"."provisioning_request" (
  "request_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "desired_profile" jsonb NOT NULL,
  "state" text NOT NULL,
  "correlation_id" uuid NOT NULL,
  "requested_at" timestamptz NOT NULL DEFAULT now(),
  "resolved_at" timestamptz NULL,
  "detail" text NULL,
  PRIMARY KEY ("request_id"),
  CONSTRAINT "provisioning_request_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "provisioning_state_check" CHECK (state = ANY (ARRAY['requested'::text, 'realized'::text, 'failed'::text, 'unresolved'::text]))
);
-- Create index "provisioning_request_tenant_idx" to table: "provisioning_request"
CREATE INDEX "provisioning_request_tenant_idx" ON "tenant"."provisioning_request" ("tenant_id", "requested_at");
-- Set comment to table: "provisioning_request"
COMMENT ON TABLE "tenant"."provisioning_request" IS 'Desired provisioning state sent outward, and the realized state reported back. RLS-protected.';
