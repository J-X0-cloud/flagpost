-- Experiment event storage.
--
-- Exposures are deduplicated per context: the first variant a context saw
-- is the one it is attributed to, even if targeting later changes, which
-- keeps the analysis intention-to-treat. Conversions are likewise counted
-- once per context and event. Times are unix microseconds so that the
-- attribution join can compare them numerically.

CREATE TABLE exposures (
    env_key     TEXT NOT NULL REFERENCES environments(key) ON DELETE CASCADE,
    flag_key    TEXT NOT NULL,
    context_key TEXT NOT NULL,
    variant     TEXT NOT NULL,
    first_seen  INTEGER NOT NULL,
    PRIMARY KEY (env_key, flag_key, context_key)
);

CREATE INDEX exposures_variant ON exposures(env_key, flag_key, variant);

CREATE TABLE conversions (
    env_key     TEXT NOT NULL REFERENCES environments(key) ON DELETE CASCADE,
    event_key   TEXT NOT NULL,
    context_key TEXT NOT NULL,
    first_seen  INTEGER NOT NULL,
    PRIMARY KEY (env_key, event_key, context_key)
);
