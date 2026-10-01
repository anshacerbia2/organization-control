-- Modify "provider_grant" table
ALTER TABLE "organization"."provider_grant" ADD CONSTRAINT "provider_grant_revocation_check" CHECK (((revoked_at IS NULL) = (revoked_by IS NULL)) AND ((revoked_at IS NULL) = (revoke_reason IS NULL)) AND ((revoke_reason IS NULL) OR (btrim(revoke_reason) <> ''::text))), ADD COLUMN "revoked_at" timestamptz NULL, ADD COLUMN "revoked_by" uuid NULL, ADD COLUMN "revoke_reason" text NULL;
-- Drop index "provider_grant_principal_scope" from table: "provider_grant"
DROP INDEX "organization"."provider_grant_principal_scope";
-- Create index "provider_grant_principal_scope" to table: "provider_grant"
CREATE UNIQUE INDEX "provider_grant_principal_scope" ON "organization"."provider_grant" ("principal_id", "scope") WHERE (revoked_at IS NULL);
