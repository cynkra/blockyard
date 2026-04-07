-- phase: expand
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';

CREATE TABLE app_data_mounts (
    app_id   TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    source   TEXT NOT NULL,
    target   TEXT NOT NULL,
    readonly INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (app_id, target)
);
