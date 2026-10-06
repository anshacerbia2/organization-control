-- Create "emergency_grant_use" table
CREATE TABLE "organization"."emergency_grant_use" (
  "grant_id" uuid NOT NULL,
  "first_used_at" timestamptz NOT NULL DEFAULT now(),
  "last_used_at" timestamptz NOT NULL DEFAULT now(),
  "uses" bigint NOT NULL DEFAULT 1,
  PRIMARY KEY ("grant_id"),
  CONSTRAINT "emergency_grant_use_grant_fk" FOREIGN KEY ("grant_id") REFERENCES "organization"."provider_grant" ("grant_id") ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT "emergency_grant_use_order_check" CHECK (first_used_at <= last_used_at),
  CONSTRAINT "emergency_grant_use_uses_check" CHECK (uses > 0)
);
-- Set comment to table: "emergency_grant_use"
COMMENT ON TABLE "organization"."emergency_grant_use" IS 'The last use of each emergency provider grant of this service''s scope, recorded by every request it authorizes. ADR-ORG-002 §5.2.';
