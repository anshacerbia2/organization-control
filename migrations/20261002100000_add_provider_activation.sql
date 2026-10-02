-- Modify "provider_grant" table: a grant is eligible or emergency (ADR-ORG-002)
ALTER TABLE "organization"."provider_grant" ADD CONSTRAINT "provider_grant_kind_check" CHECK (kind = ANY (ARRAY['eligible'::text, 'emergency'::text])), ADD COLUMN "kind" text NOT NULL DEFAULT 'eligible';
-- The bootstrap grant is an emergency grant (ADR-ORG-002 §5.2).
UPDATE "organization"."provider_grant" SET "kind" = 'emergency' WHERE "granted_by" IS NULL;
-- Create "provider_activation" table
CREATE TABLE "organization"."provider_activation" (
  "activation_id" uuid NOT NULL,
  "grant_id" uuid NOT NULL,
  "principal_id" uuid NOT NULL,
  "scope" text NOT NULL,
  "reason" text NOT NULL,
  "duration_seconds" integer NOT NULL,
  "approval_required" boolean NOT NULL,
  "requested_at" timestamptz NOT NULL DEFAULT now(),
  "decided_by" uuid NULL,
  "decision" text NULL,
  "decision_reason" text NULL,
  "decided_at" timestamptz NULL,
  "ends_at" timestamptz NULL,
  "ended_by" uuid NULL,
  "end_reason" text NULL,
  "ended_at" timestamptz NULL,
  PRIMARY KEY ("activation_id"),
  CONSTRAINT "provider_activation_grant_id_fkey" FOREIGN KEY ("grant_id") REFERENCES "organization"."provider_grant" ("grant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "provider_activation_decision_check" CHECK (((decision IS NULL) = (decided_at IS NULL)) AND ((decision IS NULL) OR (decision = 'lapsed'::text) OR (decided_by IS NOT NULL))),
  CONSTRAINT "provider_activation_decision_value_check" CHECK (decision = ANY (ARRAY['approved'::text, 'denied'::text, 'lapsed'::text])),
  CONSTRAINT "provider_activation_duration_check" CHECK (duration_seconds > 0),
  CONSTRAINT "provider_activation_end_check" CHECK (((ended_at IS NULL) = (ended_by IS NULL)) AND ((ended_at IS NULL) = (end_reason IS NULL))),
  CONSTRAINT "provider_activation_reason_check" CHECK (btrim(reason) <> ''::text),
  CONSTRAINT "provider_activation_separation_check" CHECK ((NOT approval_required) OR (decision IS DISTINCT FROM 'approved'::text) OR (decided_by <> principal_id)),
  CONSTRAINT "provider_activation_window_check" CHECK ((decision = 'approved'::text) = (ends_at IS NOT NULL))
);
-- Create index "provider_activation_pending" to table: "provider_activation"
CREATE UNIQUE INDEX "provider_activation_pending" ON "organization"."provider_activation" ("grant_id") WHERE (decision IS NULL);
-- Create index "provider_activation_principal" to table: "provider_activation"
CREATE INDEX "provider_activation_principal" ON "organization"."provider_activation" ("principal_id", "scope") WHERE ((decision = 'approved'::text) AND (ended_at IS NULL));
-- Set comment to table: "provider_activation"
COMMENT ON TABLE "organization"."provider_activation" IS 'An activation of an eligible provider grant: requested, decided, and ended. ADR-ORG-002.';
