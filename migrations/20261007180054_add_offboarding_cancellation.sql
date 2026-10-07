-- Two CHECK constraints are widened by dropping and re-adding them, in one ALTER TABLE each, which
-- PostgreSQL applies atomically and Atlas runs in one transaction: no moment exists without the
-- check, and no row is dropped. The new sets contain the old ones, so every existing row satisfies
-- them. Reviewed 2026-10-07 (TDD-organization-control-004 1.8.0, ADR-ORG-006).
-- Modify "offboarding_obligation" table
ALTER TABLE "operation"."offboarding_obligation" DROP CONSTRAINT "obligation_state_check", ADD CONSTRAINT "obligation_state_check" CHECK (state = ANY (ARRAY['open'::text, 'completed'::text, 'waived'::text, 'failed'::text, 'cancelled'::text])); -- atlas:destructive-approved: check widened in place, replaced in the same statement
-- Modify "offboarding" table
ALTER TABLE "operation"."offboarding" DROP CONSTRAINT "offboarding_stage_check", ADD CONSTRAINT "offboarding_stage_check" CHECK (stage = ANY (ARRAY['freeze'::text, 'obligations'::text, 'release'::text, 'retired'::text, 'cancelled'::text])), ADD CONSTRAINT "offboarding_cancellation_check" CHECK (((stage = 'cancelled'::text) = (cancelled_at IS NOT NULL)) AND ((cancelled_at IS NULL) = (cancelled_by IS NULL)) AND ((cancelled_at IS NULL) = (cancel_reason IS NULL)) AND ((cancel_reason IS NULL) OR (btrim(cancel_reason) <> ''::text))), ADD CONSTRAINT "offboarding_prior_status_check" CHECK ((prior_status IS NULL) OR (prior_status = ANY (ARRAY['active'::text, 'suspended'::text]))), ADD COLUMN "prior_status" text NULL, ADD COLUMN "cancelled_by" uuid NULL, ADD COLUMN "cancel_reason" text NULL, ADD COLUMN "cancelled_at" timestamptz NULL; -- atlas:destructive-approved: check widened in place, replaced in the same statement
-- Create "offboarding_freeze" table
CREATE TABLE "membership"."offboarding_freeze" (
  "offboarding_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "membership_id" uuid NOT NULL,
  "suspended_at" timestamptz NOT NULL DEFAULT now(),
  "restored_at" timestamptz NULL,
  PRIMARY KEY ("offboarding_id", "membership_id"),
  CONSTRAINT "offboarding_freeze_membership_fk" FOREIGN KEY ("membership_id") REFERENCES "membership"."membership" ("membership_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "offboarding_freeze_offboarding_fk" FOREIGN KEY ("tenant_id", "offboarding_id") REFERENCES "operation"."offboarding" ("tenant_id", "offboarding_id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Create index "offboarding_freeze_tenant_idx" to table: "offboarding_freeze"
CREATE INDEX "offboarding_freeze_tenant_idx" ON "membership"."offboarding_freeze" ("tenant_id", "offboarding_id");
-- Set comment to table: "offboarding_freeze"
COMMENT ON TABLE "membership"."offboarding_freeze" IS 'Memberships an offboarding''s freeze suspended, and when a cancellation restored each. RLS-protected.';
