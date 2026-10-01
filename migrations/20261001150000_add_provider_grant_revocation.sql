-- Modify "provider_grant" table
ALTER TABLE "organization"."provider_grant" ADD CONSTRAINT "provider_grant_revocation_check" CHECK (((revoked_at IS NULL) = (revoked_by IS NULL)) AND ((revoked_at IS NULL) = (revoke_reason IS NULL)) AND ((revoke_reason IS NULL) OR (btrim(revoke_reason) <> ''::text))), ADD COLUMN "revoked_at" timestamptz NULL, ADD COLUMN "revoked_by" uuid NULL, ADD COLUMN "revoke_reason" text NULL;
-- The unique index becomes partial, so a revoked Principal can be granted again. Replacing it drops
-- an index and no data, and the replacement is created in this same file, which Atlas applies in
-- one transaction: no moment exists without a uniqueness guard. Reviewed 2026-10-01.
-- Replace index "provider_grant_principal_scope" on table "provider_grant" -- atlas:destructive-approved
DROP INDEX "organization"."provider_grant_principal_scope"; -- atlas:destructive-approved: index only, recreated below in the same transaction
-- Create index "provider_grant_principal_scope" to table: "provider_grant"
CREATE UNIQUE INDEX "provider_grant_principal_scope" ON "organization"."provider_grant" ("principal_id", "scope") WHERE (revoked_at IS NULL);
