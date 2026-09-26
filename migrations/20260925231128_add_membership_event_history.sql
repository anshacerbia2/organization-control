-- Create "membership_event" table
CREATE TABLE "membership"."membership_event" (
  "event_id" uuid NOT NULL,
  "membership_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "membership_version" bigint NOT NULL,
  "event_type" text NOT NULL,
  "recorded_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("event_id"),
  CONSTRAINT "membership_event_version_unique" UNIQUE ("membership_id", "membership_version"),
  CONSTRAINT "membership_event_membership_fk" FOREIGN KEY ("membership_id") REFERENCES "membership"."membership" ("membership_id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Set comment to table: "membership_event"
COMMENT ON TABLE "membership"."membership_event" IS 'Membership version carried by each published authority event. Immutable. RLS-protected.';
