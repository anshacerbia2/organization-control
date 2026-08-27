-- Modify "consumer" table
ALTER TABLE "projection"."consumer" ADD CONSTRAINT "verify_counters_non_negative" CHECK ((verify_calls_since_report >= 0) AND ((last_reported_requests IS NULL) OR (last_reported_requests >= 0))), ADD COLUMN "verify_calls_since_report" bigint NOT NULL DEFAULT 0, ADD COLUMN "last_reported_requests" bigint NULL, ADD COLUMN "last_verify_ratio" double precision NULL;
-- Set comment to column: "last_reported_requests" on table: "consumer"
COMMENT ON COLUMN "projection"."consumer"."last_reported_requests" IS 'Requests the consumer served in the interval it last reported. The denominator.';
-- Set comment to column: "last_verify_ratio" on table: "consumer"
COMMENT ON COLUMN "projection"."consumer"."last_verify_ratio" IS 'verify_calls_since_report / last_reported_requests at the last report. NULL until a report supplies a denominator.';
