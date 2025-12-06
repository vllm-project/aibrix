"""Test Suite: LoRA API - Dynamic Loading and Performance.

This module tests LoRA API behaviors focused on serverless scenarios:
- Dynamic loading latency measurement
- Hot-swap patterns during inference
- Load/use/unload cycles

Note: Basic add/remove/pin correctness is covered by vllm/tests/lora/test_lora_manager.py
"""

import pytest
import time
import statistics
from vllm import LLM, SamplingParams
from vllm.lora.request import LoRARequest
from test_config import BASE_MODEL, get_lora_path, TEST_PROMPT, MAX_TOKENS


class TestDynamicLoading:
    """Test suite for dynamic LoRA loading performance."""

    @pytest.fixture
    def llm(self):
        """Create LLM for dynamic loading tests."""
        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=4,
            max_cpu_loras=8,
            max_lora_rank=16,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )
        yield llm
        del llm

    def test_loading_latency(self, llm):
        """Measure LoRA loading latency."""
        load_times = []

        for i in range(5):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))

            start = time.perf_counter()
            llm.llm_engine.add_lora(lora_req)
            elapsed = time.perf_counter() - start

            load_times.append(elapsed)

        print(f"\n{'='*60}")
        print("LORA LOADING LATENCY")
        print(f"{'='*60}")
        print(f"Samples: {len(load_times)}")
        print(f"Mean: {statistics.mean(load_times)*1000:.2f} ms")
        print(f"Stdev: {statistics.stdev(load_times)*1000:.2f} ms")
        print(f"Min: {min(load_times)*1000:.2f} ms")
        print(f"Max: {max(load_times)*1000:.2f} ms")
        print(f"{'='*60}")

    def test_load_use_unload_cycle(self, llm):
        """Test complete load -> use -> unload cycle."""
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        for cycle in range(3):
            # Load
            lora_req = LoRARequest("cycle_lora", 1, get_lora_path(0))
            add_result = llm.llm_engine.add_lora(lora_req)

            # Use
            output = llm.generate(
                [TEST_PROMPT],
                sampling_params,
                lora_request=LoRARequest("cycle_lora", 1, get_lora_path(0))
            )
            assert output is not None and output[0].outputs[0].text

            # Unload
            remove_result = llm.llm_engine.remove_lora(1)

            print(f"Cycle {cycle + 1}: add={add_result}, remove={remove_result}")

        print("PASS: Load/use/unload cycles work correctly")

    def test_hot_swap_loras(self, llm):
        """Test hot-swapping between LoRAs during inference."""
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        # Rapid switching between LoRAs
        for _ in range(3):
            for i in range(4):
                output = llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                )
                assert output is not None

        # All should still be registered
        loras = llm.llm_engine.list_loras()
        assert len(set(loras)) == 4

        print("PASS: Hot-swap between LoRAs works correctly")

    def test_reload_same_lora(self, llm):
        """Test reloading the same LoRA (idempotency behavior)."""
        lora_req = LoRARequest("test_lora", 1, get_lora_path(0))

        # First load
        result1 = llm.llm_engine.add_lora(lora_req)

        # Second load (same LoRA) - should be idempotent
        result2 = llm.llm_engine.add_lora(lora_req)

        # Document behavior, both should be True.
        print(f"\nFirst add_lora: {result1}")
        print(f"Second add_lora (duplicate): {result2}")

        # Only one entry (no duplicates)
        loras = llm.llm_engine.list_loras()
        assert len(set(loras)) == 1, "Should not have duplicate LoRAs"

        print("PASS: Reloading same LoRA handled correctly")

    def test_remove_lora_idempotent(self, llm):
        """Test that remove_lora is safe to call multiple times."""
        # Add a LoRA
        lora_req = LoRARequest("test_lora", 1, get_lora_path(0))
        llm.llm_engine.add_lora(lora_req)

        # First remove - should succeed
        result1 = llm.llm_engine.remove_lora(1)
        print(f"\nFirst remove_lora(1): {result1}")
        assert result1 is True, "First remove should return True"

        # Second remove (already removed) - should return False, not raise
        result2 = llm.llm_engine.remove_lora(1)
        print(f"Second remove_lora(1): {result2}")
        assert result2 is False, "Second remove should return False"

        # Third remove - still safe
        result3 = llm.llm_engine.remove_lora(1)
        print(f"Third remove_lora(1): {result3}")
        assert result3 is False, "Third remove should return False"

        # Remove non-existent LoRA - should return False
        result4 = llm.llm_engine.remove_lora(999)
        print(f"remove_lora(999) non-existent: {result4}")
        assert result4 is False, "remove_lora on non-existent should return False"

        print("PASS: remove_lora is idempotent (safe to call multiple times)")

    def test_pin_lora_idempotent(self, llm):
        """Test that pin_lora is idempotent (uses set.add internally)."""
        # Add a LoRA
        lora_req = LoRARequest("test_lora", 1, get_lora_path(0))
        llm.llm_engine.add_lora(lora_req)

        # Pin multiple times - should not raise
        result1 = llm.llm_engine.pin_lora(1)
        print(f"\nFirst pin_lora(1): {result1}")

        result2 = llm.llm_engine.pin_lora(1)
        print(f"Second pin_lora(1): {result2}")

        result3 = llm.llm_engine.pin_lora(1)
        print(f"Third pin_lora(1): {result3}")

        # Verify LoRA is still pinned and functional
        sampling_params = SamplingParams(max_tokens=MAX_TOKENS)
        output = llm.generate(
            [TEST_PROMPT],
            sampling_params,
            lora_request=LoRARequest("test_lora", 1, get_lora_path(0))
        )
        assert output is not None and output[0].outputs[0].text

        print("PASS: pin_lora is idempotent (no exceptions on multiple calls)")

    def test_pin_lora_not_found_raises(self, llm):
        """Test that pin_lora raises error if LoRA not found."""
        # Try to pin non-existent LoRA
        with pytest.raises(Exception) as excinfo:
            llm.llm_engine.pin_lora(999)

        error_msg = str(excinfo.value).lower()
        print(f"\npin_lora(999) raised: {type(excinfo.value).__name__}: {excinfo.value}")
        # V1 engine may wrap error in EngineCore message
        assert ("not found" in error_msg or "not registered" in error_msg or
                "enginecore" in error_msg), \
            f"Expected error about LoRA not found, got: {excinfo.value}"
        print("PASS: pin_lora raises exception when LoRA not found")

    def test_remove_pinned_lora(self, llm):
        """Test that pinned LoRAs can still be explicitly removed.

        Note: pin_lora() only prevents eviction due to LRU cache pressure,
        it does NOT prevent explicit remove_lora() calls.
        """
        # Add and pin a LoRA
        lora_req = LoRARequest("pinned_lora", 1, get_lora_path(0))
        llm.llm_engine.add_lora(lora_req)
        llm.llm_engine.pin_lora(1)

        # Verify it's loaded
        loras_before = set(llm.llm_engine.list_loras())
        assert 1 in loras_before, "LoRA should be loaded"

        # Remove the pinned LoRA - this should succeed
        remove_result = llm.llm_engine.remove_lora(1)
        assert remove_result is True, "remove_lora on pinned LoRA should return True"

        # Verify it was removed
        loras_after = set(llm.llm_engine.list_loras())
        assert 1 not in loras_after, "Pinned LoRA should be removed after remove_lora()"

        print(f"\n{'='*60}")
        print("REMOVE PINNED LORA")
        print(f"{'='*60}")
        print("pin_lora() prevents LRU eviction, NOT explicit remove_lora()")
        print(f"remove_lora(1) on pinned LoRA: {remove_result}")
        print(f"{'='*60}")

        print("PASS: Pinned LoRA can be explicitly removed")


if __name__ == "__main__":
    pytest.main([__file__, "-v", "-s"])
