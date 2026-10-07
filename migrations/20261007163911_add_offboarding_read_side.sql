-- Modify "offboarding" table
ALTER TABLE "operation"."offboarding" ADD COLUMN "released_at" timestamptz NULL;
-- Modify "offboarding_obligation" table
ALTER TABLE "operation"."offboarding_obligation" ADD COLUMN "resolved_by" uuid NULL, ADD COLUMN "resolved_at" timestamptz NULL;
