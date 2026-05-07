import asyncio
from datetime import datetime, timezone
from typing import Any, Callable, Coroutine, Dict, List, Optional

from aibrix.batch.job_entity import BatchJob, BatchJobSpec, JobEntityManager
from aibrix.logger import init_logger

logger = init_logger(__name__)


class MongoJobCache(JobEntityManager):
    def __init__(
        self,
        uri: str = "mongodb://localhost:27017",
        database: str = "aibrix",
        collection: str = "batch",
        client: Any = None,
        mongo_collection: Any = None,
    ) -> None:
        super().__init__()

        self.active_jobs: Dict[str, BatchJob] = {}
        self._job_committed_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_updated_handler: Optional[
            Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_deleted_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._client = client
        self._collection = mongo_collection or self._build_collection(
            uri=uri,
            database=database,
            collection=collection,
            client=client,
        )
        self._ensure_indexes()

    def get_job(self, job_id: str) -> Optional[BatchJob]:
        if job_id in self.active_jobs:
            return self.active_jobs[job_id]
        document = self._collection.find_one({"_id": job_id})
        if document is None:
            return None
        return self._document_to_batch_job(document)

    def list_jobs(self) -> List[BatchJob]:
        cursor = self._collection.find({})
        if hasattr(cursor, "sort"):
            cursor = cursor.sort("created_at", -1)
        jobs = [self._document_to_batch_job(document) for document in cursor]
        self.active_jobs = {job.job_id: job for job in jobs if job.job_id is not None}
        return jobs

    async def submit_job(self, session_id: str, job_spec: BatchJobSpec):
        job = BatchJob.new_local(spec=job_spec)
        job.session_id = session_id
        stored_job = await asyncio.to_thread(self._upsert_job, job, None)
        await self.job_committed(stored_job)

    async def update_job_ready(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = self.get_job(job.job_id)
        stored_job = await asyncio.to_thread(self._upsert_job, job, old_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)

    async def update_job_status(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = self.get_job(job.job_id)
        stored_job = await asyncio.to_thread(self._upsert_job, job, old_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)

    async def cancel_job(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = self.get_job(job.job_id)
        stored_job = await asyncio.to_thread(self._upsert_job, job, old_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)

    async def delete_job(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        existing_job = self.get_job(job.job_id) or job
        await asyncio.to_thread(self._collection.delete_one, {"_id": job.job_id})
        self.active_jobs.pop(job.job_id, None)
        await self.job_deleted(existing_job)

    def _build_collection(
        self,
        uri: str,
        database: str,
        collection: str,
        client: Any = None,
    ) -> Any:
        if client is None:
            try:
                from pymongo import MongoClient
            except ImportError as exc:
                raise RuntimeError("pymongo is required to use MongoJobCache") from exc
            client = MongoClient(uri)
        self._client = client
        return client[database][collection]

    def _ensure_indexes(self) -> None:
        self._collection.create_index("session_id")
        self._collection.create_index("state")
        self._collection.create_index("created_at")

    def _upsert_job(self, job: BatchJob, old_job: Optional[BatchJob]) -> BatchJob:
        if job.job_id is None:
            raise ValueError("job_id is required")
        stored_job = job.model_copy(deep=True)
        stored_job_id = stored_job.job_id
        if stored_job_id is None:
            raise ValueError("job_id is required")
        stored_job.metadata.resource_version = self._next_resource_version(old_job)
        document = self._batch_job_to_document(stored_job)
        self._collection.replace_one({"_id": stored_job_id}, document, upsert=True)
        self.active_jobs[stored_job_id] = stored_job
        return stored_job

    def _next_resource_version(self, old_job: Optional[BatchJob]) -> str:
        if old_job is None or old_job.metadata.resource_version is None:
            return "1"
        try:
            return str(int(old_job.metadata.resource_version) + 1)
        except ValueError:
            return "1"

    def _batch_job_to_document(self, job: BatchJob) -> Dict[str, Any]:
        payload = job.model_dump(by_alias=True, mode="json")
        return {
            "_id": job.job_id,
            "session_id": job.session_id,
            "state": job.status.state.value,
            "created_at": payload["status"]["createdAt"],
            "updated_at": datetime.now(timezone.utc).isoformat(),
            "job": payload,
        }

    def _document_to_batch_job(self, document: Dict[str, Any]) -> BatchJob:
        job = BatchJob.model_validate(document["job"])
        if job.job_id is not None:
            self.active_jobs[job.job_id] = job
        return job
