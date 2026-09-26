-- Create "tenant_event" table
CREATE TABLE "tenant"."tenant_event" (
  "event_id" uuid NOT NULL,
  "tenant_id" uuid NOT NULL,
  "tenant_security_version" bigint NOT NULL,
  "event_type" text NOT NULL,
  "recorded_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("event_id"),
  CONSTRAINT "tenant_event_version_unique" UNIQUE ("tenant_id", "tenant_security_version"),
  CONSTRAINT "tenant_event_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenant"."tenant" ("tenant_id") ON UPDATE NO ACTION ON DELETE NO ACTION
);
-- Set comment to table: "tenant_event"
COMMENT ON TABLE "tenant"."tenant_event" IS 'Tenant security version carried by each published Tenant event. Immutable. RLS-protected.';
