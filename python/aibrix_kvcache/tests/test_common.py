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

import threading
import unittest

from aibrix_kvcache.common import ObjectPool


class TestObjectPool(unittest.TestCase):
    def test_constructor_validates_parameters(self):
        with self.assertRaises(ValueError):
            ObjectPool(klass=None, object_creator=None)

        with self.assertRaises(ValueError):
            ObjectPool(min_pool_size=-1)

        with self.assertRaises(ValueError):
            ObjectPool(min_pool_size=10, max_pool_size=5)

    def test_initialization_fills_min_objects(self):
        pool = ObjectPool(object_creator=lambda: object(), min_pool_size=3)
        self.assertEqual(pool.size(), 3)
        self.assertEqual(pool.capacity(), 3)

    def test_get_returns_objects_from_pool(self):
        pool = ObjectPool(object_creator=lambda: object(), min_pool_size=3)
        objs = pool.get(2)
        self.assertEqual(len(objs), 2)
        self.assertEqual(pool.size(), 1)

    def test_get_creates_new_objects_when_pool_empty(self):
        pool = ObjectPool(object_creator=lambda: object(), min_pool_size=1)
        obj = pool.get(1)[0]
        self.assertIsNotNone(obj)

        new_objs = pool.get(2)
        self.assertEqual(len(new_objs), 2)
        self.assertEqual(pool.size(), 0)
        self.assertEqual(pool.capacity(), 3)

    def test_get_respects_max_capacity(self):
        pool = ObjectPool(
            object_creator=lambda: object(), min_pool_size=1, max_pool_size=2
        )
        _ = pool.get(1)
        _ = pool.get(1)
        obj3 = pool.get(1)
        self.assertIsNone(obj3)

    def test_put_returns_objects_back_to_pool(self):
        pool = ObjectPool(object_creator=lambda: object(), min_pool_size=1)
        obj = pool.get(1)[0]
        pool.put(obj)
        self.assertEqual(pool.size(), 1)

    def test_put_ignores_full_pool(self):
        pool = ObjectPool(
            object_creator=lambda: object(), min_pool_size=2, max_pool_size=2
        )
        obj = pool.get(2)
        pool.put(obj)
        self.assertEqual(pool.size(), 2)
        pool.put(obj)
        self.assertEqual(pool.size(), 2)

    def test_thread_safety(self):
        POOL_SIZE = 3
        THREAD_COUNT = 10
        ITERATIONS = 100

        pool = ObjectPool(
            object_creator=lambda: object(), min_pool_size=POOL_SIZE
        )

        def worker():
            for _ in range(ITERATIONS):
                obj = pool.get(1)
                if obj:
                    pool.put(obj[0])

        threads = [threading.Thread(target=worker) for _ in range(THREAD_COUNT)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        self.assertGreaterEqual(pool.size(), 0)
        self.assertLessEqual(pool.size(), pool.max_pool_size)

    def test_put_with_multiple_objects(self):
        pool = ObjectPool(
            object_creator=lambda: object(), min_pool_size=0, max_pool_size=5
        )
        objs = [object(), object()]
        pool.put(objs)
        self.assertEqual(pool.size(), 2)

    def test_put_with_invalid_input(self):
        pool = ObjectPool(object_creator=lambda: object(), min_pool_size=0)
        pool.put(None)
        self.assertEqual(pool.size(), 0)
