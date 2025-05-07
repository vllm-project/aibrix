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
def allocator():
    # use a small slab size for testing
    TensorPoolAllocator.SLAB_MAX_NBYTES = 1024
    return TensorPoolAllocator(16, 1024 * 1024)


def test_basic_allocation(allocator):
    """Test basic allocation and deallocation."""
    assert allocator.num_memory_regions == 1024
    assert allocator.capacity_nbytes == 1024 * 1024
    size = 1024
    status = allocator.alloc(size)
    allocator.assert_consistency()
    assert status.is_ok()
    assert len(allocator) == size
    mrs = status.value
    assert len(mrs) == size // allocator.mr_nbytes
    assert sum([mr.length for mr in mrs]) == size
    assert allocator.num_memory_regions == 1023
    [mr.ref_down() for mr in mrs]  # Trigger garbage collection
    assert len(allocator) == 0
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1024


def test_allocating_large(allocator):
    """Test allocating with a size larger than the slab size."""
    assert allocator.num_memory_regions == 1024
    size = 1024 * 2 + 256
    status = allocator.alloc(size)
    allocator.assert_consistency()
    assert status.is_ok()
    assert len(allocator) == size
    mrs = status.value
    assert len(mrs) == size // allocator.mr_nbytes
    assert sum([mr.length for mr in mrs]) == size
    assert allocator.num_memory_regions == 1022
    [mr.ref_down() for mr in mrs]  # Trigger garbage collection
    assert len(allocator) == 0
    allocator.assert_consistency()
    assert allocator.num_memory_regions == 1024


def test_coalescing_mechanism(allocator):
    """Test memory coalescing when MRs are deallocated."""
    assert allocator.num_memory_regions == 1024
    size1, size2, size3 = 128, 512, 128

    # Allocate three MRs
    status1 = allocator.alloc(size1)
    assert allocator.num_memory_regions == 1024
    status2 = allocator.alloc(size2)
    assert allocator.num_memory_regions == 1024
    status3 = allocator.alloc(size3)
    assert allocator.num_memory_regions == 1024

    assert status1.is_ok()
    assert status2.is_ok()
    assert status3.is_ok()
    assert len(allocator) == size1 + size2 + size3

    mrs1 = status1.value
    assert len(mrs1) == size1 // allocator.mr_nbytes
    assert sum([mr.length for mr in mrs1]) == size1
    mrs2 = status2.value
    assert len(mrs2) == size2 // allocator.mr_nbytes
    assert sum([mr.length for mr in mrs2]) == size2
    mrs3 = status3.value
    assert len(mrs3) == size3 // allocator.mr_nbytes
    assert sum([mr.length for mr in mrs3]) == size3

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
    max_size = 1 << 30  # 1GB for testing
    # the first allocation should succeed
    first = allocator.alloc(max_size)
    assert first.is_ok()
    # the second allocation should fail with OOM
    second = allocator.alloc(max_size)
    assert second.is_out_of_memory()


@pytest.mark.parametrize("rseed", [i * 100 + 43 for i in range(13)])
def test_stress_allocation(allocator, rseed):
    """Stress test: allocate and free many small blocks to test fragmentation
    and coalescing.
    """
    random.seed(rseed)

    num_allocations = 1000
    sizes = [16, 64, 128, 512]
    mrs = []

    allocated_size = 0
    for i in range(num_allocations):
        random.shuffle(sizes)
        size = sizes[i % len(sizes)]
        status = allocator.alloc(size)
        assert status.is_ok()
        mrs.extend(status.value)
        allocated_size += size
        assert sum([mr.length for mr in status.value]) == size
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
