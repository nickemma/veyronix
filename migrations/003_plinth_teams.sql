CREATE TABLE IF NOT EXISTS plinth_teams (
    name TEXT PRIMARY KEY,
    members JSONB NOT NULL,
    namespace TEXT NOT NULL,
    service_quota INTEGER NOT NULL CHECK (service_quota > 0)
);
