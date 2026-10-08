-- The privileged-access record, read and reviewed (ADR-ORG-002 §5.6, TDD-organization-control-001 1.21.0).
-- Modify "privileged_access" table
ALTER TABLE "audit"."privileged_access" ADD CONSTRAINT "privileged_access_activation_check" CHECK ((authority = 'activation'::text) = (activation_id IS NOT NULL)), ADD CONSTRAINT "privileged_access_authority_check" CHECK (authority = ANY (ARRAY['emergency'::text, 'activation'::text, 'eligible'::text, 'consumer'::text])), ADD CONSTRAINT "privileged_access_operation_check" CHECK ((operation IS NULL) OR (btrim(operation) <> ''::text)), ADD COLUMN "authority" text NOT NULL, ADD COLUMN "activation_id" uuid NULL, ADD COLUMN "tenant_id" uuid NULL, ADD COLUMN "operation" text NULL;
-- Create index "privileged_access_tenant_idx" to table: "privileged_access"
CREATE INDEX "privileged_access_tenant_idx" ON "audit"."privileged_access" ("tenant_id", "access_id") WHERE (tenant_id IS NOT NULL);
-- Set comment to column: "authority" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."authority" IS 'emergency, activation, eligible or consumer: what admitted the access.';
-- Set comment to column: "activation_id" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."activation_id" IS 'The activation in force, when the authority is one.';
-- Set comment to column: "tenant_id" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."tenant_id" IS 'The one Tenant the access named: the Tenant a provider act binds, or the route path''s.';
-- Set comment to column: "operation" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."operation" IS 'The route pattern, method included, that opened the transaction.';
-- Create "privileged_access_review" table
CREATE TABLE "audit"."privileged_access_review" (
  "review_id" uuid NOT NULL,
  "actor_id" uuid NOT NULL,
  "period_from" timestamptz NOT NULL,
  "period_to" timestamptz NOT NULL,
  "outcome" text NOT NULL,
  "statement" text NOT NULL,
  "accesses" bigint NOT NULL,
  "emergency_accesses" bigint NOT NULL,
  "reviewed_by" uuid NOT NULL,
  "correlation_id" uuid NOT NULL,
  "reviewed_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("review_id"),
  CONSTRAINT "privileged_access_review_count_check" CHECK ((accesses >= 0) AND (emergency_accesses >= 0) AND (emergency_accesses <= accesses)),
  CONSTRAINT "privileged_access_review_outcome_check" CHECK (outcome = ANY (ARRAY['appropriate'::text, 'escalated'::text])),
  CONSTRAINT "privileged_access_review_period_check" CHECK ((period_from < period_to) AND (period_to <= reviewed_at)),
  CONSTRAINT "privileged_access_review_separation_check" CHECK (reviewed_by <> actor_id),
  CONSTRAINT "privileged_access_review_statement_check" CHECK (btrim(statement) <> ''::text)
);
-- Create index "privileged_access_review_actor_idx" to table: "privileged_access_review"
CREATE INDEX "privileged_access_review_actor_idx" ON "audit"."privileged_access_review" ("actor_id", "period_from", "period_to");
-- Set comment to table: "privileged_access_review"
COMMENT ON TABLE "audit"."privileged_access_review" IS 'A provider''s review of another provider''s access over a period. Insert-only. ADR-ORG-002 §5.6.';
-- Set comment to column: "actor_id" on table: "privileged_access_review"
COMMENT ON COLUMN "audit"."privileged_access_review"."actor_id" IS 'The provider whose access is reviewed.';
