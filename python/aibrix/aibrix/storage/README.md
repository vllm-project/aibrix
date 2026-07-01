# Redis Storage Versioning

## Versions

- `v1`
  - Redis sorted-set timestamp indexes store positive Unix timestamps.
  - Ascending `ZRANGE` pagination therefore returns oldest-first results.
- `v2`
  - Redis sorted-set timestamp indexes store negative Unix timestamps.
  - The existing `list_objects()` pagination and rank logic stays unchanged.
  - Ascending `ZRANGE` now returns newest-first results because newer objects
    have smaller scores.

## Version Key

- Redis key: `storage:version`
- Missing key defaults to version `1`
- Latest version is `2`

## Upgrade Logic

- Startup inspects every initialized Redis-backed storage instance before the
  metadata service reports readiness.
- If `storage:version` is missing, startup treats the storage as `v1`.
- The `v1 -> v2` upgrade rewrites:
  - `timestamps:all`
  - each hierarchical parent index `timestamps:{parent}`
- Every score is normalized from `score` to `-abs(score)`.
- After all index rewrites complete successfully, startup sets
  `storage:version = 2`.

## Startup Behavior

- `metadata/app.py` sets `app.state.storage_upgrade_in_progress = True` before
  running Redis storage upgrades.
- `/readyz` returns `503` while that flag is set.
- The batch driver starts only after upgrades finish, so recovery never reads a
  partially migrated Redis index.

## Manual Upgrade Script

- Run from `python/aibrix`:

```bash
poetry run python scripts/upgrade_redis_storage.py
```

- The script uses the normal Redis environment configuration and applies the
  same `v1 -> v2` migration as startup initialization.
