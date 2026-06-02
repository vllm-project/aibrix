import asyncio
import json
from datetime import datetime
from typing import Any, List, Optional

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatusCopy,
    JobEntityManager,
    aggregate_batch_job_status,
)
from aibrix.client.redis import AsyncRedis, RedisPipeline, run_pipeline


class RedisJobCache(JobEntityManager):
    def __init__(
        self,
        redis_client: AsyncRedis,
        key_prefix: str = "batch_jobs",
    ) -> None:
        super().__init__()
        self._client: AsyncRedis = redis_client
        self._key_prefix = key_prefix
        self._metastore_compatible_keys = key_prefix == ""
        # If key_prefix is empty, we try to make it compatible with storage/redis convention.
        self._index_key = f"{key_prefix}:index" if key_prefix else "timestamps:all"
        self._job_prefix = f"{key_prefix}:batchjob" if key_prefix else "batchjob"
        self._status_copy_prefix = (
            f"{key_prefix}:batchstatus_copies" if key_prefix else "batchstatus_copies"
        )
        self._key_prefix = key_prefix

    async def get_job(
        self, job_id: str, force_reload: bool = False
    ) -> Optional[BatchJob]:
        if job_id in self.active_jobs and not force_reload:
            return self.active_jobs[job_id]
        payload = await self._client.get(self._job_key(job_id))
        if payload is None:
            return None
        return await self._prepare_loaded_job(self._deserialize_job(payload))

    async def list_jobs(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> List[BatchJob]:
        decoded_job_ids = await self._list_indexed_job_ids(after=after, limit=limit)
        if after is not None and not decoded_job_ids:
            return []
        payloads = await self._pipeline_get(
            [self._job_key(job_id) for job_id in decoded_job_ids]
        )
        jobs: list[Optional[BatchJob]] = []
        jobs_to_prepare: list[BatchJob] = []
        jobs_to_prepare_indices: list[int] = []
        for payload in payloads:
            if payload is None:
                continue
            job = self._deserialize_job(payload)
            if self._should_load_status_copies(job):
                jobs_to_prepare_indices.append(len(jobs))
                jobs.append(None)
                jobs_to_prepare.append(job)
            else:
                self._sync_active_job(job)
                jobs.append(job)
        if jobs_to_prepare:
            prepared_jobs = await self._prepare_loaded_jobs(jobs_to_prepare)
            for index, prepared_job in zip(jobs_to_prepare_indices, prepared_jobs):
                jobs[index] = prepared_job
        listed_jobs = [job for job in jobs if job is not None]
        return listed_jobs

    async def _list_recovery_jobs(self) -> List[BatchJob]:
        return await self._list_jobs_for_recovery(
            await self._get_oldest_unfinished_job_created_at()
        )

    async def submit_job(
        self, session_id: str, job_spec: BatchJobSpec, request_count: int = 0
    ):
        job = BatchJob.new_local(spec=job_spec, request_count=request_count)
        job.session_id = session_id
        stored_job = await self._upsert_job(job, None)
        await self.job_committed(stored_job)

    async def update_job_ready(self, job: BatchJob):
        await self._update_existing_job(job)

    async def update_job_status(self, job: BatchJob):
        await self._update_existing_job(job)

    async def cancel_job(self, job: BatchJob):
        await self._update_existing_job(job)

    async def delete_job(self, job: BatchJob):
        existing_job = await self.get_job(job.job_id) or job
        worker_ids = await self._list_status_copy_worker_ids(job.job_id)
        await run_pipeline(
            self._client,
            lambda pipeline: self._delete_job_pipeline(
                pipeline, job.job_id, worker_ids
            ),
        )
        await self._persist_oldest_unfinished_job_created_at(job)
        await self.job_deleted(existing_job)

    async def _update_existing_job(self, job: BatchJob) -> None:
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = await self.get_job(job.job_id)
        stored_job = await self._upsert_job(job, old_job)
        await self._persist_oldest_unfinished_job_created_at(stored_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)
        else:
            self._sync_active_job(stored_job)

    async def _upsert_job(self, job: BatchJob, old_job: Optional[BatchJob]) -> BatchJob:
        if job.job_id is None:
            raise ValueError("job_id is required")
        stored_job = job.model_copy(deep=True)
        stored_job_id = stored_job.job_id
        if stored_job_id is None:
            raise ValueError("job_id is required")
        stored_job.metadata.resource_version = self._next_resource_version(old_job)
        if not self._should_load_status_copies(stored_job):
            stored_job.status.status_copies = None
        payload = stored_job.model_dump_json(by_alias=True)
        status_copies = stored_job.status.status_copies or {}
        await run_pipeline(
            self._client,
            lambda pipeline: self._upsert_job_pipeline(
                pipeline,
                stored_job_id,
                payload,
                stored_job,
                status_copies,
            ),
        )
        return stored_job

    def _job_key(self, job_id: str) -> str:
        return f"{self._job_prefix}:{job_id}"

    def _status_copy_key(self, job_id: str, worker_id: str) -> str:
        return f"{self._status_copy_prefix}:{job_id}:{worker_id}"

    def _status_copy_index_key(self, job_id: str) -> str:
        return f"{self._status_copy_prefix}:{job_id}:index"

    def _deserialize_job(self, payload: Any) -> BatchJob:
        if isinstance(payload, bytes):
            payload = payload.decode("utf-8")
        job = BatchJob.model_validate(json.loads(payload))
        job.status = aggregate_batch_job_status(job.status, False)
        return job

    async def _hydrate_job(self, job: BatchJob) -> BatchJob:
        if job.job_id is None or not self._should_load_status_copies(job):
            return job
        status_copies = await self._load_status_copies(job.job_id)
        if not status_copies:
            return job
        job.status.status_copies = status_copies
        job.status = aggregate_batch_job_status(job.status, False)
        return job

    async def _prepare_loaded_job(self, job: BatchJob) -> BatchJob:
        hydrated_job = await self._hydrate_job(job)
        self._sync_active_job(hydrated_job)
        return hydrated_job

    async def _prepare_loaded_jobs(self, jobs: list[BatchJob]) -> list[BatchJob]:
        if not jobs:
            return []
        job_ids = [job.job_id for job in jobs if job.job_id is not None]
        status_copies_by_job = await self._load_status_copies_for_jobs(job_ids)
        prepared_jobs: list[BatchJob] = []
        for job in jobs:
            job_id = job.job_id
            if job_id is not None:
                status_copies = status_copies_by_job.get(job_id)
                if status_copies:
                    job.status.status_copies = status_copies
                    job.status = aggregate_batch_job_status(job.status, False)
            self._sync_active_job(job)
            prepared_jobs.append(job)
        return prepared_jobs

    async def _load_status_copies(self, job_id: str) -> dict[str, BatchJobStatusCopy]:
        return (await self._load_status_copies_for_jobs([job_id])).get(job_id, {})

    async def _load_status_copies_for_jobs(
        self, job_ids: list[str]
    ) -> dict[str, dict[str, BatchJobStatusCopy]]:
        if not job_ids:
            return {}
        worker_ids_by_job = await self._list_status_copy_worker_ids_for_jobs(job_ids)
        status_copy_keys: list[str] = []
        status_copy_refs: list[tuple[str, str]] = []
        for job_id in job_ids:
            for worker_id in worker_ids_by_job.get(job_id, []):
                status_copy_keys.append(self._status_copy_key(job_id, worker_id))
                status_copy_refs.append((job_id, worker_id))
        if not status_copy_keys:
            return {}
        payloads = await self._pipeline_get(status_copy_keys)
        status_copies_by_job: dict[str, dict[str, BatchJobStatusCopy]] = {}
        for (job_id, worker_id), payload in zip(status_copy_refs, payloads):
            if payload is None:
                continue
            if isinstance(payload, bytes):
                payload = payload.decode("utf-8")
            status_copies_by_job.setdefault(job_id, {})[worker_id] = (
                BatchJobStatusCopy.model_validate_json(payload)
            )
        return status_copies_by_job

    async def _list_status_copy_worker_ids_for_jobs(
        self, job_ids: list[str]
    ) -> dict[str, list[str]]:
        if not job_ids:
            return {}
        if self._metastore_compatible_keys:
            worker_ids = await asyncio.gather(
                *(self._list_status_copy_worker_ids(job_id) for job_id in job_ids)
            )
            return dict(zip(job_ids, worker_ids))
        worker_ids = await self._pipeline_smembers(
            [self._status_copy_index_key(job_id) for job_id in job_ids]
        )
        return {
            job_id: [self._decode(worker_id) for worker_id in raw_worker_ids]
            for job_id, raw_worker_ids in zip(job_ids, worker_ids)
        }

    async def _list_status_copy_worker_ids(self, job_id: str) -> list[str]:
        if self._metastore_compatible_keys:
            prefix = f"{self._status_copy_prefix}:{job_id}:"
            keys = await self._list_keys_with_prefix(prefix)
            return [key[len(prefix) :] for key in keys]
        worker_ids = await self._client.smembers(self._status_copy_index_key(job_id))
        return [self._decode(worker_id) for worker_id in worker_ids]

    async def _list_job_ids_from_metastore_keys(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> list[str]:
        prefix = f"{self._job_prefix}:"
        start = 0
        if after is not None:
            after_rank = await self._client.zrevrank(
                "timestamps:all", self._job_key(after)
            )
            if after_rank is None:
                return []
            start = after_rank + 1
        job_keys = await self._list_keys_with_prefix(prefix, start=start, limit=limit)
        return [job_key[len(prefix) :] for job_key in job_keys]

    async def _list_indexed_job_ids(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> list[str]:
        if self._metastore_compatible_keys:
            return await self._list_job_ids_from_metastore_keys(
                after=after, limit=limit
            )
        start = 0
        if after is not None:
            after_rank = await self._client.zrevrank(self._index_key, after)
            if after_rank is None:
                return []
            start = after_rank + 1
        end = start + limit - 1
        job_ids = await self._client.zrevrange(self._index_key, start, end)
        return [self._decode(raw_job_id) for raw_job_id in job_ids]

    async def _list_keys_with_prefix(
        self,
        prefix: str,
        start: int = 0,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> list[str]:
        matched_keys: list[str] = []
        range_start = start
        chunk_size = max(limit, 100)
        while len(matched_keys) < limit:
            range_end = range_start + chunk_size - 1
            raw_keys = await self._client.zrevrange(
                "timestamps:all", range_start, range_end
            )
            if not raw_keys:
                break
            for raw_key in raw_keys:
                decoded_key = self._decode(raw_key)
                if not decoded_key.startswith(prefix):
                    continue
                matched_keys.append(decoded_key)
                if len(matched_keys) >= limit:
                    return matched_keys
            range_start += len(raw_keys)
        return matched_keys

    async def _get_oldest_unfinished_job_created_at(self) -> Optional[datetime]:
        raw_value = await self._client.get(self._oldest_unfinished_created_at_key)
        if raw_value is None:
            return None
        return datetime.fromisoformat(self._decode(raw_value))

    async def _persist_oldest_unfinished_job_created_at(
        self, candidate_job: Optional[BatchJob] = None
    ) -> None:
        oldest_created_at = self._oldest_unfinished_job_created_at(candidate_job)
        if oldest_created_at is None:
            await self._client.delete(self._oldest_unfinished_created_at_key)
            return
        await self._client.set(
            self._oldest_unfinished_created_at_key,
            oldest_created_at.isoformat(),
        )

    def _oldest_unfinished_job_created_at(
        self, candidate_job: Optional[BatchJob] = None
    ) -> Optional[datetime]:
        created_at_values = [
            job.status.created_at
            for job in self.active_jobs.values()
            if not job.status.finished
            and job.status.created_at is not None
            and (candidate_job is None or job.job_id != candidate_job.job_id)
        ]
        if (
            candidate_job is not None
            and not candidate_job.status.finished
            and candidate_job.status.created_at is not None
        ):
            created_at_values.append(candidate_job.status.created_at)
        return min(created_at_values, default=None)

    async def _pipeline_get(self, keys: list[str]) -> list[Any]:
        if not keys:
            return []
        return await run_pipeline(
            self._client, lambda pipeline: [pipeline.get(key) for key in keys]
        )

    async def _pipeline_smembers(self, keys: list[str]) -> list[Any]:
        if not keys:
            return []
        return await run_pipeline(
            self._client, lambda pipeline: [pipeline.smembers(key) for key in keys]
        )

    def _upsert_job_pipeline(
        self,
        pipeline: RedisPipeline,
        job_id: str,
        payload: str,
        stored_job: BatchJob,
        status_copies: dict[str, BatchJobStatusCopy],
    ) -> None:
        pipeline.set(self._job_key(job_id), payload)
        created_at_score = self._created_at_score(stored_job)
        if self._metastore_compatible_keys:
            pipeline.zadd("timestamps:all", {self._job_key(job_id): created_at_score})
        else:
            pipeline.zadd(self._index_key, {job_id: created_at_score})
        for worker_id, status_copy in status_copies.items():
            if not status_copy.updated:
                continue
            pipeline.set(
                self._status_copy_key(job_id, worker_id),
                status_copy.model_dump_json(by_alias=True, exclude_none=True),
            )
            if self._metastore_compatible_keys:
                pipeline.zadd(
                    "timestamps:all",
                    {self._status_copy_key(job_id, worker_id): created_at_score},
                )
            else:
                pipeline.sadd(self._status_copy_index_key(job_id), worker_id)

    def _delete_job_pipeline(
        self, pipeline: RedisPipeline, job_id: str, worker_ids: list[str]
    ) -> None:
        pipeline.delete(self._job_key(job_id))
        if self._metastore_compatible_keys:
            pipeline.zrem("timestamps:all", self._job_key(job_id))
        else:
            pipeline.zrem(self._index_key, job_id)
            pipeline.delete(self._status_copy_index_key(job_id))
        for worker_id in worker_ids:
            if self._metastore_compatible_keys:
                pipeline.zrem(
                    "timestamps:all", self._status_copy_key(job_id, worker_id)
                )
            pipeline.delete(self._status_copy_key(job_id, worker_id))

    def _should_load_status_copies(self, job: BatchJob) -> bool:
        return job.status.state not in {
            BatchJobState.CREATED,
            BatchJobState.VALIDATING,
            BatchJobState.FINALIZED,
        }

    def _decode(self, value: bytes | str) -> str:
        if isinstance(value, bytes):
            return value.decode("utf-8")
        return value

    def _next_resource_version(self, old_job: Optional[BatchJob]) -> str:
        if old_job is None or old_job.metadata.resource_version is None:
            return "1"
        try:
            return str(int(old_job.metadata.resource_version) + 1)
        except ValueError:
            return "1"

    def _created_at_score(self, job: BatchJob) -> float:
        return job.status.created_at.timestamp()

    @property
    def _oldest_unfinished_created_at_key(self) -> str:
        return (
            f"{self._key_prefix}:oldest_unfinished_created_at"
            if self._key_prefix
            else "oldest_unfinished_created_at"
        )
