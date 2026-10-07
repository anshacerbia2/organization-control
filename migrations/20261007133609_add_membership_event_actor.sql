-- Modify "membership_event" table
ALTER TABLE "membership"."membership_event" ADD CONSTRAINT "membership_event_reason_present" CHECK ((reason IS NULL) OR (btrim(reason) <> ''::text)), ADD COLUMN "actor_id" uuid NULL, ADD COLUMN "correlation_id" uuid NULL, ADD COLUMN "reason" text NULL;
