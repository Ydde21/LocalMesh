-- Node maintenance/drain state. '' = in service; 'draining' = no new
-- work, in-flight finishing; 'maintenance' = fully drained, admin may
-- act; 'updating' = set by the update orchestrator during agent
-- upgrades (a specialised maintenance state that also blocks new work).

ALTER TABLE nodes ADD COLUMN maintenance        text        NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN maintenance_reason text        NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN maintenance_since  timestamptz;
