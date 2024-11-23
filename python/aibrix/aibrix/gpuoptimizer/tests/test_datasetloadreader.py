import unittest

import numpy as np
from loadmonitor.loadreader import DatasetLoadReader, unittest_filepath


class TestDatasetLoadReader(unittest.TestCase):
    def __init__(self, methodName: str = "runTest") -> None:
        super().__init__(methodName)

        self.reader = DatasetLoadReader(unittest_filepath)

    def test_stair_agggregate(self):
        series = np.array(
            [7, 8, 59, 127, 128, 341, 1023, 1024, 2047, 2048, 3100, 4100, 9000, 10150],
            dtype=float,
        )
        expected = np.array(
            [1, 8, 56, 120, 128, 320, 960, 1024, 1984, 2048, 3072, 4096, 8192, 9216],
            dtype=float,
        )
        np.testing.assert_array_equal(
            self.reader.stair_aggregate(series, skip_log2=True), expected
        )


if __name__ == "__main__":
    unittest.main()
