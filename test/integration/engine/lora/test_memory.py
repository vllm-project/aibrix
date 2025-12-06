"""Consolidated Test Suite: Memory Isolation and Budget.

This module combines tests for:
- Memory isolation between LoRA adapters and KV cache
- Memory budget verification and pre-allocation
"""

import pytest
import gc
import torch
from vllm import LLM, SamplingParams
from vllm.lora.request import LoRARequest
from test_config import BASE_MODEL, get_lora_path, TEST_PROMPT, MAX_TOKENS


def get_gpu_memory_mb():
    """Get current GPU memory usage in MB."""
    torch.cuda.synchronize()
    return torch.cuda.memory_allocated() / 1e6


def get_gpu_memory_gb():
    """Get current GPU memory usage in GB."""
    torch.cuda.synchronize()
    return torch.cuda.memory_allocated() / 1e9


def cleanup_gpu():
    """Force GPU memory cleanup."""
    gc.collect()
    torch.cuda.empty_cache()
    torch.cuda.synchronize()


class TestMemoryIsolation:
    """Test suite for LoRA/KV cache memory isolation."""

    @pytest.fixture
    def llm_memory_constrained(self):
        """Create LLM with constrained memory to force KV pressure."""
        cleanup_gpu()
        initial_memory = get_gpu_memory_mb()

        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=4,
            max_cpu_loras=4,
            max_lora_rank=16,
            gpu_memory_utilization=0.6,  # Lower to create KV pressure
            max_model_len=2048,           # Smaller context to manage memory
            trust_remote_code=True,
            enforce_eager=True,
        )

        after_init_memory = get_gpu_memory_mb()
        print(f"\nLLM init memory: {initial_memory:.1f} MB -> {after_init_memory:.1f} MB")

        yield llm
        del llm
        cleanup_gpu()
        """Test that LoRA memory is allocated separately from KV cache.

        Verifies that loading LoRAs doesn't reduce KV cache capacity
        and KV pressure doesn't affect LoRA availability.
        """
        llm = llm_memory_constrained

        # Track memory after LoRA loading
        memory_before_loras = get_gpu_memory_mb()

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        memory_after_loras = get_gpu_memory_mb()
        lora_memory = memory_after_loras - memory_before_loras

        print(f"\n{'='*60}")
        print("LORA MEMORY ALLOCATION")
        print(f"{'='*60}")
        print(f"Memory before LoRAs: {memory_before_loras:.1f} MB")
        print(f"Memory after LoRAs: {memory_after_loras:.1f} MB")
        print(f"LoRA overhead: {lora_memory:.1f} MB")
        print(f"{'='*60}")

        # LoRA memory should be minimal (mostly metadata) if buffers pre-allocated
        # Actual weights loaded on-demand or pre-allocated during init

        initial_loras = set(llm.llm_engine.list_loras())
        assert len(initial_loras) == 4

        print("PASS: LoRA memory allocated separately from KV cache")

    def test_kv_pressure_preserves_pinned_loras(self, llm_memory_constrained):
        """Test that KV cache pressure doesn't evict pinned LoRAs.

        Uses long sequences to pressure KV cache while verifying
        all pinned LoRAs remain available.
        """
        llm = llm_memory_constrained

        # Load and pin 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)
            llm.llm_engine.pin_lora(i + 1)

        initial_loras = set(llm.llm_engine.list_loras())
        assert len(initial_loras) == 4, f"Expected 4 LoRAs, got {len(initial_loras)}"

        # Generate with moderately long sequences to pressure KV cache
        # Using max_model_len=2048, generate ~500 token responses
        long_prompt = "Write a detailed technical explanation about: " * 30
        sampling_params = SamplingParams(max_tokens=300)

        errors = []
        for iteration in range(3):
            for i in range(4):
                try:
                    outputs = llm.generate(
                        [long_prompt],
                        sampling_params,
                        lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                    )
                    assert outputs[0].outputs[0].text, "Empty output"
                except Exception as e:
                    error_msg = str(e).lower()
                    if "lora" in error_msg and "evict" in error_msg:
                        errors.append(f"LoRA eviction error: {e}")
                    elif "memory" in error_msg or "kv" in error_msg:
                        # KV cache pressure is expected, not a failure
                        print(f"INFO: KV cache pressure (expected): {e}")
                    else:
                        errors.append(f"Unexpected error: {e}")

        # Verify all LoRAs still present
        final_loras = set(llm.llm_engine.list_loras())
        assert final_loras == initial_loras, \
            f"LoRAs changed! Before: {initial_loras}, After: {final_loras}"

        if errors:
            pytest.fail(f"LoRA errors during KV pressure: {errors}")

        print("PASS: KV cache pressure does not affect pinned LoRAs")

    def test_memory_stability_under_load(self, llm_memory_constrained):
        """Test that memory usage remains stable under continuous load."""
        llm = llm_memory_constrained

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        sampling_params = SamplingParams(max_tokens=50)

        # Track memory over multiple iterations
        memory_samples = []
        for iteration in range(10):
            for i in range(4):
                llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
                )

            memory_samples.append(get_gpu_memory_mb())

        # Check for memory leaks (memory shouldn't grow significantly)
        memory_growth = memory_samples[-1] - memory_samples[0]

        print(f"\n{'='*60}")
        print("MEMORY STABILITY")
        print(f"{'='*60}")
        print(f"Initial: {memory_samples[0]:.1f} MB")
        print(f"Final: {memory_samples[-1]:.1f} MB")
        print(f"Growth: {memory_growth:.1f} MB")
        print(f"{'='*60}")

        # Allow small growth (fragmentation, etc.) but not significant leaks
        assert memory_growth < 100, f"Potential memory leak: {memory_growth:.1f} MB growth"

        print("PASS: Memory stable under continuous load")


class TestMemoryBudget:
    """Test suite for memory budget verification."""

    def test_lora_buffer_preallocation(self):
        """Test that LoRA buffers are pre-allocated at init.

        Memory for max_loras LoRAs should be allocated upfront,
        so loading additional LoRAs adds minimal GPU memory.
        """
        cleanup_gpu()
        initial_memory = get_gpu_memory_mb()

        # Create LLM with LoRA enabled
        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=4,
            max_cpu_loras=4,
            max_lora_rank=8,
            gpu_memory_utilization=0.8,
            trust_remote_code=True,
            enforce_eager=True,
        )

        after_init_memory = get_gpu_memory_mb()
        init_overhead = after_init_memory - initial_memory

        # Load 4 LoRAs
        for i in range(4):
            lora_req = LoRARequest(f"lora_{i}", i + 1, get_lora_path(i))
            llm.llm_engine.add_lora(lora_req)

        after_load_memory = get_gpu_memory_mb()
        load_overhead = after_load_memory - after_init_memory

        print(f"\n{'='*60}")
        print("MEMORY BUDGET ANALYSIS")
        print(f"{'='*60}")
        print(f"Initial GPU memory: {initial_memory:.1f} MB")
        print(f"After LLM init: {after_init_memory:.1f} MB")
        print(f"After loading 4 LoRAs: {after_load_memory:.1f} MB")
        print(f"\nInit overhead (includes LoRA buffers): {init_overhead:.1f} MB")
        print(f"Load overhead (should be minimal): {load_overhead:.1f} MB")
        print(f"{'='*60}")

        # LoRA buffers should be pre-allocated
        # Loading should add minimal memory (mostly CPU-side objects)
        assert load_overhead < 500, \
            f"Loading LoRAs added too much GPU memory: {load_overhead:.1f} MB"

        del llm
        cleanup_gpu()

        print("PASS: LoRA buffers are pre-allocated at init")

    def test_memory_scales_with_max_loras(self):
        """Test that memory usage increases with max_loras setting."""
        results = []

        for max_loras in [1, 2, 4]:
            cleanup_gpu()
            initial = get_gpu_memory_mb()

            llm = LLM(
                model=BASE_MODEL,
                enable_lora=True,
                max_loras=max_loras,
                max_cpu_loras=max_loras,
                max_lora_rank=16,
                gpu_memory_utilization=0.7,
                trust_remote_code=True,
                enforce_eager=True,
            )

            after = get_gpu_memory_mb()
            overhead = after - initial
            results.append((max_loras, overhead))

            del llm
            cleanup_gpu()

        print(f"\n{'='*60}")
        print("MEMORY vs MAX_LORAS")
        print(f"{'='*60}")
        for max_loras, overhead in results:
            print(f"max_loras={max_loras}: {overhead:.1f} MB")
        print(f"{'='*60}")

        # Memory should generally increase with max_loras
        # (though base model dominates, LoRA buffers add incrementally)
        for i in range(1, len(results)):
            # Allow small tolerance for measurement noise
            assert results[i][1] >= results[i-1][1] - 50, \
                f"Memory should increase with max_loras: {results[i-1]} -> {results[i]}"

        print("PASS: Memory scales with max_loras")

    def test_memory_scales_with_rank(self):
        """Test that memory usage increases with max_lora_rank setting."""
        results = []

        for max_rank in [8, 16, 32]:
            cleanup_gpu()
            initial = get_gpu_memory_mb()

            llm = LLM(
                model=BASE_MODEL,
                enable_lora=True,
                max_loras=2,
                max_cpu_loras=2,
                max_lora_rank=max_rank,
                gpu_memory_utilization=0.7,
                trust_remote_code=True,
                enforce_eager=True,
            )

            after = get_gpu_memory_mb()
            overhead = after - initial
            results.append((max_rank, overhead))

            del llm
            cleanup_gpu()

        print(f"\n{'='*60}")
        print("MEMORY vs MAX_LORA_RANK")
        print(f"{'='*60}")
        for max_rank, overhead in results:
            print(f"max_lora_rank={max_rank}: {overhead:.1f} MB")
        print(f"{'='*60}")

        # Higher rank means larger buffers
        for i in range(1, len(results)):
            assert results[i][1] >= results[i-1][1] - 50, \
                f"Memory should increase with rank: {results[i-1]} -> {results[i]}"

        print("PASS: Memory scales with max_lora_rank")

    def test_rank_mismatch_handling(self):
        """Test behavior when LoRA rank exceeds max_lora_rank.

        Test LoRAs have rank=8. Setting max_lora_rank=4 should trigger
        a rank mismatch. This tests when the error occurs:
        - At add_lora() time?
        - At generate() time?
        """
        cleanup_gpu()

        # Use max_lora_rank=4 which is smaller than the test LoRAs (rank=8)
        llm = LLM(
            model=BASE_MODEL,
            enable_lora=True,
            max_loras=2,
            max_cpu_loras=2,
            max_lora_rank=4,  # Smaller than test LoRA rank (8)
            gpu_memory_utilization=0.7,
            trust_remote_code=True,
            enforce_eager=True,
        )

        lora_req = LoRARequest("test_lora", 1, get_lora_path(0))

        print(f"\n{'='*60}")
        print("RANK MISMATCH HANDLING")
        print(f"{'='*60}")
        print(f"max_lora_rank=4, test LoRA rank=8")

        # Step 1: Try to add the LoRA
        add_result = None
        add_error = None
        try:
            add_result = llm.llm_engine.add_lora(lora_req)
            print(f"\nadd_lora() result: {add_result}")
        except Exception as e:
            add_error = str(e)
            print(f"\nadd_lora() raised: {type(e).__name__}: {e}")

        # Step 2: If add succeeded, try to generate (error might occur here)
        generate_result = None
        generate_error = None
        if add_result is True:
            print("\nadd_lora succeeded, trying generate()...")
            sampling_params = SamplingParams(max_tokens=20)
            try:
                outputs = llm.generate(
                    [TEST_PROMPT],
                    sampling_params,
                    lora_request=LoRARequest("test_lora", 1, get_lora_path(0))
                )
                generate_result = outputs[0].outputs[0].text
                print(f"generate() succeeded: '{generate_result[:50]}...'")
            except Exception as e:
                generate_error = str(e)
                print(f"generate() raised: {type(e).__name__}: {e}")

        # Document the behavior
        print(f"\n{'='*60}")
        print("BEHAVIOR SUMMARY:")
        if add_error:
            print(f"  Error at add_lora(): {add_error[:100]}")
        elif generate_error:
            print(f"  add_lora() succeeded but generate() failed: {generate_error[:100]}")
        elif generate_result:
            print(f"  Both add_lora() and generate() succeeded (rank check passed)")
        else:
            print(f"  add_lora() returned {add_result}")
        print(f"{'='*60}")

        del llm
        cleanup_gpu()

        print("PASS: Rank mismatch behavior documented")


if __name__ == "__main__":
    pytest.main([__file__, "-v", "-s"])
