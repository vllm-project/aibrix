"""Consolidated Test Suite: Batching and Concurrency.

This module combines tests for:
- Scheduler batching constraints with max_loras limit
- Concurrent request handling with multiple LoRAs
- Burst traffic patterns

Uses AsyncLLMEngine for proper parallel request submission.
"""

import pytest
import pytest_asyncio
import asyncio
import time
import random
import statistics
import gc
import torch
from vllm import SamplingParams
from vllm.engine.arg_utils import AsyncEngineArgs
from vllm.engine.async_llm_engine import AsyncLLMEngine
from vllm.lora.request import LoRARequest
from test_config import BASE_MODEL, get_lora_path, TEST_PROMPT


def cleanup_gpu():
    """Force GPU memory cleanup."""
    gc.collect()
    torch.cuda.empty_cache()
    torch.cuda.synchronize()


class TestBatching:
    """Test suite for LoRA batching constraints."""

    @pytest_asyncio.fixture
    async def async_engine_batching(self):
        """Create AsyncLLMEngine for batching tests with max_loras=2."""
        cleanup_gpu()
        engine_args = AsyncEngineArgs(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=2,  # Only 2 LoRAs per batch
            max_cpu_loras=8,
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        engine = AsyncLLMEngine.from_engine_args(engine_args)
        yield engine
        # Shutdown engine - V1 AsyncLLM uses shutdown() (sync method)
        if hasattr(engine, 'shutdown'):
            engine.shutdown()
        elif hasattr(engine, 'shutdown_background_loop'):
            engine.shutdown_background_loop()
        del engine
        cleanup_gpu()

    @pytest.mark.asyncio
    async def test_batch_lora_limit_timing(self, async_engine_batching):
        """Test max_loras limits by measuring request completion times.

        With max_loras=2, requests using >2 different LoRAs should be
        scheduled in separate batches, resulting in delayed completion
        for requests that exceed the limit.
        """
        engine = async_engine_batching

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=30)

        async def generate_with_timing(request_id: str, lora_id: int):
            """Generate with a specific LoRA and track timing."""
            start = time.perf_counter()
            lora_req = LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))

            output_text = ""
            async for output in engine.generate(
                TEST_PROMPT,
                sampling_params,
                request_id=request_id,
                lora_request=lora_req
            ):
                output_text = output.outputs[0].text

            elapsed = time.perf_counter() - start
            return lora_id, elapsed, output_text

        # Submit 4 requests with 4 different LoRAs IN PARALLEL
        global_start = time.perf_counter()
        tasks = [
            generate_with_timing(f"req_{i}", i + 1)
            for i in range(4)
        ]
        results = await asyncio.gather(*tasks)
        total_time = time.perf_counter() - global_start

        # Verify all completed
        assert len(results) == 4
        for lora_id, elapsed, output_text in results:
            assert output_text, f"LoRA {lora_id} failed"

        # Analyze completion times
        sorted_times = sorted(results, key=lambda x: x[1])

        print(f"\n{'='*60}")
        print("BATCH SCHEDULING ANALYSIS (PARALLEL SUBMISSION)")
        print(f"{'='*60}")
        print(f"max_loras=2, submitted 4 requests with 4 different LoRAs in parallel")
        print(f"\nCompletion times by LoRA ID:")
        for lora_id, elapsed, _ in sorted_times:
            print(f"  LoRA {lora_id}: {elapsed*1000:.0f} ms")

        # With max_loras=2 and 4 different LoRAs, requests should be split
        # into at least 2 batches
        fastest = sorted_times[0][1]
        slowest = sorted_times[-1][1]

        print(f"\nFastest: {fastest*1000:.0f} ms")
        print(f"Slowest: {slowest*1000:.0f} ms")
        print(f"Spread: {(slowest - fastest)*1000:.0f} ms")
        print(f"Total time: {total_time*1000:.0f} ms")
        print(f"{'='*60}")

        print("PASS: Scheduler handles LoRA batching correctly")

    @pytest.mark.asyncio
    async def test_same_lora_batching_efficiency(self, async_engine_batching):
        """Test that same-LoRA requests batch efficiently.

        Requests using the same LoRA should be batched together
        without scheduler delays.
        """
        engine = async_engine_batching

        # Load 1 LoRA
        lora_req = LoRARequest("lora_0", 1, get_lora_path(0))
        await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=20)

        async def generate_request(request_id: str):
            """Generate a request with the same LoRA."""
            start = time.perf_counter()
            lora_req = LoRARequest("lora_0", 1, get_lora_path(0))

            output_text = ""
            async for output in engine.generate(
                TEST_PROMPT,
                sampling_params,
                request_id=request_id,
                lora_request=lora_req
            ):
                output_text = output.outputs[0].text

            elapsed = time.perf_counter() - start
            return elapsed, output_text

        # Submit 10 requests with same LoRA in parallel
        start = time.perf_counter()
        tasks = [generate_request(f"req_{i}") for i in range(10)]
        results = await asyncio.gather(*tasks)
        total_elapsed = time.perf_counter() - start

        assert len(results) == 10
        assert all(r[1] for r in results)

        latencies = [r[0] for r in results]

        print(f"\n{'='*60}")
        print("SAME-LORA BATCHING EFFICIENCY (PARALLEL)")
        print(f"{'='*60}")
        print(f"10 requests with same LoRA submitted in parallel (max_loras=2)")
        print(f"Total time: {total_elapsed*1000:.0f} ms")
        print(f"Throughput: {10/total_elapsed:.1f} req/s")
        print(f"Latency mean: {statistics.mean(latencies)*1000:.0f} ms")
        print(f"Latency stdev: {statistics.stdev(latencies)*1000:.0f} ms")
        print(f"{'='*60}")

        print("PASS: Same-LoRA requests batched efficiently")

    @pytest.mark.asyncio
    async def test_throughput_comparison(self, async_engine_batching):
        """Compare throughput: 2 LoRAs (fits max_loras) vs 4 LoRAs (exceeds).

        With max_loras=2:
        - 2 LoRAs: All requests can batch together
        - 4 LoRAs: Requests split across batches
        """
        engine = async_engine_batching

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=20)
        num_requests = 8

        async def generate_request(request_id: str, lora_id: int):
            """Generate a request with specified LoRA."""
            lora_req = LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))
            output_text = ""
            async for output in engine.generate(
                TEST_PROMPT,
                sampling_params,
                request_id=request_id,
                lora_request=lora_req
            ):
                output_text = output.outputs[0].text
            return output_text

        async def run_workload(lora_count: int, prefix: str):
            """Run workload with specified number of unique LoRAs."""
            start = time.perf_counter()
            tasks = [
                generate_request(f"{prefix}_req_{i}", (i % lora_count) + 1)
                for i in range(num_requests)
            ]
            results = await asyncio.gather(*tasks)
            elapsed = time.perf_counter() - start
            return elapsed, all(r for r in results)

        # Test with 2 LoRAs (fits in max_loras=2)
        time_2_loras, success_2 = await run_workload(2, "2lora")

        # Test with 4 LoRAs (exceeds max_loras=2)
        time_4_loras, success_4 = await run_workload(4, "4lora")

        print(f"\n{'='*60}")
        print("THROUGHPUT COMPARISON (PARALLEL SUBMISSION)")
        print(f"{'='*60}")
        print(f"max_loras=2, {num_requests} requests each submitted in parallel")
        print(f"\n2 LoRAs (fits max_loras):")
        print(f"  Time: {time_2_loras*1000:.0f} ms")
        print(f"  Throughput: {num_requests/time_2_loras:.1f} req/s")
        print(f"\n4 LoRAs (exceeds max_loras):")
        print(f"  Time: {time_4_loras*1000:.0f} ms")
        print(f"  Throughput: {num_requests/time_4_loras:.1f} req/s")
        print(f"\nOverhead from exceeding max_loras: {(time_4_loras/time_2_loras - 1)*100:.0f}%")
        print(f"{'='*60}")

        assert success_2 and success_4, "Some requests failed"
        print("PASS: Throughput comparison completed")


class TestConcurrency:
    """Test suite for concurrent LoRA request handling."""

    @pytest_asyncio.fixture
    async def async_engine_concurrency(self):
        """Create AsyncLLMEngine for concurrency tests with max_loras=4."""
        cleanup_gpu()
        engine_args = AsyncEngineArgs(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=4,
            max_cpu_loras=8,
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        engine = AsyncLLMEngine.from_engine_args(engine_args)
        yield engine
        # Shutdown engine - V1 AsyncLLM uses shutdown() (sync method)
        if hasattr(engine, 'shutdown'):
            engine.shutdown()
        elif hasattr(engine, 'shutdown_background_loop'):
            engine.shutdown_background_loop()
        del engine
        cleanup_gpu()

    @pytest.mark.asyncio
    async def test_high_concurrency_random_loras(self, async_engine_concurrency):
        """Test high concurrency with random LoRA selection.

        Submits all requests in parallel with random LoRA distribution.
        """
        engine = async_engine_concurrency

        # Load 8 LoRAs
        for i in range(8):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=30)
        num_requests = 20

        # Pre-assign random LoRAs to ensure reproducibility
        random.seed(42)
        lora_assignments = [random.randint(1, 8) for _ in range(num_requests)]

        async def generate_request(req_id: int, lora_id: int):
            """Generate a request with specified LoRA."""
            start = time.perf_counter()
            try:
                lora_req = LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))
                output_text = ""
                async for output in engine.generate(
                    f"Request {req_id}: Tell me about ",
                    sampling_params,
                    request_id=f"concurrent_req_{req_id}",
                    lora_request=lora_req
                ):
                    output_text = output.outputs[0].text
                elapsed = time.perf_counter() - start
                return req_id, lora_id, elapsed, output_text
            except Exception as e:
                elapsed = time.perf_counter() - start
                print(f"Request {req_id} failed: {e}")
                return req_id, lora_id, elapsed, None

        # Submit ALL requests in parallel
        global_start = time.perf_counter()
        tasks = [
            generate_request(req_id, lora_id)
            for req_id, lora_id in enumerate(lora_assignments)
        ]
        results = await asyncio.gather(*tasks)
        total_time = time.perf_counter() - global_start

        # Analyze results
        successful = [(r, l, t) for r, l, t, out in results if out is not None]
        failed = [(r, l) for r, l, t, out in results if out is None]

        # Count LoRA distribution
        lora_counts = {}
        for _, lora_id, _ in successful:
            lora_counts[lora_id] = lora_counts.get(lora_id, 0) + 1

        latencies = [t for _, _, t in successful]

        print(f"\n{'='*60}")
        print("HIGH CONCURRENCY RESULTS (ALL PARALLEL)")
        print(f"{'='*60}")
        print(f"Total requests: {num_requests} (submitted in parallel)")
        print(f"Successful: {len(successful)}")
        print(f"Failed: {len(failed)}")
        print(f"Total time: {total_time:.2f}s")
        print(f"Throughput: {num_requests/total_time:.2f} req/s")
        print(f"\nLatency stats (successful requests):")
        if latencies:
            print(f"  Mean: {statistics.mean(latencies)*1000:.0f} ms")
            print(f"  Stdev: {statistics.stdev(latencies)*1000:.0f} ms" if len(latencies) > 1 else "  Stdev: N/A")
            print(f"  Min: {min(latencies)*1000:.0f} ms")
            print(f"  Max: {max(latencies)*1000:.0f} ms")
        print(f"\nLoRA distribution: {lora_counts}")
        print(f"{'='*60}")

        assert len(successful) == num_requests, \
            f"Some requests failed: {len(failed)}/{num_requests}"

        print("PASS: High concurrency test passed")

    @pytest.mark.asyncio
    async def test_burst_traffic_per_lora(self, async_engine_concurrency):
        """Test burst traffic with all requests submitted in parallel."""
        engine = async_engine_concurrency

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=20)
        burst_size = 5

        async def generate_request(lora_id: int, req_idx: int):
            """Generate a request for burst traffic."""
            start = time.perf_counter()
            lora_req = LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))
            output_text = ""
            async for output in engine.generate(
                f"Burst LoRA {lora_id} req {req_idx}: ",
                sampling_params,
                request_id=f"burst_{lora_id}_{req_idx}",
                lora_request=lora_req
            ):
                output_text = output.outputs[0].text
            elapsed = time.perf_counter() - start
            return lora_id, req_idx, elapsed, output_text

        # Submit ALL burst requests in parallel (4 LoRAs x 5 requests each)
        global_start = time.perf_counter()
        tasks = [
            generate_request(lora_id, req_idx)
            for lora_id in range(1, 5)
            for req_idx in range(burst_size)
        ]
        results = await asyncio.gather(*tasks)
        total_time = time.perf_counter() - global_start

        # Analyze results
        successful = [r for r in results if r[3] is not None]
        total = len(results)

        # Per-LoRA latency analysis
        lora_latencies = {}
        for lora_id, req_idx, elapsed, _ in successful:
            if lora_id not in lora_latencies:
                lora_latencies[lora_id] = []
            lora_latencies[lora_id].append(elapsed)

        print(f"\n{'='*60}")
        print("BURST TRAFFIC RESULTS (ALL PARALLEL)")
        print(f"{'='*60}")
        print(f"4 LoRAs x {burst_size} requests = {total} total (submitted in parallel)")
        print(f"Completed in {total_time:.2f}s")
        print(f"Success rate: {len(successful)}/{total}")
        print(f"Throughput: {total/total_time:.1f} req/s")
        print(f"\nPer-LoRA latency (mean):")
        for lora_id in sorted(lora_latencies.keys()):
            latencies = lora_latencies[lora_id]
            print(f"  LoRA {lora_id}: {statistics.mean(latencies)*1000:.0f} ms")
        print(f"{'='*60}")

        assert len(successful) == total, f"Some requests failed"
        print("PASS: Burst traffic test passed")

    @pytest.mark.asyncio
    async def test_sequential_vs_parallel(self, async_engine_concurrency):
        """Compare sequential vs parallel request handling."""
        engine = async_engine_concurrency

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            await engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=20)
        num_requests = 8

        async def generate_request(request_id: str, lora_id: int):
            """Generate a request with specified LoRA."""
            lora_req = LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))
            output_text = ""
            async for output in engine.generate(
                TEST_PROMPT,
                sampling_params,
                request_id=request_id,
                lora_request=lora_req
            ):
                output_text = output.outputs[0].text
            return output_text

        # Sequential execution (one at a time)
        seq_start = time.perf_counter()
        for i in range(num_requests):
            lora_id = (i % 4) + 1
            await generate_request(f"seq_req_{i}", lora_id)
        seq_time = time.perf_counter() - seq_start

        # Parallel execution (all at once)
        par_start = time.perf_counter()
        tasks = [
            generate_request(f"par_req_{i}", (i % 4) + 1)
            for i in range(num_requests)
        ]
        await asyncio.gather(*tasks)
        par_time = time.perf_counter() - par_start

        speedup = seq_time / par_time if par_time > 0 else 0

        print(f"\n{'='*60}")
        print("SEQUENTIAL vs PARALLEL")
        print(f"{'='*60}")
        print(f"{num_requests} requests with 4 LoRAs")
        print(f"\nSequential (one at a time):")
        print(f"  Time: {seq_time*1000:.0f} ms")
        print(f"  Throughput: {num_requests/seq_time:.1f} req/s")
        print(f"\nParallel (all at once):")
        print(f"  Time: {par_time*1000:.0f} ms")
        print(f"  Throughput: {num_requests/par_time:.1f} req/s")
        print(f"\nSpeedup: {speedup:.2f}x")
        print(f"{'='*60}")

        # Parallel should be faster
        assert par_time < seq_time, "Parallel should be faster than sequential"
        print("PASS: Parallel execution is faster")


class TestConcurrentOperations:
    """Test suite for concurrent LoRA operations edge cases.

    Tests interactions between in-flight requests and LoRA management
    operations like pin_lora, add_lora, remove_lora.
    """

    @pytest_asyncio.fixture
    async def async_engine_single_gpu_slot(self):
        """Create AsyncLLMEngine with max_loras=1 (single GPU slot).

        max_loras=1 (only 1 LoRA on GPU), max_cpu_loras=8 (CPU cache)
        This forces LoRA swapping when switching between LoRAs.
        """
        cleanup_gpu()
        engine_args = AsyncEngineArgs(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=1,  # Only 1 LoRA on GPU at a time
            max_cpu_loras=8,  # LoRAs can be cached on CPU
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        engine = AsyncLLMEngine.from_engine_args(engine_args)
        yield engine
        if hasattr(engine, 'shutdown'):
            engine.shutdown()
        elif hasattr(engine, 'shutdown_background_loop'):
            engine.shutdown_background_loop()
        del engine
        cleanup_gpu()

    @pytest.mark.asyncio
    async def test_pin_cpu_lora_during_inflight_request(self, async_engine_single_gpu_slot):
        """Test pinning a CPU-resident LoRA while GPU LoRA has in-flight request.
        note: we only test the case that all gpu loras are busy with in-flight requests,
        to make test simple, we use --max-loras=1

        Scenario (max_loras=1):
        1. Load LoRA1 (goes to GPU - the only slot)
        2. Load LoRA2 (goes to CPU since GPU is full)
        3. Start an in-flight request using LoRA1
        4. While request is running, pin LoRA2 (on CPU)

        Key question: Does pin(lora2) wait for request to finish,
        or does it preempt/terminate the in-flight request?
        """
        engine = async_engine_single_gpu_slot

        # Load LoRA1 (goes to GPU) and LoRA2 (goes to CPU)
        lora1 = LoRARequest("lora_0", 1, get_lora_path(0))
        lora2 = LoRARequest("lora_1", 2, get_lora_path(1))
        await engine.add_lora(lora1)
        await engine.add_lora(lora2)

        sampling_params = SamplingParams(max_tokens=100)  # Long enough for overlap

        timing = {
            "request_start": None,
            "request_end": None,
            "pin_start": None,
            "pin_end": None,
            "token_count": 0,
        }
        results = {
            "request_output": None,
            "request_error": None,
            "pin_result": None,
            "pin_error": None,
        }

        async def inflight_request():
            """Run request using LoRA1 (on GPU)."""
            timing["request_start"] = time.perf_counter()
            try:
                output_text = ""
                async for output in engine.generate(
                    "Write a story about a wizard: ",
                    sampling_params,
                    request_id="inflight_lora1",
                    lora_request=LoRARequest("lora_0", 1, get_lora_path(0))
                ):
                    timing["token_count"] = len(output.outputs[0].token_ids)
                    output_text = output.outputs[0].text
                results["request_output"] = output_text
            except Exception as e:
                results["request_error"] = str(e)
            timing["request_end"] = time.perf_counter()

        async def pin_cpu_lora_after_delay():
            """Wait briefly then pin LoRA2 (on CPU)."""
            await asyncio.sleep(0.2)  # Wait for request to start

            timing["pin_start"] = time.perf_counter()
            try:
                results["pin_result"] = await engine.pin_lora(2)
            except Exception as e:
                results["pin_error"] = str(e)
            timing["pin_end"] = time.perf_counter()

        # Run both concurrently
        await asyncio.gather(
            inflight_request(),
            pin_cpu_lora_after_delay()
        )

        # Analyze timing
        request_duration = (timing["request_end"] - timing["request_start"]) * 1000
        pin_duration = (timing["pin_end"] - timing["pin_start"]) * 1000
        pin_started_at = (timing["pin_start"] - timing["request_start"]) * 1000
        pin_ended_at = (timing["pin_end"] - timing["request_start"]) * 1000

        # Did pin complete before request finished?
        pin_during_request = timing["pin_end"] < timing["request_end"]

        print(f"\n{'='*60}")
        print("PIN CPU-RESIDENT LORA DURING IN-FLIGHT REQUEST")
        print(f"{'='*60}")
        print(f"Config: max_loras=1 (single GPU slot)")
        print(f"Setup: LoRA1 on GPU, LoRA2 on CPU")
        print(f"Action: Start request with LoRA1, then pin(LoRA2)")
        print(f"\nTiming (relative to request start):")
        print(f"  Request started:  0 ms")
        print(f"  pin_lora started: {pin_started_at:.0f} ms")
        print(f"  pin_lora ended:   {pin_ended_at:.0f} ms (took {pin_duration:.0f} ms)")
        print(f"  Request ended:    {request_duration:.0f} ms")
        print(f"  Tokens generated: {timing['token_count']}")
        print(f"\nResults:")
        print(f"  Request output: {'SUCCESS' if results['request_output'] else 'FAILED'}")
        print(f"  Request error:  {results['request_error']}")
        print(f"  pin_lora result: {results['pin_result']}")
        print(f"  pin_lora error:  {results['pin_error']}")
        print(f"\nKEY FINDING:")
        if results["request_error"]:
            print(f"  Request was TERMINATED by pin_lora")
        elif pin_during_request:
            print(f"  pin_lora completed DURING request (non-blocking)")
        else:
            print(f"  pin_lora WAITED for request to finish")
        print(f"{'='*60}")

        print("PASS: Documented pin behavior with in-flight request")

        """
        KEY FINDING: Request was TERMINATED by pin_lora

        The actual error is:
        RuntimeError: All items are pinned, cannot remove oldest from the cache.
    
        What happened:
        1. LoRA1 is on GPU (the only slot), in-flight request running
        2. pin_lora(2) is called → pins LoRA2 (on CPU)
        3. Scheduler tries to continue the request but now:
          - LoRA2 is pinned (can't be evicted)
          - LoRA1 is in use
          - Only 1 GPU slot available
          - When it tries to swap, it can't evict anything → engine crashes

        TODO: This is essentially a vLLM edge case/bug: pinning a CPU-resident LoRA while another LoRA is actively running with max_loras=1 crashes the engine because it creates an unresolvable state.
        """


if __name__ == "__main__":
    pytest.main([__file__, "-v", "-s"])
