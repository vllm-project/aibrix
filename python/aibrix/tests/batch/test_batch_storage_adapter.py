"""Unit tests for BatchStorageAdapter.finalize_job_output_data metastore-based behavior"""

import uuid
from datetime import datetime, timezone
from unittest.mock import AsyncMock, patch

import pytest

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.batch.storage.adapter import BatchStorageAdapter
from aibrix.storage.base import BaseStorage


def create_test_batch_job(
    job_id: str = "test-job-id",
    input_file_id: str = "test-input-file",
    state: BatchJobState = BatchJobState.CREATED,
    launched: int = 0,
    completed: int = 0,
    failed: int = 0,
    total: int = 0,
    output_file_id: str = "output_123",
    error_file_id: str = "error_123",
    temp_output_file_id: str = "temp_output_123",
    temp_error_file_id: str = "temp_error_123",
) -> BatchJob:
    """Factory function to create BatchJob instances for testing."""
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid=str(uuid.uuid4()),
            creationTimestamp=datetime.now(timezone.utc),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id=input_file_id,
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID=job_id,
            state=state,
            createdAt=datetime.now(timezone.utc),
            outputFileID=output_file_id,
            errorFileID=error_file_id,
            tempOutputFileID=temp_output_file_id,
            tempErrorFileID=temp_error_file_id,
            requestCounts=RequestCountStats(
                total=total,
                launched=launched,
                completed=completed,
                failed=failed,
            ),
        ),
    )


@pytest.fixture
def mock_storage():
    """Create a mock storage instance."""
    storage = AsyncMock(spec=BaseStorage)
    return storage


@pytest.mark.asyncio
async def test_finalize_job_output_data_corrects_counts_from_metastore(mock_storage):
    """Test that finalize_job_output_data correctly calculates counts from metastore keys."""
    adapter = BatchStorageAdapter(mock_storage)

    # Create job with incorrect initial counts
    batch_job = create_test_batch_job(
        job_id="job-123",
        launched=10,  # Wrong - should be corrected to 3 based on metastore
        completed=5,  # Wrong - should be corrected to 2 based on metadata
        failed=2,  # Wrong - should be corrected to 1 based on metadata
        total=15,  # Wrong - should be corrected to 5 based on max index
    )

    # Mock metastore keys for indices 0, 2, 4 (non-consecutive to test max calculation)
    expected_keys = [
        f"batch:{batch_job.job_id}:done/0",
        f"batch:{batch_job.job_id}:done/2",
        f"batch:{batch_job.job_id}:done/4",
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ) as mock_delete_metadata:
                # Setup mocks
                mock_list_keys.return_value = expected_keys

                # Mock metadata responses: idx 0 and 2 are outputs, idx 4 is error
                mock_get_metadata.side_effect = [
                    ("output:etag123", True),  # idx 0: success
                    ("output:etag456", True),  # idx 2: success
                    ("error:etag789", True),  # idx 4: error
                ]

                # Mock storage operations
                mock_storage.complete_multipart_upload.return_value = None

                # Call the method
                await adapter.finalize_job_output_data(batch_job)

                # Verify list_metastore_keys was called with correct prefix
                mock_list_keys.assert_called_once_with(
                    f"batch:{batch_job.job_id}:done/"
                )

                # Verify get_metadata was called for each found index
                assert mock_get_metadata.call_count == 3

                # Verify job request counts were updated correctly
                assert batch_job.status.request_counts.total == 5  # max index (4) + 1
                assert (
                    batch_job.status.request_counts.launched == 3
                )  # number of keys found
                assert (
                    batch_job.status.request_counts.completed == 2
                )  # 2 outputs (idx 0, 2)
                assert batch_job.status.request_counts.failed == 1  # 1 error (idx 4)

                # Verify storage complete_multipart_upload was called correctly
                assert mock_storage.complete_multipart_upload.call_count == 2

                # Check output parts
                output_call = mock_storage.complete_multipart_upload.call_args_list[0]
                output_parts = output_call[0][2]  # third argument is the parts list
                expected_output_parts = [
                    {"etag": "etag123", "part_number": 0},
                    {"etag": "etag456", "part_number": 2},
                ]
                assert output_parts == expected_output_parts

                # Check error parts
                error_call = mock_storage.complete_multipart_upload.call_args_list[1]
                error_parts = error_call[0][2]  # third argument is the parts list
                expected_error_parts = [{"etag": "etag789", "part_number": 4}]
                assert error_parts == expected_error_parts

                # Verify cleanup - metadata should be deleted for all valid keys
                assert mock_delete_metadata.call_count == 3


@pytest.mark.asyncio
async def test_finalize_job_output_data_handles_missing_metadata(mock_storage):
    """Test handling when some keys exist in list but metadata is missing."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(job_id="job-456")

    expected_keys = [
        f"batch:{batch_job.job_id}:done/0",
        f"batch:{batch_job.job_id}:done/1",
        f"batch:{batch_job.job_id}:done/2",
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ) as mock_delete_metadata:
                mock_list_keys.return_value = expected_keys

                # Mock metadata: idx 0 exists, idx 1 missing, idx 2 exists
                mock_get_metadata.side_effect = [
                    ("output:etag123", True),  # idx 0: exists
                    ("", False),  # idx 1: missing metadata
                    ("error:etag789", True),  # idx 2: exists
                ]

                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # Should calculate based on all keys found, but only process existing metadata
                assert batch_job.status.request_counts.total == 3  # max index (2) + 1
                assert (
                    batch_job.status.request_counts.launched == 3
                )  # all keys found in list
                assert (
                    batch_job.status.request_counts.completed == 1
                )  # only idx 0 had output
                assert (
                    batch_job.status.request_counts.failed == 1
                )  # only idx 2 had error

                # Should only delete metadata for existing keys (idx 0 and 2)
                assert mock_delete_metadata.call_count == 2


@pytest.mark.asyncio
async def test_finalize_job_output_data_handles_empty_metastore(mock_storage):
    """Test handling when no keys are found in metastore."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(
        job_id="job-789", launched=5, total=10
    )  # Initial wrong counts

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ) as mock_delete_metadata:
                # No keys found in metastore
                mock_list_keys.return_value = []

                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # Verify counts are corrected to zero
                assert batch_job.status.request_counts.total == 0
                assert batch_job.status.request_counts.launched == 0
                assert batch_job.status.request_counts.completed == 0
                assert batch_job.status.request_counts.failed == 0

                # get_metadata should not be called
                mock_get_metadata.assert_not_called()

                # delete_metadata should not be called
                mock_delete_metadata.assert_not_called()

                # Storage operations should still be called with empty lists
                assert mock_storage.complete_multipart_upload.call_count == 2


@pytest.mark.asyncio
async def test_finalize_job_output_data_handles_invalid_key_formats(mock_storage):
    """Test handling of invalid key formats in metastore."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(job_id="job-invalid")

    keys_with_invalid = [
        f"batch:{batch_job.job_id}:done/0",  # valid
        f"batch:{batch_job.job_id}:done/abc",  # invalid - non-numeric
        f"batch:{batch_job.job_id}:done/2",  # valid
        f"batch:{batch_job.job_id}:done/",  # invalid - empty index
        "batch:other-job:done/1",  # invalid - different job prefix
        "other:format:key/3",  # invalid - completely different format
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ) as mock_delete_metadata:
                mock_list_keys.return_value = keys_with_invalid

                # Only valid keys should get metadata calls
                mock_get_metadata.side_effect = [
                    ("output:etag123", True),  # idx 0
                    ("output:etag456", True),  # idx 2
                ]

                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # Should only process valid indices (0, 2)
                assert (
                    batch_job.status.request_counts.total == 3
                )  # max valid index (2) + 1
                assert batch_job.status.request_counts.launched == 2  # valid keys found
                assert (
                    batch_job.status.request_counts.completed == 2
                )  # both were outputs
                assert batch_job.status.request_counts.failed == 0

                # Should only call metadata operations for valid keys
                assert mock_get_metadata.call_count == 2
                assert mock_delete_metadata.call_count == 2


@pytest.mark.asyncio
async def test_finalize_job_output_data_no_update_when_counts_match(mock_storage):
    """Test that job counts are not updated when they already match metastore data."""
    adapter = BatchStorageAdapter(mock_storage)

    # Create job with correct initial counts
    batch_job = create_test_batch_job(
        job_id="job-correct",
        launched=2,  # Correct
        completed=1,  # Correct
        failed=1,  # Correct
        total=2,  # Correct (max index 1 + 1)
    )

    expected_keys = [
        f"batch:{batch_job.job_id}:done/0",
        f"batch:{batch_job.job_id}:done/1",
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ):
                mock_list_keys.return_value = expected_keys
                mock_get_metadata.side_effect = [
                    ("output:etag123", True),  # idx 0: success
                    ("error:etag456", True),  # idx 1: error
                ]
                mock_storage.complete_multipart_upload.return_value = None

                # Store initial counts to verify they don't change
                initial_counts = {
                    "total": batch_job.status.request_counts.total,
                    "launched": batch_job.status.request_counts.launched,
                    "completed": batch_job.status.request_counts.completed,
                    "failed": batch_job.status.request_counts.failed,
                }

                await adapter.finalize_job_output_data(batch_job)

                # Verify counts remain the same (since they were already correct)
                assert batch_job.status.request_counts.total == initial_counts["total"]
                assert (
                    batch_job.status.request_counts.launched
                    == initial_counts["launched"]
                )
                assert (
                    batch_job.status.request_counts.completed
                    == initial_counts["completed"]
                )
                assert (
                    batch_job.status.request_counts.failed == initial_counts["failed"]
                )


@pytest.mark.asyncio
async def test_finalize_job_output_data_sequential_indices_calculation(mock_storage):
    """Test proper calculation when indices are sequential starting from 0."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(job_id="job-sequential")

    # Sequential indices 0,1,2,3,4
    expected_keys = [
        f"batch:{batch_job.job_id}:done/0",
        f"batch:{batch_job.job_id}:done/1",
        f"batch:{batch_job.job_id}:done/2",
        f"batch:{batch_job.job_id}:done/3",
        f"batch:{batch_job.job_id}:done/4",
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ):
                mock_list_keys.return_value = expected_keys
                mock_get_metadata.side_effect = [
                    ("output:etag0", True),
                    ("output:etag1", True),
                    ("error:etag2", True),
                    ("output:etag3", True),
                    ("error:etag4", True),
                ]
                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # For sequential indices 0-4, total should be 5 (max index 4 + 1)
                assert batch_job.status.request_counts.total == 5
                assert batch_job.status.request_counts.launched == 5  # All 5 keys found
                assert (
                    batch_job.status.request_counts.completed == 3
                )  # indices 0,1,3 are outputs
                assert (
                    batch_job.status.request_counts.failed == 2
                )  # indices 2,4 are errors


@pytest.mark.asyncio
async def test_finalize_job_output_data_single_request(mock_storage):
    """Test edge case with single request (index 0 only)."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(
        job_id="job-single", launched=10, total=20
    )  # Wrong initial counts

    expected_keys = [f"batch:{batch_job.job_id}:done/0"]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ):
                mock_list_keys.return_value = expected_keys
                mock_get_metadata.side_effect = [("output:etag0", True)]
                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # For single request at index 0, total should be 1
                assert batch_job.status.request_counts.total == 1
                assert batch_job.status.request_counts.launched == 1
                assert batch_job.status.request_counts.completed == 1
                assert batch_job.status.request_counts.failed == 0


@pytest.mark.asyncio
async def test_finalize_job_output_data_preserves_part_numbers(mock_storage):
    """Test that part numbers in multipart upload correspond to original indices."""
    adapter = BatchStorageAdapter(mock_storage)
    batch_job = create_test_batch_job(job_id="job-parts")

    # Non-sequential indices to test part number preservation
    expected_keys = [
        f"batch:{batch_job.job_id}:done/5",  # Large index
        f"batch:{batch_job.job_id}:done/10",  # Even larger index
        f"batch:{batch_job.job_id}:done/15",  # Largest index
    ]

    with patch(
        "aibrix.batch.storage.adapter.list_metastore_keys", new_callable=AsyncMock
    ) as mock_list_keys:
        with patch(
            "aibrix.batch.storage.adapter.get_metadata", new_callable=AsyncMock
        ) as mock_get_metadata:
            with patch(
                "aibrix.batch.storage.adapter.delete_metadata", new_callable=AsyncMock
            ):
                mock_list_keys.return_value = expected_keys
                mock_get_metadata.side_effect = [
                    ("output:etag5", True),  # idx 5
                    ("output:etag10", True),  # idx 10
                    ("error:etag15", True),  # idx 15
                ]
                mock_storage.complete_multipart_upload.return_value = None

                await adapter.finalize_job_output_data(batch_job)

                # Verify part numbers match original indices
                output_call = mock_storage.complete_multipart_upload.call_args_list[0]
                output_parts = output_call[0][2]
                expected_output_parts = [
                    {"etag": "etag5", "part_number": 5},
                    {"etag": "etag10", "part_number": 10},
                ]
                assert output_parts == expected_output_parts

                error_call = mock_storage.complete_multipart_upload.call_args_list[1]
                error_parts = error_call[0][2]
                expected_error_parts = [{"etag": "etag15", "part_number": 15}]
                assert error_parts == expected_error_parts

                # Total should be max index + 1
                assert batch_job.status.request_counts.total == 16  # 15 + 1
