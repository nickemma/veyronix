CREATE TABLE IF NOT EXISTS plinth_audit (
    id bigserial PRIMARY KEY,
    event_time timestamptz NOT NULL,
    actor text NOT NULL,
    team text NOT NULL,
    action text NOT NULL,
    resource text NOT NULL,
    revision integer NOT NULL,
    previous_revision integer NOT NULL,
    outcome text NOT NULL,
    detail text NOT NULL DEFAULT ''
);
