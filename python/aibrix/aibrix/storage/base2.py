import asyncio
from io import BytesIO
from typing import AsyncIterator, Union

from aibrix.storage.base import BaseStorage


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
    ) -> AsyncIterator[tuple[int, bytes]]:
        prefetch_concurrency = max(
            1,
            min(
                self.config.max_session_concurrency,
                len(parts),
            ),
        )

        async def _load_part(
            part_number: int, staged_part_key: str
        ) -> tuple[int, bytes]:
            try:
                return part_number, await self.get_object(staged_part_key)
            except Exception as exc:
                raise ValueError(
                    f"Failed to retrieve part {part_number} for upload {upload_id}"
                ) from exc

        for chunk_start in range(0, len(parts), prefetch_concurrency):
            chunk_parts = parts[chunk_start : chunk_start + prefetch_concurrency]
            chunk_keys = staged_part_keys[
                chunk_start : chunk_start + prefetch_concurrency
            ]
            chunk_results = await asyncio.gather(
                *(
                    _load_part(int(part["part_number"]), staged_part_key)
                    for part, staged_part_key in zip(chunk_parts, chunk_keys)
                )
            )
            for part_number, part_data in chunk_results:
                yield part_number, part_data

    async def complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete small-parts uploads using bounded native multipart reassembly."""
        try:
            metadata_data = await self.get_object(self._multipart_upload_key(upload_id))
            upload_metadata = eval(metadata_data.decode("utf-8"))
        except Exception:
            if self.is_native_multipart_supported():
                return await self._native_complete_multipart_upload(
                    key, upload_id, parts
                )
            raise ValueError(f"Upload ID {upload_id} not found or corrupted")

        content_type = upload_metadata.get("content_type")
        metadata = upload_metadata.get("metadata", {})
        sorted_parts = sorted(parts, key=lambda p: p["part_number"])
        staged_part_keys = [
            self._multipart_upload_part_key(upload_id, int(part["part_number"]))
            for part in sorted_parts
        ]

        if not sorted_parts:
            await self.put_object(key, b"", content_type, metadata)
            await self.abort_multipart_upload(key, upload_id)
            return

        if self.is_native_multipart_supported():
            strict_min_part_size = self._is_strict_multipart_min_part_size_enabled()
            threshold = max(self.config.multipart_threshold, 1)
            native_upload_id: str | None = None
            aggregated_parts: list[dict[str, Union[str, int]]] = []
            buffer = bytearray()
            native_part_number = 1

            try:
                async for _, part_data in self._iter_prefetched_staged_parts(
                    upload_id, sorted_parts, staged_part_keys
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
                    await self.abort_multipart_upload(key, upload_id)
                    return

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

                assert native_upload_id is not None
                await self._native_complete_multipart_upload(
                    key, native_upload_id, aggregated_parts
                )
            except Exception:
                if native_upload_id is not None:
                    await self._native_abort_multipart_upload(key, native_upload_id)
                raise

            await self.abort_multipart_upload(key, upload_id)
            return

        aggregated_data = BytesIO()
        async for _, part_data in self._iter_prefetched_staged_parts(
            upload_id, sorted_parts, staged_part_keys
        ):
            aggregated_data.write(part_data)

        aggregated_data.seek(0)
        await self.put_object(key, aggregated_data, content_type, metadata)
        await self.abort_multipart_upload(key, upload_id)

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
        del buffer[:flush_size]
        etag = await self._native_upload_part(key, upload_id, part_number, chunk)
        parts.append({"part_number": part_number, "etag": etag})
        return part_number + 1
