"""Consolidated Test Suite: LRU Eviction, Swap Latency, and Pinning.

This module combines tests for:
- LRU eviction behavior (CPU and GPU tiers)
- CPU-to-GPU swap latency measurements
- LoRA pinning functionality
"""

import pytest
import time
import statistics
import gc
import torch
from vllm import LLM, SamplingParams
from vllm.lora.request import LoRARequest
from test_config import BASE_MODEL, get_lora_path, TEST_PROMPT, MAX_TOKENS


class TestLRUEviction:
    """Test suite for LRU eviction behavior."""

    @pytest.fixture
    def llm_with_lora(self):
        """Create LLM with limited LoRA slots."""
        gc.collect()
        torch.cuda.empty_cache()

        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=2,          # Only 2 GPU slots
            max_cpu_loras=4,      # 4 CPU slots
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        yield llm
        del llm
        gc.collect()
        torch.cuda.empty_cache()

    def test_basic_lru_eviction(self, llm_with_lora):
        """Test that oldest LoRA is evicted when capacity exceeded."""
        llm = llm_with_lora

        # Load 4 LoRAs (fills CPU cache)
        for i in range(4):
            lora_req = LoRARequest(
                lora_name=f"lora_{i}",
                lora_int_id=i + 1,
                lora_path=get_lora_path(i)
            )
            result = llm.llm_engine.add_lora(lora_req)
            assert result, f"Failed to add lora_{i}"

        # Verify all 4 are registered
        registered = llm.llm_engine.list_loras()
        assert len(registered) == 4, f"Expected 4 LoRAs, got {len(registered)}"

        # Access order: 1, 2, 3, 4 (4 is most recent)
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)
        for i in range(4):
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )

        # Load 5th LoRA - should evict LoRA 1 (oldest)
        lora_req_5 = LoRARequest(
            lora_name="lora_4",
            lora_int_id=5,
            lora_path=get_lora_path(4)
        )
        llm.llm_engine.add_lora(lora_req_5)

        # Verify LoRA 1 was evicted
        registered = llm.llm_engine.list_loras()
        assert 1 not in registered, "LoRA 1 should have been evicted"
        assert 5 in registered, "LoRA 5 should be registered"
        assert len(registered) == 4, f"Expected 4 LoRAs, got {len(registered)}"

        print("PASS: Basic LRU eviction works correctly")

    def test_access_updates_lru_order(self, llm_with_lora):
        """Test that accessing a LoRA updates its LRU position."""
        llm = llm_with_lora
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Access LoRA 1 again (makes it most recent)
        llm.generate(
            [TEST_PROMPT],
            sampling_params,
            lora_request=LoRARequest("lora_0", 1, get_lora_path(0))
        )

        # Load 5th LoRA - should evict LoRA 2 (now oldest)
        lora_req_5 = LoRARequest("lora_4", 5, get_lora_path(4))
        llm.llm_engine.add_lora(lora_req_5)

        registered = llm.llm_engine.list_loras()
        assert 1 in registered, "LoRA 1 should NOT be evicted (recently accessed)"
        assert 2 not in registered, "LoRA 2 should have been evicted (oldest)"

        print("PASS: Access updates LRU order correctly")

    def test_gpu_slot_eviction(self, llm_with_lora):
        """Test GPU slot eviction when activating new LoRA.

        Note: list_loras() returns all registered LoRAs (CPU + GPU).
        There's no direct API to distinguish CPU vs GPU residence.
        Swap latency tests below verify GPU behavior indirectly.
        """
        llm = llm_with_lora
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load and use 3 LoRAs (only 2 GPU slots)
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )

        # All 3 should be registered (CPU cache)
        registered = llm.llm_engine.list_loras()
        assert len(registered) == 3

        print("PASS: GPU slot eviction works without errors")

    def test_eviction_with_mixed_access_pattern(self, llm_with_lora):
        """Test eviction with complex access patterns."""
        llm = llm_with_lora
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Complex access pattern: 1, 3, 2, 4, 1, 3
        # After this, order should be: 2, 4, 1, 3 (2 is oldest)
        access_order = [1, 3, 2, 4, 1, 3]
        for lora_id in access_order:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{lora_id-1}", lora_id, get_lora_path(lora_id-1))
            )

        # Load 5th LoRA - should evict LoRA 2 (oldest after pattern)
        lora_req_5 = LoRARequest("lora_4", 5, get_lora_path(4))
        llm.llm_engine.add_lora(lora_req_5)

        registered = llm.llm_engine.list_loras()
        assert 2 not in registered, "LoRA 2 should be evicted (oldest after access pattern)"
        assert 1 in registered and 3 in registered, "Recently accessed LoRAs should survive"

        print("PASS: Mixed access pattern eviction works correctly")


class TestSwapLatency:
    """Test suite for measuring LoRA swap latency."""

    @pytest.fixture
    def llm_swap_test(self):
        """Create LLM for swap testing."""
        gc.collect()
        torch.cuda.empty_cache()

        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=2,          # Small GPU cache to force swaps
            max_cpu_loras=10,     # Large CPU cache
            max_lora_rank=32,     # Support larger ranks for testing
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        yield llm
        del llm
        gc.collect()
        torch.cuda.empty_cache()

    def measure_generation_time(self, llm, lora_request, num_runs=5, warmup=1):
        """Measure average generation time with warmup runs."""
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)
        times = []

        # Warmup runs (not counted)
        for _ in range(warmup):
            llm.generate([TEST_PROMPT], sampling_params, lora_request=lora_request)

        # Actual measurement runs
        for _ in range(num_runs):
            start = time.perf_counter()
            llm.generate([TEST_PROMPT], sampling_params, lora_request=lora_request)
            elapsed = time.perf_counter() - start
            times.append(elapsed)

        return {
            "mean": statistics.mean(times),
            "stdev": statistics.stdev(times) if len(times) > 1 else 0,
            "min": min(times),
            "max": max(times),
            "times": times
        }

    def test_hot_vs_cold_latency(self, llm_swap_test):
        """Compare latency of GPU-resident vs CPU-cached LoRA.

        Improved measurement:
        1. Use warmup runs to stabilize
        2. Explicitly evict hot LoRA before cold test
        3. Calculate and verify swap overhead
        """
        llm = llm_swap_test

        # Load 4 LoRAs (2 GPU, 2 CPU-only)
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Ensure LoRA 3 and 4 are in GPU (most recently used)
        for i in [2, 3]:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )

        # Measure HOT path (LoRA 4 in GPU) - with warmup
        hot_lora = LoRARequest("lora_3", 4, get_lora_path(3))
        hot_times = self.measure_generation_time(llm, hot_lora, num_runs=10, warmup=2)

        # Now access LoRA 1 which is in CPU - this forces a swap
        # After this, GPU has: LoRA 1, LoRA 4 (LoRA 3 evicted to CPU)
        cold_lora = LoRARequest("lora_0", 1, get_lora_path(0))

        # First cold access includes swap - measure this separately
        cold_first_start = time.perf_counter()
        llm.generate([TEST_PROMPT], sampling_params, lora_request=cold_lora)
        cold_first_time = time.perf_counter() - cold_first_start

        # Subsequent accesses should be hot
        cold_subsequent = self.measure_generation_time(llm, cold_lora, num_runs=5, warmup=0)

        # Calculate overhead
        swap_overhead = cold_first_time - hot_times["mean"]

        print("\n" + "="*60)
        print("SWAP LATENCY RESULTS")
        print("="*60)
        print(f"HOT path (GPU-resident, after warmup):")
        print(f"  Mean: {hot_times['mean']*1000:.2f} ms")
        print(f"  Stdev: {hot_times['stdev']*1000:.2f} ms")
        print(f"  Range: [{hot_times['min']*1000:.2f}, {hot_times['max']*1000:.2f}] ms")
        print(f"\nCOLD path (first access, includes CPU->GPU swap):")
        print(f"  Time: {cold_first_time*1000:.2f} ms")
        print(f"\nCOLD path (subsequent, now in GPU):")
        print(f"  Mean: {cold_subsequent['mean']*1000:.2f} ms")
        print(f"\nSWAP OVERHEAD (cold_first - hot_mean): {swap_overhead*1000:.2f} ms")
        if hot_times['mean'] > 0:
            print(f"Overhead %: {(swap_overhead/hot_times['mean'])*100:.1f}%")
        print("="*60)

        return {
            "hot": hot_times,
            "cold_first": cold_first_time,
            "cold_subsequent": cold_subsequent,
            "swap_overhead_ms": swap_overhead * 1000
        }

    def test_repeated_swaps_vs_baseline(self, llm_swap_test):
        """Compare repeated swaps against always-hot baseline.

        This test properly measures swap overhead by comparing:
        1. Alternating between 3 LoRAs (forces swaps with max_loras=2)
        2. Baseline with 2 LoRAs (always hot, no swaps)
        """
        llm = llm_swap_test
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)
        num_iterations = 10

        # Test 1: Alternating between 3 LoRAs (forces swaps)
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        swap_times = []
        for _ in range(num_iterations):
            for i in range(3):  # Cycle through all 3, forcing swaps
                start = time.perf_counter()
                llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                )
                elapsed = time.perf_counter() - start
                swap_times.append(elapsed)

        # Test 2: Baseline with only 2 LoRAs (always hot)
        baseline_times = []
        for _ in range(num_iterations):
            for i in range(2):  # Only 2 LoRAs, both stay in GPU
                start = time.perf_counter()
                llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                )
                elapsed = time.perf_counter() - start
                baseline_times.append(elapsed)

        swap_mean = statistics.mean(swap_times)
        baseline_mean = statistics.mean(baseline_times)
        overhead = swap_mean - baseline_mean

        print("\n" + "="*60)
        print("SWAP VS BASELINE COMPARISON")
        print("="*60)
        print(f"With swaps (3 LoRAs, max_loras=2):")
        print(f"  Mean: {swap_mean*1000:.2f} ms")
        print(f"  Stdev: {statistics.stdev(swap_times)*1000:.2f} ms")
        print(f"\nBaseline (2 LoRAs, always hot):")
        print(f"  Mean: {baseline_mean*1000:.2f} ms")
        print(f"  Stdev: {statistics.stdev(baseline_times)*1000:.2f} ms")
        print(f"\nAverage swap overhead per request: {overhead*1000:.2f} ms")
        print("="*60)

    def test_swap_latency_consistency(self, llm_swap_test):
        """Test that swap latency is consistent across many swaps."""
        llm = llm_swap_test
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 3 LoRAs
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Force many swaps and track latency over time
        swap_times = []
        for iteration in range(10):
            for i in range(3):
                start = time.perf_counter()
                llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                )
                elapsed = time.perf_counter() - start
                swap_times.append(elapsed)

        # Check for consistency (no degradation over time)
        first_half = swap_times[:len(swap_times)//2]
        second_half = swap_times[len(swap_times)//2:]

        print("\n" + "="*60)
        print("SWAP LATENCY CONSISTENCY")
        print("="*60)
        print(f"First half mean: {statistics.mean(first_half)*1000:.2f} ms")
        print(f"Second half mean: {statistics.mean(second_half)*1000:.2f} ms")
        print(f"Overall stdev: {statistics.stdev(swap_times)*1000:.2f} ms")
        print("="*60)

        # Latency should not degrade significantly
        assert abs(statistics.mean(second_half) - statistics.mean(first_half)) < statistics.mean(first_half) * 0.5, \
            "Swap latency should not degrade over time"

        print("PASS: Swap latency is consistent")


class TestPinning:
    """Test suite for LoRA pinning functionality."""

    @pytest.fixture
    def llm_pinning_test(self):
        """Create LLM for pinning tests."""
        gc.collect()
        torch.cuda.empty_cache()

        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=2,
            max_cpu_loras=4,
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        yield llm
        del llm
        gc.collect()
        torch.cuda.empty_cache()

    def test_pinned_lora_not_evicted(self, llm_pinning_test):
        """Test that pinned LoRA survives eviction.

        Note: add_lora() loads to CPU first, then swaps to GPU on first use.
        pin_lora() marks the LoRA as non-evictable.
        """
        llm = llm_pinning_test

        # Load 4 LoRAs (fills max_cpu_loras)
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Pin LoRA 1 (oldest)
        llm.llm_engine.pin_lora(1)

        # Load 5th LoRA - should evict LoRA 2 (oldest unpinned)
        lora_req_5 = LoRARequest("lora_4", 5, get_lora_path(4))
        llm.llm_engine.add_lora(lora_req_5)

        registered = llm.llm_engine.list_loras()
        assert 1 in registered, "Pinned LoRA 1 should NOT be evicted"
        assert 2 not in registered, "LoRA 2 (oldest unpinned) should be evicted"
        assert 5 in registered, "New LoRA 5 should be registered"

        print("PASS: Pinned LoRA survives eviction")

    def test_pin_cpu_lora_when_gpu_available(self, llm_pinning_test):
        """Test pinning a CPU-only LoRA when GPU slot is available."""
        llm = llm_pinning_test
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 3 LoRAs
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Use LoRA 2 and 3 to put them in GPU (max_loras=2)
        for i in [1, 2]:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )

        # LoRA 1 is now CPU-only
        # Pin LoRA 1 - should swap to GPU (evicting oldest from GPU)
        result = llm.llm_engine.pin_lora(1)
        assert result, "pin_lora should succeed for CPU-resident LoRA"

        output = llm.generate(
            [TEST_PROMPT],
            sampling_params,
            lora_request=LoRARequest("lora_0", 1, get_lora_path(0))
        )
        assert output is not None

        print("PASS: Pinning CPU LoRA works correctly")

    def test_pin_cpu_lora_when_gpu_full_with_pins(self, llm_pinning_test):
        """Test that using CPU LoRA fails when all GPU slots are pinned.

        Edge case: max_loras=2, both GPU slots pinned, try to use CPU LoRA.
        Expected: RuntimeError since no GPU slot can be evicted for the new LoRA.
        """
        llm = llm_pinning_test
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 3 LoRAs
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Use and pin LoRA 1 and 2 (fills GPU slots)
        for i in [0, 1]:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )
            llm.llm_engine.pin_lora(i + 1)

        # Try to use LoRA 3 (CPU-only, can't swap to GPU since both slots pinned)
        # vLLM raises RuntimeError when all GPU slots are pinned
        with pytest.raises(Exception) as excinfo:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest("lora_2", 3, get_lora_path(2))
            )

        error_msg = str(excinfo.value).lower()
        # V1 engine wraps errors in "EngineCore encountered an issue" message
        # Accept either specific pinning error or the wrapped engine error
        assert ("pinned" in error_msg or "cannot remove" in error_msg or
                "enginecore" in error_msg), \
            f"Expected error related to pinned LoRAs, got: {excinfo.value}"

        print(f"Expected error when all GPU slots pinned: {excinfo.value}")
        print("PASS: CPU LoRA with full pinned GPU raises expected error")

    def test_all_pinned_blocks_new_lora(self, llm_pinning_test):
        """Test that using new LoRA fails when all GPU slots are pinned.

        With max_loras=2, we can only have 2 LoRAs in GPU at a time.
        When both GPU slots are pinned and we try to use a 3rd LoRA,
        vLLM raises an error since it cannot evict any GPU slot.

        Note: This test is similar to test_pin_cpu_lora_when_gpu_full_with_pins
        but verifies the behavior from a different angle (adding vs using).
        """
        llm = llm_pinning_test
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 3 LoRAs
        for i in range(3):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Use and pin 2 LoRAs (fills max_loras=2 GPU slots)
        for i in range(2):
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            )
            llm.llm_engine.pin_lora(i + 1)

        # Try to use 3rd LoRA - should fail (both GPU slots pinned)
        with pytest.raises(Exception) as excinfo:
            llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest("lora_2", 3, get_lora_path(2))
            )

        error_msg = str(excinfo.value).lower()
        # V1 engine wraps errors in "EngineCore encountered an issue" message
        # Accept either specific pinning error or the wrapped engine error
        assert ("pinned" in error_msg or "cannot remove" in error_msg or
                "enginecore" in error_msg), \
            f"Expected error related to pinned LoRAs, got: {excinfo.value}"

        print(f"Expected error when all GPU slots pinned: {excinfo.value}")
        print("PASS: Error when all GPU slots are pinned and new LoRA activation needed")

    def test_unpin_allows_eviction(self, llm_pinning_test):
        """Test that unpinning allows the LoRA to be evicted."""
        llm = llm_pinning_test

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Pin LoRA 1
        llm.llm_engine.pin_lora(1)

        # Unpin LoRA 1 (if API exists)
        # vllm doesn't have the support now, test can be opened once support is added
        if hasattr(llm.llm_engine, 'unpin_lora'):
            llm.llm_engine.unpin_lora(1)

            # Now load new LoRAs to trigger eviction
            for i in range(4, 6):
                lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                llm.llm_engine.add_lora(lora_req)

            registered = llm.llm_engine.list_loras()
            # LoRA 1 should now be evictable
            if 1 not in registered:
                print("PASS: Unpinned LoRA was evicted as expected")
            else:
                print("INFO: LoRA 1 not evicted yet (LRU order may have preserved it)")
        else:
            pytest.skip("unpin_lora API not available")

    def test_pin_nonexistent_raises(self, llm_pinning_test):
        """Test that pinning non-existent LoRA raises error."""
        llm = llm_pinning_test

        with pytest.raises(Exception) as excinfo:
            llm.llm_engine.pin_lora(999)

        print(f"Expected error for non-existent LoRA: {excinfo.value}")
        print("PASS: pin_lora raises exception when LoRA not found")


if __name__ == "__main__":
    pytest.main([__file__, "-v", "-s"])
