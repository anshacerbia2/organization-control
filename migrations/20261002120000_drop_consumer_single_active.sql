-- Several projection consumers may be active at once (ADR-GLB-018, TDD-organization-control-002
-- §Consumer Registry). The index held the estate to one while the outbox could record one outcome
-- per event; foundation-platform v0.3.0 gave each consumer a delivery of its own.
DROP INDEX "projection"."consumer_single_active"; -- atlas:destructive-approved: index only; the rule it enforced is withdrawn by ADR-GLB-018
