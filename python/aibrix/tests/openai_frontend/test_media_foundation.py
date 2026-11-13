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

"""Media schema/inspection, fetching and rewriting contracts."""

from __future__ import annotations

import asyncio
import os
from pathlib import Path
from typing import List, Union

import pytest

from aibrix.openai_frontend.common.artifact_fetcher import (
    ArtifactFetcher,
    ArtifactFetchError,
    DefaultArtifactFetcher,
    FetchResult,
)
from aibrix.openai_frontend.proxy.media.request_inspector import (
    MediaURLInspector,
    SglangGenerateInspector,
)
from aibrix.openai_frontend.proxy.media.request_rewriter import RequestRewriter


class TestMediaURLInspector:
    def test_new_schema_media_field_is_recognized_without_inspector_list(
        self, monkeypatch
    ) -> None:
        from typing import Literal

        from pydantic import BaseModel, Field

        from aibrix.openai_frontend.proxy.media import request_inspector
        from aibrix.openai_frontend.schemas.media import media_url_fields

        class ExtraMedia(BaseModel):
            type: Literal["input_extra"] = "input_extra"
            source: str = Field(alias="extra_url", json_schema_extra={"media_url": ""})

        monkeypatch.setattr(
            request_inspector,
            "_MEDIA_URL_FIELDS",
            {**request_inspector._MEDIA_URL_FIELDS, **media_url_fields(ExtraMedia)},
        )
        request = {"input": [{"type": "input_extra", "extra_url": "https://h/extra"}]}
        locations = MediaURLInspector().extract_urls(request)
        assert [(loc.url, loc.path, loc.media_type) for loc in locations] == [
            ("https://h/extra", ["input", 0, "extra_url"], "extra_url")
        ]
        assert MediaURLInspector([]).extract_urls(request) == []

    def test_sglang_native_media_fields_follow_schema(self):
        from aibrix.openai_frontend.schemas.sglang import (
            SglangGenerateMediaFields,
        )

        request = {
            "image_data": ["https://h/image", ["tos://b/image"]],
            "video_data": {"url": "s3://b/video", "timestamp": 7},
            "audio_data": "gs://b/audio",
            "vendor": {"url": "https://h/not-media"},
        }
        assert (
            SglangGenerateMediaFields.model_validate(request).model_dump(
                exclude_unset=True
            )
            == request
        )
        assert MediaURLInspector().extract_urls(request) == []
        locations = SglangGenerateInspector().extract_urls(request)
        assert [(loc.path, loc.media_type) for loc in locations] == [
            (["image_data", 0], "image_url"),
            (["image_data", 1, 0], "image_url"),
            (["video_data", "url"], "video_url"),
            (["audio_data"], "audio_url"),
        ]
        assert len(SglangGenerateInspector(["video_url"]).extract_urls(request)) == 1

    @pytest.mark.parametrize(
        "part_type,url_field",
        [("input_image", "image_url"), ("input_file", "file_url")],
    )
    def test_responses_schema_roundtrip_and_url_rewrite(
        self, monkeypatch, tmp_path, part_type, url_field
    ):
        from pydantic import TypeAdapter

        from aibrix.openai_frontend.proxy.media.request_rewriter import RequestRewriter
        from aibrix.openai_frontend.schemas.responses import (
            ResponseInputFile,
            ResponseInputImage,
        )

        adapter = TypeAdapter(Union[ResponseInputImage, ResponseInputFile])
        original = {
            "type": part_type,
            url_field: "https://h/media.bin",
            **({"detail": "auto"} if part_type == "input_image" else {}),
            "vendor_extra": {"keep": True},
        }
        parsed = adapter.validate_python(original)
        assert parsed.model_dump(by_alias=True, exclude_unset=True) == original
        request = {"input": [original]}
        inspector = MediaURLInspector()
        assert inspector.extract_urls(request)[0].path == ["input", 0, url_field]
        assert MediaURLInspector([]).extract_urls(request) == []
        for source in ("file_id", "file_data"):
            alternative = {
                "type": part_type,
                source: "opaque",
                **({"detail": "auto"} if part_type == "input_image" else {}),
            }
            assert parsed.model_dump().get(url_field) is not None
            assert (
                adapter.validate_python(alternative).model_dump().get(url_field) is None
            )
            assert inspector.extract_urls({"input": [alternative]}) == []
        assert (
            inspector.extract_urls(
                {
                    "input": [
                        {"type": part_type, url_field: {"invalid": True}},
                        {"type": "vendor", url_field: "https://h/not-media.bin"},
                    ]
                }
            )
            == []
        )
        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: _StubDownloader(),
        )

        async def _run():
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            results = []
            try:
                rewritten, results = await RequestRewriter(inspector, fetcher).rewrite(
                    request
                )
                assert rewritten["input"][0][url_field].startswith("file://")
                assert rewritten["input"][0]["vendor_extra"] == {"keep": True}
                assert request["input"][0] == original
            finally:
                await fetcher.cleanup(results)

        asyncio.run(_run())
        assert not list(tmp_path.iterdir())

    def test_finds_all_native_media_shapes(self):
        req = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "image_url", "image_url": {"url": "tos://b/a.png"}},
                        {"type": "video_url", "video_url": {"url": "s3://b/v.mp4"}},
                        {"type": "audio_url", "audio_url": {"url": "https://h/a.wav"}},
                        {"type": "image_url", "image_url": "hf://org/repo/x.jpg"},
                        {
                            "type": "image_url",
                            "image_url": {"url": "data:image/png;base64,AA"},
                        },
                        {
                            "type": "image_url",
                            "image_url": {"url": "file:///tmp/x.png"},
                        },
                    ],
                }
            ],
            "input": [
                {
                    "role": "user",
                    "content": [{"type": "input_image", "image_url": "gs://b/r.png"}],
                }
            ],
            "image_data": ["https://h/1.png", ["tos://b/2.png"]],
            "video_data": "s3://b/3.mp4",
        }
        locs = MediaURLInspector().extract_urls(req)
        got = {(loc.url, tuple(loc.path), loc.media_type) for loc in locs}
        assert got == {
            (
                "tos://b/a.png",
                ("messages", 0, "content", 0, "image_url", "url"),
                "image_url",
            ),
            (
                "s3://b/v.mp4",
                ("messages", 0, "content", 1, "video_url", "url"),
                "video_url",
            ),
            (
                "https://h/a.wav",
                ("messages", 0, "content", 2, "audio_url", "url"),
                "audio_url",
            ),
            (
                "hf://org/repo/x.jpg",
                ("messages", 0, "content", 3, "image_url"),
                "image_url",
            ),
            ("gs://b/r.png", ("input", 0, "content", 0, "image_url"), "image_url"),
        }


class _StubDownloader:
    def __init__(self, payload: bytes = b"PIXELS", fail: bool = False):
        self.payload = payload
        self.fail = fail
        self.calls: List[tuple] = []

    async def download(self, url, local_path, credentials=None):
        self.calls.append((url, credentials))
        if self.fail:
            raise RuntimeError("403 Forbidden")
        Path(local_path).mkdir(parents=True, exist_ok=True)
        (Path(local_path) / os.path.basename(url)).write_bytes(self.payload)
        return local_path


class TestDefaultArtifactFetcher:
    @pytest.mark.parametrize("batch", [False, True])
    def test_cancelled_download_discards_partial_files(
        self, monkeypatch, tmp_path, batch
    ):
        async def run():
            started = asyncio.Event()

            class Downloader:
                async def download(self, url, local_path, credentials=None):
                    (Path(local_path) / "partial.bin").write_bytes(b"partial")
                    started.set()
                    await asyncio.Event().wait()

            monkeypatch.setattr(
                "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                lambda url: Downloader(),
            )
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            task = asyncio.create_task(
                fetcher.fetch_many(["https://h/media.bin"])
                if batch
                else fetcher.fetch("https://h/media.bin")
            )
            try:
                await asyncio.wait_for(started.wait(), 5)
                task.cancel("client disconnected")
                with pytest.raises(asyncio.CancelledError):
                    await task
                assert not fetcher._active_tmpdirs
                assert not list(tmp_path.iterdir())
            finally:
                task.cancel()
                await asyncio.gather(task, return_exceptions=True)
                fetcher.cleanup_all()

        asyncio.run(run())

    @pytest.mark.parametrize("source", ["inline", "s3", "gcs", "hf"])
    @pytest.mark.parametrize("batch", [False, True])
    @pytest.mark.parametrize("write_fails", [False, True])
    def test_cancel_waits_for_thread_before_discard(
        self, monkeypatch, tmp_path, source, batch, write_fails
    ):
        import threading

        from aibrix.runtime.downloaders import get_downloader

        started = threading.Event()
        release = threading.Event()
        finished = threading.Event()
        write_bytes = Path.write_bytes

        def blocked_write(path, data):
            started.set()
            try:
                assert release.wait(5)
                # The parent must survive cancellation until this writer stops.
                assert path.parent.exists()
                if write_fails:
                    raise OSError("writer failed after cancellation")
                return write_bytes(path, data)
            finally:
                finished.set()

        async def run():
            if source == "inline":
                monkeypatch.setattr(Path, "write_bytes", blocked_write)
                url = "data:image/png;base64,UElYRUxT"
            else:
                url = f"{source}://bucket/media.bin"
                downloader = get_downloader(url)

                def download(source_url, local_path, credentials=None):
                    blocked_write(Path(local_path) / "media.bin", b"PIXELS")
                    return local_path

                monkeypatch.setattr(downloader, "_download_sync", download)
                monkeypatch.setattr(
                    "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                    lambda url: downloader,
                )
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            task = asyncio.create_task(
                fetcher.fetch_many([url]) if batch else fetcher.fetch(url)
            )
            try:
                assert await asyncio.to_thread(started.wait, 5)
                task.cancel("client disconnected")
                await asyncio.sleep(0)
                task.cancel("cancel again while closing")
                for _ in range(3):
                    await asyncio.sleep(0)
                assert not task.done()
                assert fetcher._active_tmpdirs
                release.set()
                with pytest.raises(asyncio.CancelledError):
                    await asyncio.wait_for(task, 5)
                assert finished.is_set()
                assert not fetcher._active_tmpdirs
                assert not list(tmp_path.iterdir())
            finally:
                release.set()
                await asyncio.gather(task, return_exceptions=True)
                await asyncio.to_thread(finished.wait, 5)
                fetcher.cleanup_all()

        asyncio.run(run())

    @pytest.mark.parametrize("cancel_batch", [False, True])
    def test_batch_cleans_completed_and_interrupted_downloads(
        self, monkeypatch, tmp_path, cancel_batch
    ):
        async def run():
            partial_started = asyncio.Event()
            fail = asyncio.Event()

            class Downloader:
                async def download(self, url, local_path, credentials=None):
                    (Path(local_path) / "media.bin").write_bytes(b"PIXELS")
                    if url.endswith("complete"):
                        return local_path
                    partial_started.set()
                    if url.endswith("failure"):
                        await fail.wait()
                        raise RuntimeError("download failed")
                    await asyncio.Event().wait()

            monkeypatch.setattr(
                "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                lambda url: Downloader(),
            )
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            urls = ["https://h/complete", "https://h/partial"]
            if not cancel_batch:
                urls.append("https://h/failure")
            task = asyncio.create_task(fetcher.fetch_many(urls))
            try:
                await asyncio.wait_for(partial_started.wait(), 5)
                await asyncio.sleep(0)
                if cancel_batch:
                    task.cancel()
                    error = asyncio.CancelledError
                else:
                    fail.set()
                    error = ArtifactFetchError
                with pytest.raises(error):
                    await task
                assert not fetcher._active_tmpdirs
                assert not list(tmp_path.iterdir())
            finally:
                task.cancel()
                await asyncio.gather(task, return_exceptions=True)
                fetcher.cleanup_all()

        asyncio.run(run())

    @pytest.mark.asyncio
    async def test_repeated_cancel_waits_for_batch_cleanup(self, monkeypatch, tmp_path):
        started = asyncio.Event()
        cleanup_started = asyncio.Event()
        release_cleanup = asyncio.Event()

        class Downloader:
            async def download(self, url, local_path, credentials=None):
                (Path(local_path) / "media.bin").write_bytes(b"PIXELS")
                if url.endswith("complete"):
                    return local_path
                started.set()
                await asyncio.Event().wait()

        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: Downloader(),
        )
        fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
        real_cleanup = fetcher.cleanup

        async def delayed_cleanup(results):
            cleanup_started.set()
            await release_cleanup.wait()
            await real_cleanup(results)

        monkeypatch.setattr(fetcher, "cleanup", delayed_cleanup)
        task = asyncio.create_task(
            fetcher.fetch_many(["https://h/complete", "https://h/partial"])
        )
        try:
            await asyncio.wait_for(started.wait(), 5)
            task.cancel("first cancel")
            await asyncio.wait_for(cleanup_started.wait(), 5)
            task.cancel("second cancel")
            await asyncio.sleep(0)
            assert not task.done()
            release_cleanup.set()
            # Python 3.10's gather() does not preserve Task.cancel() messages.
            with pytest.raises(asyncio.CancelledError):
                await asyncio.wait_for(task, 5)
            assert task.cancelled()
            assert not fetcher._active_tmpdirs
            assert not list(tmp_path.iterdir())
        finally:
            release_cleanup.set()
            await asyncio.gather(task, return_exceptions=True)
            fetcher.cleanup_all()

    @pytest.mark.parametrize("max_download_bytes", [512 * 1024 * 1024, None])
    def test_returns_file_uri_of_the_file(
        self, monkeypatch, tmp_path, max_download_bytes
    ):
        stub = _StubDownloader()
        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: stub,
        )

        async def _run():
            fetcher = DefaultArtifactFetcher(
                base_temp_dir=str(tmp_path), max_download_bytes=max_download_bytes
            )
            res = await fetcher.fetch("tos://bucket/dir/cat.png")
            assert res.local_path.startswith("file://")
            local = Path(res.local_path[len("file://") :])
            assert local.name == "cat.png" and local.read_bytes() == b"PIXELS"
            assert res.size_bytes == len(b"PIXELS")
            assert str(local).startswith(str(tmp_path.resolve()))
            await fetcher.cleanup([res])
            assert not local.exists()

        asyncio.run(_run())

    def test_tos_credentials_default_from_downloader_env(self, monkeypatch, tmp_path):
        # Download credentials come from the DOWNLOADER_* family (raw
        # TOS_ACCESS_KEY/...), never from STORAGE_* which configures uploads.
        stub = _StubDownloader()
        monkeypatch.setattr(
            "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
            lambda url: stub,
        )
        for name in (
            "TOS_ACCESS_KEY",
            "TOS_SECRET_KEY",
            "TOS_ENDPOINT",
            "TOS_REGION",
        ):
            monkeypatch.delenv(name, raising=False)
        monkeypatch.setenv("TOS_ACCESS_KEY", "ak")
        monkeypatch.setenv("TOS_SECRET_KEY", "sk")
        monkeypatch.setenv("TOS_ENDPOINT", "tos-x.volces.com")
        monkeypatch.setenv("TOS_REGION", "cn-x")
        # STORAGE_* must be ignored even if set.
        monkeypatch.setenv("STORAGE_TOS_ACCESS_KEY", "storage-ak")
        monkeypatch.setenv("STORAGE_TOS_SECRET_KEY", "storage-sk")
        # Isolate from the import-time snapshot / real environment.
        from aibrix import envs as _envs

        for attr in (
            "DOWNLOADER_TOS_ACCESS_KEY",
            "DOWNLOADER_TOS_SECRET_KEY",
            "DOWNLOADER_TOS_ENDPOINT",
            "DOWNLOADER_TOS_REGION",
            "STORAGE_TOS_ACCESS_KEY",
            "STORAGE_TOS_SECRET_KEY",
            "STORAGE_TOS_ENDPOINT",
            "STORAGE_TOS_REGION",
        ):
            monkeypatch.setattr(_envs, attr, None, raising=False)

        async def _run():
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            res = await fetcher.fetch("tos://bucket/cat.png")
            await fetcher.cleanup([res])

        asyncio.run(_run())
        assert stub.calls[0][1] == {
            "TOS_ACCESS_KEY": "ak",
            "TOS_SECRET_KEY": "sk",
            "endpoint": "tos-x.volces.com",
            "region": "cn-x",
        }

    def test_failure_and_size_limit_leave_nothing_behind(self, monkeypatch, tmp_path):
        async def _run():
            monkeypatch.setattr(
                "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                lambda url: _StubDownloader(fail=True),
            )
            fetcher = DefaultArtifactFetcher(base_temp_dir=str(tmp_path))
            with pytest.raises(ArtifactFetchError, match="403"):
                await fetcher.fetch("https://h/x.png")

            monkeypatch.setattr(
                "aibrix.openai_frontend.common.artifact_fetcher.get_downloader",
                lambda url: _StubDownloader(payload=b"x" * 100),
            )
            small = DefaultArtifactFetcher(
                base_temp_dir=str(tmp_path), max_download_bytes=10
            )
            with pytest.raises(ArtifactFetchError, match="exceeds limit"):
                await small.fetch("https://h/big.png")
            assert list(tmp_path.iterdir()) == []

        asyncio.run(_run())


class TestMediaMessageURLs:
    def setup_method(self):
        self.inspector = MediaURLInspector()

    def test_single_image_url(self):
        request = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "Describe this image"},
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/img.png"},
                        },
                    ],
                }
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 1
        assert locations[0].url == "https://cdn.example.com/img.png"
        assert locations[0].path == ["messages", 0, "content", 1, "image_url", "url"]

    def test_multiple_image_urls_across_messages(self):
        request = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/a.jpg"},
                        },
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "And this one"},
                        {
                            "type": "image_url",
                            "image_url": {"url": "s3://bucket/key"},
                        },
                    ],
                },
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 2
        assert locations[0].url == "https://cdn.example.com/a.jpg"
        assert locations[0].path == ["messages", 0, "content", 0, "image_url", "url"]
        assert locations[1].url == "s3://bucket/key"
        assert locations[1].path == ["messages", 1, "content", 1, "image_url", "url"]

    def test_local_file_url_ignored(self):
        request = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "file:///tmp/local.png"},
                        },
                    ],
                }
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 0

    def test_data_uri_ignored(self):
        request = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "data:image/png;base64,abc123"},
                        },
                    ],
                }
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 0

    def test_string_content_skipped(self):
        request = {
            "messages": [
                {"role": "user", "content": "Just a text message"},
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 0

    def test_no_messages(self):
        locations = self.inspector.extract_urls({})
        assert len(locations) == 0

    def test_tos_url_detected(self):
        request = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "tos://bucket/path/img.png"},
                        },
                    ],
                }
            ]
        }
        locations = self.inspector.extract_urls(request)
        assert len(locations) == 1
        assert locations[0].url == "tos://bucket/path/img.png"


class _MockFetcher(ArtifactFetcher):
    """Mock fetcher that maps URLs to local paths without real downloads."""

    def __init__(self, url_to_path: dict[str, str]):
        self._url_to_path = url_to_path
        self.cleaned_up: list[list[FetchResult]] = []

    async def fetch(self, url: str, credentials: dict | None = None) -> FetchResult:
        local = self._url_to_path.get(url, f"file:///tmp/mock_{url.split('/')[-1]}")
        return FetchResult(original_url=url, local_path=local, temp_dir=None)

    async def fetch_many(
        self,
        urls: list[str],
        credentials_map: dict[str, dict] | None = None,
    ) -> list[FetchResult]:
        return [await self.fetch(u) for u in urls]

    async def cleanup(self, results: list[FetchResult]) -> None:
        self.cleaned_up.append(results)


class TestRequestRewriter:
    @pytest.mark.asyncio
    async def test_credential_patterns_exact_first_then_insertion_order(self):
        urls = [
            "https://cdn.example.com/private.png?sig=x",
            "https://cdn.example.com/other.png",
            "https://cdn.example.com/OTHER.PNG",
            "https://unmatched.example.com/a.png",
            "https://region.cdn.example.com/public.png?sig=x#fragment",
            "https://foreign.example/.cdn.example.com/public.png",
            "https://foreign.example/?u=.cdn.example.com/public.png",
            "https://region.cdn.example.com@foreign.example/public.png",
            "https://region.cdn.example.com/private/nested.png",
            "https://region.cdn.example.com.evil.example/public.png",
            "https://cdn.example.com/private.png?sig=y#other",
            "https://cdn.example.com/private.png",
        ]
        seen = {}

        class Fetcher(_MockFetcher):
            async def fetch_many(self, urls, credentials_map=None):
                seen.update(credentials_map or {})
                return await super().fetch_many(urls, credentials_map)

        exact, wildcard, later = (
            {"token": "exact"},
            {"token": "first"},
            {"token": "later"},
        )
        rewriter = RequestRewriter(
            MediaURLInspector(),
            Fetcher({}),
            {
                urls[0]: exact,
                "https://cdn.example.com/*.png*": wildcard,
                "https://cdn.example.com/*": later,
                "https://*.cdn.example.com/*.png*": wildcard,
            },
        )
        await rewriter.rewrite(
            {
                "messages": [
                    {
                        "role": "user",
                        "content": [
                            {"type": "image_url", "image_url": {"url": url}}
                            for url in urls
                        ],
                    }
                ]
            }
        )
        assert seen == {
            urls[0]: exact,
            urls[1]: wildcard,
            urls[2]: later,
            urls[4]: wildcard,
            urls[10]: wildcard,
            urls[11]: wildcard,
        }
        # A literal signed URL never falls back to path-only matching.
        exact_only = RequestRewriter(MediaURLInspector(), Fetcher({}), {urls[0]: exact})
        assert exact_only._credentials_for(urls[10]) is None
        assert exact_only._credentials_for(urls[11]) is None

    @pytest.mark.asyncio
    async def test_rewrites_single_image_url(self):
        fetcher = _MockFetcher(
            {"https://cdn.example.com/img.png": "file:///tmp/downloaded_img.png"}
        )
        inspector = MediaURLInspector()
        rewriter = RequestRewriter(inspector, fetcher)

        request = {
            "model": "gpt-4",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "Describe this"},
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/img.png"},
                        },
                    ],
                }
            ],
        }

        rewritten, results = await rewriter.rewrite(request)
        url_in_rewritten = rewritten["messages"][0]["content"][1]["image_url"]["url"]
        assert url_in_rewritten == "file:///tmp/downloaded_img.png"
        assert len(results) == 1
        assert results[0].original_url == "https://cdn.example.com/img.png"

    @pytest.mark.asyncio
    async def test_rewrites_multiple_urls(self):
        fetcher = _MockFetcher(
            {
                "https://cdn.example.com/a.jpg": "file:///tmp/a.jpg",
                "s3://bucket/key": "file:///tmp/s3_key",
            }
        )
        inspector = MediaURLInspector()
        rewriter = RequestRewriter(inspector, fetcher)

        request = {
            "model": "gpt-4",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/a.jpg"},
                        },
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "s3://bucket/key"},
                        },
                    ],
                },
            ],
        }

        rewritten, results = await rewriter.rewrite(request)
        assert (
            rewritten["messages"][0]["content"][0]["image_url"]["url"]
            == "file:///tmp/a.jpg"
        )
        assert (
            rewritten["messages"][1]["content"][0]["image_url"]["url"]
            == "file:///tmp/s3_key"
        )
        assert len(results) == 2

    @pytest.mark.asyncio
    async def test_no_urls_returns_original(self):
        fetcher = _MockFetcher({})
        inspector = MediaURLInspector()
        rewriter = RequestRewriter(inspector, fetcher)

        request = {
            "model": "gpt-4",
            "messages": [
                {"role": "user", "content": "Just text"},
            ],
        }

        rewritten, results = await rewriter.rewrite(request)
        assert rewritten is request  # same object, not deep-copied
        assert results == []

    @pytest.mark.asyncio
    async def test_does_not_mutate_original(self):
        fetcher = _MockFetcher(
            {"https://cdn.example.com/img.png": "file:///tmp/local.png"}
        )
        inspector = MediaURLInspector()
        rewriter = RequestRewriter(inspector, fetcher)

        request = {
            "model": "gpt-4",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/img.png"},
                        },
                    ],
                }
            ],
        }
        original_url = request["messages"][0]["content"][0]["image_url"]["url"]

        rewritten, _ = await rewriter.rewrite(request)
        # Original must be unchanged
        assert request["messages"][0]["content"][0]["image_url"]["url"] == original_url
        assert (
            rewritten["messages"][0]["content"][0]["image_url"]["url"]
            == "file:///tmp/local.png"
        )

    @pytest.mark.asyncio
    async def test_duplicate_url_fetched_once(self):
        fetcher = _MockFetcher(
            {"https://cdn.example.com/img.png": "file:///tmp/local.png"}
        )
        inspector = MediaURLInspector()
        rewriter = RequestRewriter(inspector, fetcher)

        request = {
            "model": "gpt-4",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/img.png"},
                        },
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {
                            "type": "image_url",
                            "image_url": {"url": "https://cdn.example.com/img.png"},
                        },
                    ],
                },
            ],
        }

        rewritten, results = await rewriter.rewrite(request)
        assert (
            rewritten["messages"][0]["content"][0]["image_url"]["url"]
            == "file:///tmp/local.png"
        )
        assert (
            rewritten["messages"][1]["content"][0]["image_url"]["url"]
            == "file:///tmp/local.png"
        )
        assert len(results) == 1
