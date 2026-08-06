# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import pytest

from aibrix.batch.job_lease import RedisJobLease


class FakeRedisLeaseStorage:
    def __init__(self):
        self.calls = []

    async def acquire_lease(self, key, owner_token, ttl_seconds):
        self.calls.append(("acquire", key, owner_token, ttl_seconds))
        return False, 7.5

    async def renew_lease(self, key, owner_token, ttl_seconds):
        self.calls.append(("renew", key, owner_token, ttl_seconds))
        return True

    async def release_lease(self, key, owner_token):
        self.calls.append(("release", key, owner_token))
        return True


@pytest.mark.asyncio
async def test_redis_job_lease_uses_stable_owner_token():
    storage = FakeRedisLeaseStorage()
    lease = RedisJobLease(
        storage,  # type: ignore[arg-type]
        ttl_seconds=30,
        renew_interval_seconds=10,
        owner_token="mds-1:token",
    )

    acquisition = await lease.acquire("job-1")
    renewed = await lease.renew("job-1")
    released = await lease.release("job-1")

    assert acquisition.acquired is False
    assert acquisition.retry_after_seconds == 7.5
    assert renewed is True
    assert released is True
    assert storage.calls == [
        ("acquire", "batchjob_lease:job-1", "mds-1:token", 30),
        ("renew", "batchjob_lease:job-1", "mds-1:token", 30),
        ("release", "batchjob_lease:job-1", "mds-1:token"),
    ]
