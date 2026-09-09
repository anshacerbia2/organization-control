-- Modify "consumer" table
ALTER TABLE "projection"."consumer" ADD COLUMN "retired_at" timestamptz NULL;
-- Refuse with a reason rather than with a duplicate-key error.
--
-- The index below is what enforces "at most one active projection consumer". Applied to a
-- database that already holds two, it fails as `Key ((true))=(t) is duplicated` -- true, and
-- unreadable. An operator holding a cryptic failure on a security-relevant migration reaches
-- for the shortest way past it, and the shortest way past this one is deleting the index.
--
-- So the condition is named here. Retiring the consumers that are finished is the fix, and it
-- is a decision someone makes rather than one this migration makes for them: the alternative
-- would be picking a survivor, and nothing here knows which one that should be.
DO $$
DECLARE
    active integer;
BEGIN
    SELECT count(*) INTO active FROM "projection"."consumer" WHERE "retired_at" IS NULL;
    IF active > 1 THEN
        RAISE EXCEPTION
            'projection.consumer holds % active consumers, and the distributed enforcement scope is one. Retire the consumers that are finished (UPDATE projection.consumer SET retired_at = now() WHERE consumer_id = ...), leaving exactly one active, then apply this migration again.',
            active;
    END IF;
END $$;
-- Create index "consumer_single_active" to table: "consumer"
CREATE UNIQUE INDEX "consumer_single_active" ON "projection"."consumer" ((true)) WHERE (retired_at IS NULL);
