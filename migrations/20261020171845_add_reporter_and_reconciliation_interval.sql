-- The reporter registry (ADR-ORG-001 §5.11), the reporter authority on the privileged-access record,
-- and each consumer's declared reconciliation interval (ADR-ORG-001 §5.7, ADR-ORG-003 §5.5). Written by
-- `atlas migrate diff`; the timeouts and the reviewed squawk exceptions are added by hand (.squawk.toml).
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
-- Modify "privileged_access" table
-- Reviewed: the CHECK is widened, never narrowed, so every stored row still satisfies it, and the
-- table is a development server's (no production, SAD-004 §10.3). NOT VALID with VALIDATE gains
-- nothing in Atlas's one transaction per file.
-- squawk-ignore ban-drop-constraint, constraint-missing-not-valid
ALTER TABLE "audit"."privileged_access" DROP CONSTRAINT "privileged_access_authority_check", ADD CONSTRAINT "privileged_access_authority_check" CHECK (authority = ANY (ARRAY['emergency'::text, 'activation'::text, 'eligible'::text, 'consumer'::text, 'reporter'::text]));
-- Set comment to column: "authority" on table: "privileged_access"
COMMENT ON COLUMN "audit"."privileged_access"."authority" IS 'emergency, activation, eligible, consumer or reporter: what admitted the access.';
-- Create "reporter" table
CREATE TABLE "organization"."reporter" (
  "reporter_id" uuid NOT NULL,
  "principal_id" uuid NOT NULL,
  "reports" text NOT NULL,
  "purpose" text NOT NULL,
  "version" bigint NOT NULL DEFAULT 1,
  "registered_at" timestamptz NOT NULL DEFAULT now(),
  "registered_by" uuid NOT NULL,
  "register_reason" text NOT NULL,
  "retired_at" timestamptz NULL,
  "retired_by" uuid NULL,
  "retire_reason" text NULL,
  PRIMARY KEY ("reporter_id"),
  CONSTRAINT "reporter_purpose_check" CHECK ((btrim(purpose) <> ''::text) AND (length(purpose) <= 500)),
  CONSTRAINT "reporter_reason_check" CHECK (btrim(register_reason) <> ''::text),
  CONSTRAINT "reporter_reports_check" CHECK ((reports = 'provisioning'::text) OR (reports ~ '^obligation:[a-z0-9][a-z0-9-]{0,62}$'::text)),
  CONSTRAINT "reporter_retirement_check" CHECK (((retired_at IS NULL) = (retired_by IS NULL)) AND ((retired_at IS NULL) = (retire_reason IS NULL)) AND ((retire_reason IS NULL) OR (btrim(retire_reason) <> ''::text)))
);
-- Create index "reporter_principal_unique" to table: "reporter"
CREATE UNIQUE INDEX "reporter_principal_unique" ON "organization"."reporter" ("principal_id");
-- Set comment to table: "reporter"
COMMENT ON TABLE "organization"."reporter" IS 'Workload Principals admitted to report provisioning or one domain''s obligations. Checked for every report.';
-- Modify "consumer" table
-- Reviewed: the CHECK reads a column this statement adds with a constant default inside the bounds,
-- so every row satisfies it, and the registry holds one row per consumer.
-- squawk-ignore constraint-missing-not-valid
ALTER TABLE "projection"."consumer" ADD CONSTRAINT "consumer_reconciliation_interval_check" CHECK ((reconciliation_interval >= '00:01:00'::interval) AND (reconciliation_interval <= '7 days'::interval)), ADD COLUMN "reconciliation_interval" interval NOT NULL DEFAULT '1 day'::interval, ADD COLUMN "reconciliation_requested_at" timestamptz NULL;
