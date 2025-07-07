# Copyright 2024 The Aibrix Team.
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

import random

import pytest

from aibrix_kvcache.memory import TensorPoolAllocator


@pytest.fixture
def allocator(compact_layout_enabled):
    # use a small slab size for testing
    TensorPoolAllocator.SLAB_MAX_NBYTES = 1024
    TensorPoolAllocator.ALLOC_SIZE_ALIGNMENT = 8
    return TensorPoolAllocator(capacity_nbytes=1024 * 1024)


def test_basic_allocation(allocator):
    """Test basic allocation and deallocation."""
    assert allocator.num_memory_regions == 1024
    assert allocator.capacity_nbytes == 1024 * 1024
    sizes = [16] * 64  # 1024
    status = allocator.alloc(sizes)
    allocator.assert_consistency()
    assert status.is_ok()
    assert len(allocator) == sum(sizes)
    mrs = status.value
    assert len(mrs) == len(sizes)
    assert sum([mr.length for mr in mrs]) == sum(sizes)
    assert allocator.num_memory_regions == 1023
    [mr.ref_down() for mr in mrs]  # Trigger garbage collection
    assert len(allocator) == 0
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1024


def test_allocating_large(allocator):
    """Test allocating with a set of sizes whose sum is larger than
    the slab size.
    """
    assert allocator.num_memory_regions == 1024
    sizes = [16] * 144  # 1024 * 2 + 256
    status = allocator.alloc(sizes)
    allocator.assert_consistency()
    assert status.is_ok()
    assert len(allocator) == sum(sizes)
    mrs = status.value
    assert len(mrs) == len(sizes)
    assert sum([mr.length for mr in mrs]) == sum(sizes)
    assert allocator.num_memory_regions == 1022
    [mr.ref_down() for mr in mrs]  # Trigger garbage collection
    assert len(allocator) == 0
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1024


def test_allocating_heterogeneous(allocator):
    """Test allocating with heterogeneous sizes."""
    assert allocator.num_memory_regions == 1024
    sizes = [random.randint(8, 1024) for _ in range(31)]
    status = allocator.alloc(sizes)
    allocator.assert_consistency()
    assert status.is_ok()
    assert len(allocator) == sum(sizes)
    mrs = status.value
    assert len(mrs) == len(sizes)
    assert sum([mr.length for mr in mrs]) == sum(sizes)
    [mr.ref_down() for mr in mrs]  # Trigger garbage collection
    assert len(allocator) == 0
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1024


def test_coalescing_mechanism(allocator):
    """Test memory coalescing when MRs are deallocated."""
    assert allocator.num_memory_regions == 1024
    u = [16]
    sizes1, sizes2, sizes3 = u * 8, u * 32, u * 8

    # Allocate three MRs
    status1 = allocator.alloc(sizes1)
    assert allocator.num_memory_regions == 1024
    status2 = allocator.alloc(sizes2)
    assert allocator.num_memory_regions == 1024
    status3 = allocator.alloc(sizes3)
    assert allocator.num_memory_regions == 1024

    assert status1.is_ok()
    assert status2.is_ok()
    assert status3.is_ok()
    assert len(allocator) == sum(sizes1) + sum(sizes2) + sum(sizes3)

    mrs1 = status1.value
    assert len(mrs1) == len(sizes1)
    assert sum([mr.length for mr in mrs1]) == sum(sizes1)
    mrs2 = status2.value
    assert len(mrs2) == len(sizes2)
    assert sum([mr.length for mr in mrs2]) == sum(sizes2)
    mrs3 = status3.value
    assert len(mrs3) == len(sizes3)
    assert sum([mr.length for mr in mrs3]) == sum(sizes3)

    # Free the middle allocation first
    [mr.ref_down() for mr in mrs2]
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1025

    # Free the first and last allocations
    [mr.ref_down() for mr in mrs1]
    allocator.assert_consistency()
    # mrs1 got merged with mrs2
    assert allocator.num_memory_regions == 1025
    [mr.ref_down() for mr in mrs3]
    allocator.assert_consistency()
    # all memory regions got merged into one
    assert allocator.num_memory_regions == 1024

    assert len(allocator) == 0


def test_out_of_memory(allocator):
    """Test allocator behavior when requesting more memory than available."""
    max_size = allocator.capacity_nbytes * 4
    sizes = [512] * (max_size // 512)
    # the first allocation should succeed
    first = allocator.alloc(sizes)
    assert first.is_ok()
    # the second allocation should fail with OOM
    second = allocator.alloc(sizes)
    assert second.is_out_of_memory()


@pytest.mark.parametrize("rseed", [i * 100 + 43 for i in range(13)])
def test_stress_allocation(allocator, rseed):
    """Stress test: allocate and free many small blocks to test fragmentation
    and coalescing.
    """
    random.seed(rseed)

    num_allocations = 100
    sizes_list = [
        [random.randint(8, 64) for _ in range(2)],
        [random.randint(64, 128) for _ in range(4)],
        [random.randint(128, 256) for _ in range(8)],
        [random.randint(256, 512) for _ in range(16)],
    ]
    mrs = []

    allocated_size = 0
    for i in range(num_allocations):
        random.shuffle(sizes_list)
        sizes = sizes_list[i % len(sizes_list)]
        status = allocator.alloc(sizes)
        assert status.is_ok()
        mrs.extend(status.value)
        allocated_size += sum(sizes)
        assert sum([mr.length for mr in status.value]) == sum(sizes)
        assert len(allocator) == allocated_size
        allocator.assert_consistency()

    random.shuffle(mrs)
    for i in range(len(mrs)):
        mr = mrs[i]
        mr.ref_down()

        size = mr.length
        allocated_size -= size
        assert len(allocator) == allocated_size
        allocator.assert_consistency()

    assert len(allocator) == 0
