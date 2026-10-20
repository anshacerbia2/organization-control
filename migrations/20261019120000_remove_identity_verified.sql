-- ADR-ORG-007 §5.4: acceptance is the whole join, so the half-join `identity_verified` is removed
-- (TDD-organization-control-004 1.14.0 §Data Model). Nothing ever entered it: identity-control
-- produced no verification fact. A row still in it returns to `pending` with no principal_id, so the
-- acceptance that follows checks the address itself. The index cannot collide: it already allowed
-- one outstanding invitation across both states, and every returned row is that one.
UPDATE "invitation"."invitation" SET state = 'pending', principal_id = NULL WHERE state = 'identity_verified';
-- The check is narrowed by dropping and re-adding it in one ALTER TABLE, which PostgreSQL applies
-- atomically and Atlas runs in one transaction: no moment exists without the check, and the UPDATE
-- above leaves no row outside the new set. Reviewed 2026-10-19 (TDD-organization-control-004 1.14.0,
-- ADR-ORG-007).
-- Modify "invitation" table
ALTER TABLE "invitation"."invitation" DROP CONSTRAINT "invitation_state_check", ADD CONSTRAINT "invitation_state_check" CHECK (state = ANY (ARRAY['pending'::text, 'accepted'::text, 'expired'::text, 'revoked'::text])); -- atlas:destructive-approved: check narrowed in place after the rows it excludes were returned to pending
-- The partial unique index is rebuilt with the narrower predicate. Both statements run in Atlas's one
-- transaction, so no concurrent issue sees the table without it.
-- Rebuild "invitation_pending_unique" on table "invitation": first the old predicate goes
DROP INDEX "invitation"."invitation_pending_unique"; -- atlas:destructive-approved: rebuilt below with state = 'pending', in the same transaction
-- Create index "invitation_pending_unique" to table: "invitation"
CREATE UNIQUE INDEX "invitation_pending_unique" ON "invitation"."invitation" ("tenant_id", "target_hash", (COALESCE(workspace_id, tenant_id))) WHERE (state = 'pending'::text);
