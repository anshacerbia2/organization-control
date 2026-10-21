-- The reporter registry's third kind of entry, invitation:resolve: identity-control's workload, admitted
-- to POST /v1/invitations/resolve alone (ADR-IAM-011 §5.7 (a), ADR-ORG-001 §5.11, amended 2026-10-21;
-- TDD-organization-control-004 1.18.0). Written by `atlas migrate diff`; the timeouts and the reviewed
-- squawk exception are added by hand (.squawk.toml).
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
-- Modify "reporter" table
-- Reviewed: the CHECK is widened, never narrowed, so every stored row still satisfies it, and the
-- registry holds a handful of rows on a development server only (no production, SAD-004 §10.3).
-- NOT VALID with VALIDATE gains nothing in Atlas's one transaction per file.
-- squawk-ignore ban-drop-constraint, constraint-missing-not-valid
ALTER TABLE "organization"."reporter" DROP CONSTRAINT "reporter_reports_check", ADD CONSTRAINT "reporter_reports_check" CHECK ((reports = 'provisioning'::text) OR (reports = 'invitation:resolve'::text) OR (reports ~ '^obligation:[a-z0-9][a-z0-9-]{0,62}$'::text));
-- Set comment to table: "reporter"
COMMENT ON TABLE "organization"."reporter" IS 'Workload Principals admitted to report provisioning or one domain''s obligations, or to resolve an invitation token. Checked for every report.';
