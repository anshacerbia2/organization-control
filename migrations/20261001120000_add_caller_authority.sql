-- Create "provider_grant" table
CREATE TABLE "organization"."provider_grant" (
  "grant_id" uuid NOT NULL,
  "principal_id" uuid NOT NULL,
  "scope" text NOT NULL,
  "granted_by" uuid NULL,
  "bootstrap_operator" text NULL,
  "reason" text NOT NULL,
  "granted_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("grant_id"),
  CONSTRAINT "provider_grant_origin_check" CHECK ((granted_by IS NULL) = (bootstrap_operator IS NOT NULL)),
  CONSTRAINT "provider_grant_reason_check" CHECK (btrim(reason) <> ''::text),
  CONSTRAINT "provider_grant_scope_check" CHECK (scope = 'provider:organization-control'::text)
);
-- Create index "provider_grant_principal_scope" to table: "provider_grant"
CREATE UNIQUE INDEX "provider_grant_principal_scope" ON "organization"."provider_grant" ("principal_id", "scope");
-- Create index "provider_grant_single_bootstrap" to table: "provider_grant"
CREATE UNIQUE INDEX "provider_grant_single_bootstrap" ON "organization"."provider_grant" ((true)) WHERE (granted_by IS NULL);
-- Set comment to table: "provider_grant"
COMMENT ON TABLE "organization"."provider_grant" IS 'Provider authority over this service, by principal_id. Checked for every provider request.';
-- Refuse with a reason rather than with a not-null violation.
--
-- A consumer is now the workload Principal it was registered with (ADR-ORG-001 §5.11), and a row
-- registered before this migration names none. No production deployment exists, so there is no
-- Principal to backfill: a development database holding consumers registers them again.
DO $$
DECLARE
    held integer;
BEGIN
    SELECT count(*) INTO held FROM "projection"."consumer";
    IF held > 0 THEN
        RAISE EXCEPTION
            'projection.consumer holds % row(s) registered without a principal_id, which a consumer now needs to authenticate. On a development database, clear projection.consumer and register each consumer again with its workload principal_id, then apply this migration again.',
            held;
    END IF;
END $$;
-- Modify "consumer" table
ALTER TABLE "projection"."consumer" ADD COLUMN "principal_id" uuid NOT NULL;
-- Create index "consumer_principal" to table: "consumer"
CREATE UNIQUE INDEX "consumer_principal" ON "projection"."consumer" ("principal_id");
