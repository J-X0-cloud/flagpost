-- Core schema: environments, flags, per-environment flag configuration,
-- segments and the audit log.
--
-- Timestamps are stored as fixed-width UTC strings
-- (YYYY-MM-DDTHH:MM:SS.ffffffZ) so they sort lexicographically.
-- Rule-heavy structures are stored as JSON documents: they are always read
-- and written as a unit and never queried by their inner fields.

CREATE TABLE environments (
    key         TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    sdk_key     TEXT NOT NULL UNIQUE,
    revision    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE TABLE flags (
    key         TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    variants    TEXT NOT NULL,
    tags        TEXT NOT NULL DEFAULT '[]',
    archived    INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE flag_configs (
    flag_key   TEXT NOT NULL REFERENCES flags(key) ON DELETE CASCADE,
    env_key    TEXT NOT NULL REFERENCES environments(key) ON DELETE CASCADE,
    enabled    INTEGER NOT NULL,
    body       TEXT NOT NULL,
    version    INTEGER NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (flag_key, env_key)
);

CREATE INDEX flag_configs_env ON flag_configs(env_key);

CREATE TABLE segments (
    env_key     TEXT NOT NULL REFERENCES environments(key) ON DELETE CASCADE,
    key         TEXT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL,
    version     INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (env_key, key)
);

CREATE TABLE audit_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    at            TEXT NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_key  TEXT NOT NULL,
    env_key       TEXT NOT NULL DEFAULT '',
    comment       TEXT NOT NULL DEFAULT '',
    before_doc    TEXT,
    after_doc     TEXT,
    changes       TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX audit_log_resource ON audit_log(resource_type, resource_key, id);
CREATE INDEX audit_log_env ON audit_log(env_key, id);
