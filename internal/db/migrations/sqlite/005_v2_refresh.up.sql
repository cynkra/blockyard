ALTER TABLE apps ADD COLUMN refresh_schedule TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN last_refresh_at TEXT;
