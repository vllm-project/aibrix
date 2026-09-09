import ast
import asyncio
from io import BytesIO
from typing import AsyncIterator, Awaitable, Callable, Union

from aibrix.storage.base import (
    LOCAL_PART_PROVIDER_MARKER,
    LOCAL_PART_PROVIDER_MARKER_VALUE,
    BaseStorage,
)


class BaseStorage2(BaseStorage):
    """Alternative BaseStorage with bounded-buffer small-part aggregation."""

    def _is_strict_multipart_min_part_size_enabled(self) -> bool:
        """Return whether native multipart uploads must avoid undersized tails."""
        return bool(self.config.strict_multipart_min_part_size)

    async def _iter_prefetched_staged_parts(
        self,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
        staged_part_keys: list[str],
        *,
        tolerate_part_get_error: bool = False,
        failed_parts: list[dict[str, Union[str, int]]] | None = None,
        local_part_provider: Callable[
            [dict[str, Union[str, int]]], Awaitable[bytes | str | None]
        ]
        | None = None,
    ) -> AsyncIterator[tuple[dict[str, Union[str, int]], bytes]]:
        prefetch_concurrency = max(
            1,
            min(
                self.config.max_session_concurrency,
                len(parts),
            ),
        )

        async def _load_part(
            part: dict[str, Union[str, int]], staged_part_key: str
        ) -> tuple[dict[str, Union[str, int]], bytes]:
            part_number = int(part["part_number"])
            # Only explicitly tagged internal parts may bypass remote reads via
            # the local provider. Unmarked parts still use the staged-object
            # contract first, but tolerant callers may ask the provider for a
            # local fallback body if the staged read fails.
            if (
                local_part_provider is not None
                and part.get(LOCAL_PART_PROVIDER_MARKER)
                == LOCAL_PART_PROVIDER_MARKER_VALUE
            ):
                provided_part = await local_part_provider(part)
                if isinstance(provided_part, str):
                    return part, provided_part.encode("utf-8")
                if isinstance(provided_part, bytes):
                    return part, provided_part
            try:
                return part, await self.get_object(staged_part_key)
            except Exception as exc:
                if local_part_provider is not None and tolerate_part_get_error:
                    provided_part = await local_part_provider(part)
                    if isinstance(provided_part, str):
                        return part, provided_part.encode("utf-8")
                    if isinstance(provided_part, bytes):
                        return part, provided_part
                raise ValueError(
                    f"Failed to retrieve part {part_number} for upload {upload_id}"
                ) from exc

        for chunk_start in range(0, len(parts), prefetch_concurrency):
            chunk_parts = parts[chunk_start : chunk_start + prefetch_concurrency]
            chunk_keys = staged_part_keys[
                chunk_start : chunk_start + prefetch_concurrency
            ]
            tasks = [
                asyncio.create_task(_load_part(part, staged_part_key))
                for part, staged_part_key in zip(chunk_parts, chunk_keys)
            ]
            if tolerate_part_get_error:
                chunk_results = await asyncio.gather(*tasks, return_exceptions=True)
                for part, result in zip(chunk_parts, chunk_results):
                    if isinstance(result, BaseException):
                        if failed_parts is not None:
                            failed_parts.append(dict(part))
                        continue
                    yield result
                continue
            try:
                chunk_results = await asyncio.gather(*tasks)
            except Exception:
                # TODO: Add a config option to allow faulty aggregation so callers
                # can choose whether failed staged-part reads should fail the
                # whole chunk or be tolerated best-effort.
                for task in tasks:
                    if not task.done():
                        task.cancel()
                # Drain cancelled sibling tasks so they do not keep running or
                # surface unhandled exceptions after we re-raise the first error.
                await asyncio.gather(*tasks, return_exceptions=True)
                raise

            for part, part_data in await asyncio.gather(*tasks):
                yield part, part_data

    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
        *,
        tolerate_part_get_error: bool = False,
        local_part_provider: Callable[
            [dict[str, Union[str, int]]], Awaitable[bytes | str | None]
        ]
        | None = None,
    ) -> list[dict[str, Union[str, int]]]:
        """Complete small-parts uploads using bounded native multipart reassembly."""
        try:
            metadata_data = await self.get_object(self._multipart_upload_key(upload_id))
            upload_metadata = ast.literal_eval(metadata_data.decode("utf-8"))
        except Exception:
            if self.is_native_multipart_supported():
                await self._native_complete_multipart_upload(key, upload_id, parts)
                return []
            raise ValueError(f"Upload ID {upload_id} not found or corrupted")

        content_type = upload_metadata.get("content_type")
        metadata = upload_metadata.get("metadata", {})
        sorted_parts = sorted(parts, key=lambda p: p["part_number"])
        staged_part_keys: list[str] = []
        for part in sorted_parts:
            source_upload_id = part.get("source_upload_id")
            part_upload_id = (
                source_upload_id if isinstance(source_upload_id, str) else upload_id
            )
            staged_part_keys.append(
                self._multipart_upload_part_key(
                    part_upload_id, int(part["part_number"])
                )
            )
        failed_parts: list[dict[str, Union[str, int]]] = []

        if not sorted_parts:
            await self.put_object(key, b"", content_type, metadata)
            await self.abort_multipart_upload(key, upload_id)
            return []

        if self.is_native_multipart_supported():
            strict_min_part_size = self._is_strict_multipart_min_part_size_enabled()
            threshold = max(self.config.multipart_threshold, 1)
            native_upload_id: str | None = None
            aggregated_parts: list[dict[str, Union[str, int]]] = []
            buffer = bytearray()
            native_part_number = 1

            try:
                async for _, part_data in self._iter_prefetched_staged_parts(
                    upload_id,
                    sorted_parts,
                    staged_part_keys,
                    tolerate_part_get_error=tolerate_part_get_error,
                    failed_parts=failed_parts,
                    local_part_provider=local_part_provider,
                ):
                    buffer.extend(part_data)
                    flush_threshold = (
                        threshold * 2 if strict_min_part_size else threshold
                    )
                    while len(buffer) >= flush_threshold:
                        if native_upload_id is None:
                            native_upload_id = (
                                await self._native_create_multipart_upload(
                                    key, content_type, metadata
                                )
                            )
                        native_part_number = await self._flush_native_aggregate_part(
                            key,
                            native_upload_id,
                            buffer,
                            native_part_number,
                            aggregated_parts,
                            flush_size=threshold,
                        )

                if strict_min_part_size and len(buffer) < threshold:
                    if aggregated_parts:
                        raise ValueError(
                            "Strict multipart aggregation produced an undersized "
                            "final part"
                        )
                    await self.put_object(key, bytes(buffer), content_type, metadata)
                    if not failed_parts:
                        await self.abort_multipart_upload(key, upload_id)
                    return failed_parts

                if buffer or not aggregated_parts:
                    if native_upload_id is None:
                        native_upload_id = await self._native_create_multipart_upload(
                            key, content_type, metadata
                        )
                    await self._flush_native_aggregate_part(
                        key,
                        native_upload_id,
                        buffer,
                        native_part_number,
                        aggregated_parts,
                    )

                if native_upload_id is None:
                    raise ValueError("Native upload ID was not initialized")
                await self._native_complete_multipart_upload(
                    key, native_upload_id, aggregated_parts
                )
            except Exception:
                if native_upload_id is not None:
                    await self._native_abort_multipart_upload(key, native_upload_id)
                raise

            if not failed_parts:
                await self.abort_multipart_upload(key, upload_id)
            return failed_parts

        aggregated_data = BytesIO()
        async for _, part_data in self._iter_prefetched_staged_parts(
            upload_id,
            sorted_parts,
            staged_part_keys,
            tolerate_part_get_error=tolerate_part_get_error,
            failed_parts=failed_parts,
            local_part_provider=local_part_provider,
        ):
            aggregated_data.write(part_data)

        aggregated_data.seek(0)
        await self.put_object(key, aggregated_data, content_type, metadata)
        if not failed_parts:
            await self.abort_multipart_upload(key, upload_id)
        return failed_parts

    async def abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort multipart upload with best-effort cleanup for base2 small parts."""
        prefix = self._multipart_upload_key(upload_id, "")
        no_multipart_data = True
        try:
            objects_to_delete, _ = await self.list_objects(prefix)

            if objects_to_delete:
                no_multipart_data = False
                try:
                    await self.delete_objects(objects_to_delete)
                except Exception:
                    for obj_key in objects_to_delete:
                        try:
                            await self.delete_object(obj_key)
                        except Exception:
                            pass
        except Exception:
            try:
                await self.delete_object(self._multipart_upload_key(upload_id))
                no_multipart_data = False
            except Exception:
                pass

        if no_multipart_data and self.is_native_multipart_supported():
            await self._native_abort_multipart_upload(key, upload_id)

    async def _flush_native_aggregate_part(
        self,
        key: str,
        upload_id: str,
        buffer: bytearray,
        part_number: int,
        parts: list[dict[str, Union[str, int]]],
        flush_size: int | None = None,
    ) -> int:
        if not buffer:
            return part_number

        if flush_size is None:
            flush_size = len(buffer)
        chunk = bytes(buffer[:flush_size])
        etag = await self._native_upload_part(key, upload_id, part_number, chunk)
        del buffer[:flush_size]
        parts.append({"part_number": part_number, "etag": etag})
        return part_number + 1
