-- Create "membership_batch" table
CREATE TABLE "membership"."membership_batch" (
  "batch_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "action" text NOT NULL,
  "state" text NOT NULL,
  "reason" text NULL,
  "correlation_id" uuid NOT NULL,
  "continues" uuid NULL,
  "created_by" uuid NOT NULL,
  "created_at" timestamptz NOT NULL,
  "expires_at" timestamptz NOT NULL,
  "would_change" integer NOT NULL,
  "would_not_change" integer NOT NULL,
  "fail_on_errors" integer NULL,
  "executed_by" uuid NULL,
  "executed_at" timestamptz NULL,
  "completed_at" timestamptz NULL,
  PRIMARY KEY ("batch_id"),
  CONSTRAINT "membership_batch_tenant_scope_unique" UNIQUE ("tenant_id", "batch_id"),
  CONSTRAINT "membership_batch_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "membership_batch_action_check" CHECK (action = ANY (ARRAY['suspend'::text, 'restore'::text, 'revoke'::text])),
  CONSTRAINT "membership_batch_allowance_check" CHECK ((fail_on_errors IS NULL) OR (fail_on_errors >= 0)),
  CONSTRAINT "membership_batch_reason_check" CHECK (((reason IS NULL) OR (btrim(reason) <> ''::text)) AND ((action <> 'revoke'::text) OR (reason IS NOT NULL))),
  CONSTRAINT "membership_batch_state_check" CHECK (state = ANY (ARRAY['previewed'::text, 'executing'::text, 'executed'::text]))
);
-- Set comment to table: "membership_batch"
COMMENT ON TABLE "membership"."membership_batch" IS 'Bulk Membership actions, previewed then executed (ADR-ORG-004). RLS-protected.';
-- Create "membership_batch_item" table
CREATE TABLE "membership"."membership_batch_item" (
  "batch_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "position" integer NOT NULL,
  "membership_id" uuid NOT NULL,
  "principal_id" uuid NULL,
  "current_status" text NULL,
  "version_read" bigint NULL,
  "resulting_status" text NULL,
  "refusal" jsonb NULL,
  "outcome" text NULL,
  "outcome_reason" text NULL,
  "accepted_at" timestamptz NULL,
  "event_id" uuid NULL,
  "resulting_version" bigint NULL,
  "problem" jsonb NULL,
  PRIMARY KEY ("batch_id", "position"),
  CONSTRAINT "membership_batch_item_once" UNIQUE ("batch_id", "membership_id"),
  CONSTRAINT "membership_batch_item_parent_fk" FOREIGN KEY ("tenant_id", "batch_id") REFERENCES "membership"."membership_batch" ("tenant_id", "batch_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "membership_batch_item_outcome_check" CHECK ((outcome IS NULL) OR (outcome = ANY (ARRAY['succeeded'::text, 'failed'::text, 'not_attempted'::text]))),
  CONSTRAINT "membership_batch_item_preview_check" CHECK ((resulting_status IS NULL) <> (refusal IS NULL)),
  CONSTRAINT "membership_batch_item_reason_check" CHECK ((outcome_reason IS NULL) OR (outcome_reason = ANY (ARRAY['refused_at_preview'::text, 'error_allowance'::text])))
);
-- Set comment to table: "membership_batch_item"
COMMENT ON TABLE "membership"."membership_batch_item" IS 'One item of a bulk Membership action (ADR-ORG-004). RLS-protected.';
