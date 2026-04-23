-- phase: expand
--
-- Postgres-primary port / UID allocators for the process backend
-- (see #288, parent #262). The Redis variants in
-- internal/backend/process/{ports,uids}_redis.go fail open on a Redis
-- blip — a transient outage during allocation can hand out a slot
-- already claimed by another peer, or falsely report exhaustion. Moving
-- the source of truth into Postgres makes allocation correct under
-- Redis restart; Redis stays as an optional read-through cache for the
-- diagnostic InUse() path.
--
-- The owner column carries the hostname (the same identifier the Redis
-- variants use as the SETNX value). It is NOT the per-process serverID
-- — these allocators care about "which host's crashed state should be
-- reclaimed at startup" rather than "which concurrent peer holds this
-- slot," and hostname is the right granularity for the former.
-- No secondary index on `owner`. The only owner-keyed queries are
-- InUse() (a diagnostic) and CleanupOwnedOrphans() (once per startup),
-- both fully tolerant of a seq scan over the small (< ~1000-row)
-- range. An index would add maintenance cost on every Reserve / Release
-- without paying back on any hot path.
CREATE TABLE blockyard_ports (
    port  INT  PRIMARY KEY,
    owner TEXT NOT NULL
);

CREATE TABLE blockyard_uids (
    uid   INT  PRIMARY KEY,
    owner TEXT NOT NULL
);
