# Redis Storage Versioning

## Versions

- `v1`
  - Redis sorted-set timestamp indexes store positive Unix timestamps.
  - Only oldest-first listing is available from the stored score direction.
- `v2`
  - Redis sorted-set timestamp indexes store negative Unix timestamps.
  - The existing `list_objects()` pagination and rank logic stays unchanged.
  - Ascending `ZRANGE` now returns newest-first results because newer objects
    have smaller scores.
- `v3`
  - Stores positive Unix timestamps as the canonical created_at score.
  - Supports both `created_at_asc` and `created_at_desc` list ordering.
  - Uses `ZRANGE` / `ZRANK` for ascending and `ZREVRANGE` / `ZREVRANK` for descending.
  - Rekeys batch metastore jobs from `batchjob:<id>` to `batchjob/<id>`.
  - Rekeys batch status copies from
    `batchstatus_copies:<job_id>:<worker_id>` to
    `batchstatus_copies:<job_id>/<worker_id>`.
  - Rebuilds hierarchical Redis indexes from the migrated keys so
    `list_objects()` can use `parent:index` and `timestamps:{parent}` for batch
    metastore listings.

## Version Key

- Redis key: `storage:version`
- Missing key defaults to version `1`
- Latest version is `3`

## Upgrade Logic

- Startup inspects every initialized Redis-backed storage instance before the
  metadata service reports readiness.
- If `storage:version` is missing, startup treats the storage as `v1`.
- The `v1/v2 -> v3` upgrade:
  - normalizes every timestamp score to `abs(score)`
  - migrates batch metastore keys to the new slash-delimited schema
  - rebuilds `timestamps:all`
  - rebuilds each affected `parent:index`
  - rebuilds each affected `timestamps:{parent}`
- After all index rewrites complete successfully, startup sets
  `storage:version = 3`.

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
  same direct `v1/v2 -> v3` migration as startup initialization.
- For a batch-focused operator entrypoint, run:

```bash
poetry run python scripts/upgrade_redis_storage.py
```
