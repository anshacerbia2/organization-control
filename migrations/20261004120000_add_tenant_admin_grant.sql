-- Create "tenant_admin_grant" table
CREATE TABLE "membership"."tenant_admin_grant" (
  "grant_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "principal_id" uuid NOT NULL,
  "granted_by" uuid NOT NULL,
  "reason" text NOT NULL,
  "granted_at" timestamptz NOT NULL DEFAULT now(),
  "revoked_at" timestamptz NULL,
  "revoked_by" uuid NULL,
  "revoke_reason" text NULL,
  PRIMARY KEY ("grant_id"),
  CONSTRAINT "tenant_admin_grant_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "tenant_admin_grant_reason_check" CHECK (btrim(reason) <> ''::text),
  CONSTRAINT "tenant_admin_grant_revocation_check" CHECK (((revoked_at IS NULL) = (revoked_by IS NULL)) AND ((revoked_at IS NULL) = (revoke_reason IS NULL)) AND ((revoke_reason IS NULL) OR (btrim(revoke_reason) <> ''::text)))
);
-- Create index "tenant_admin_grant_active" to table: "tenant_admin_grant"
CREATE UNIQUE INDEX "tenant_admin_grant_active" ON "membership"."tenant_admin_grant" ("principal_id", "tenant_id") WHERE (revoked_at IS NULL);
-- Set comment to table: "tenant_admin_grant"
COMMENT ON TABLE "membership"."tenant_admin_grant" IS 'Tenant administration grants (ADR-ORG-003). Written by a provider only. RLS-protected.';
