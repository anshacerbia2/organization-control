-- Modify "invitation" table
ALTER TABLE "invitation"."invitation" ADD COLUMN "token_hash" text NOT NULL;
-- Create index "invitation_token_hash_unique" to table: "invitation"
CREATE UNIQUE INDEX "invitation_token_hash_unique" ON "invitation"."invitation" ("token_hash");
