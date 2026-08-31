CREATE TABLE IF NOT EXISTS plinth_services (
    name text PRIMARY KEY,
    state jsonb NOT NULL,
    updated_at timestamptz NOT NULL
);
