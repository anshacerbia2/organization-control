-- Add new schema named "audit"
CREATE SCHEMA "audit";
-- Set comment to schema: "audit"
COMMENT ON SCHEMA "audit" IS 'Evidence about cross-Tenant administration. No tenant column, outside the RLS set.';
-- Create "privileged_access" table
CREATE TABLE "audit"."privileged_access" (
  "access_id" uuid NOT NULL,
  "actor_id" uuid NOT NULL,
  "correlation_id" uuid NOT NULL,
  "reason" text NOT NULL,
  "occurred_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("access_id"),
  CONSTRAINT "privileged_access_reason_present" CHECK (length(btrim(reason)) > 0)
);
-- Create index "privileged_access_actor_idx" to table: "privileged_access"
CREATE INDEX "privileged_access_actor_idx" ON "audit"."privileged_access" ("actor_id", "occurred_at");
-- Create index "privileged_access_correlation_idx" to table: "privileged_access"
CREATE INDEX "privileged_access_correlation_idx" ON "audit"."privileged_access" ("correlation_id");
-- Set comment to table: "privileged_access"
COMMENT ON TABLE "audit"."privileged_access" IS 'Evidence for cross-Tenant provider access: who, correlated to which request, and why.';
-- Set comment to column: "actor_id" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."actor_id" IS 'The administrative subject from the resolved provider scope.';
