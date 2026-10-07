-- Modify "membership_batch" table
ALTER TABLE "membership"."membership_batch" ADD COLUMN "lease_id" uuid NULL, ADD COLUMN "heartbeat_at" timestamptz NULL, ADD COLUMN "resumed_by" uuid NULL, ADD COLUMN "resumed_at" timestamptz NULL;
-- Modify "consumer" table
ALTER TABLE "projection"."consumer" ADD COLUMN "last_reconciled_at" timestamptz NULL, ADD COLUMN "last_reconciled_mark" bigint NULL, ADD COLUMN "last_reconciled_findings" integer NULL;
