-- The count of `extra` findings on the consumer, and the pause of Tenant administration
-- (TDD-organization-control-002 1.15.0 §Reconciliation, TDD-organization-control-001 1.22.0 §Pausing Tenant Administration).
-- Modify "consumer" table
ALTER TABLE "projection"."consumer" ADD COLUMN "last_reconciled_extra_findings" integer NULL;
-- Create "tenant_administration_pause" table
CREATE TABLE "organization"."tenant_administration_pause" (
  "pause_id" uuid NOT NULL,
  "paused" boolean NOT NULL,
  "reason" text NOT NULL,
  "actor_id" uuid NULL,
  "correlation_id" uuid NULL,
  "recorded_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("pause_id"),
  CONSTRAINT "tenant_administration_pause_lift_check" CHECK (paused OR (actor_id IS NOT NULL)),
  CONSTRAINT "tenant_administration_pause_reason_check" CHECK (btrim(reason) <> ''::text)
);
-- Create index "tenant_administration_pause_recorded_idx" to table: "tenant_administration_pause"
CREATE INDEX "tenant_administration_pause_recorded_idx" ON "organization"."tenant_administration_pause" ("recorded_at", "pause_id");
-- Set comment to table: "tenant_administration_pause"
COMMENT ON TABLE "organization"."tenant_administration_pause" IS 'Decisions to pause and lift Tenant administrators'' commands. Append-only; the latest row is the state.';
