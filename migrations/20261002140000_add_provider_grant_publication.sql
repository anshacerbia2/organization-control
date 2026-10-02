-- Provider grants for the Identity Control API, and their publication (ADR-ORG-002 §5.3,
-- TDD-organization-control-001 §Provider Authority Projection).
--
-- Replace the scope check: provider:identity-control is a registered scope.
ALTER TABLE "organization"."provider_grant" DROP CONSTRAINT "provider_grant_scope_check"; -- atlas:destructive-approved: a check constraint replaced on the next line with a wider one
ALTER TABLE "organization"."provider_grant" ADD CONSTRAINT "provider_grant_scope_check" CHECK (scope = ANY (ARRAY['provider:organization-control'::text, 'provider:identity-control'::text]));
-- The version of the last published transition of the grant or its activation.
ALTER TABLE "organization"."provider_grant" ADD COLUMN "grant_version" bigint NOT NULL DEFAULT 0;
-- Create "provider_grant_event" table
CREATE TABLE "organization"."provider_grant_event" (
  "event_id" uuid NOT NULL,
  "grant_id" uuid NOT NULL,
  "grant_version" bigint NOT NULL,
  "event_type" text NOT NULL,
  "recorded_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("event_id"),
  CONSTRAINT "provider_grant_event_version_unique" UNIQUE ("grant_id", "grant_version"),
  CONSTRAINT "provider_grant_event_grant_fk" FOREIGN KEY ("grant_id") REFERENCES "organization"."provider_grant" ("grant_id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Set comment to table: "provider_grant_event"
COMMENT ON TABLE "organization"."provider_grant_event" IS 'Grant version carried by each published provider grant event. Immutable.';
