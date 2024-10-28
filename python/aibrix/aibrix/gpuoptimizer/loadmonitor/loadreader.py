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

from typing import Protocol, List
from datetime import datetime
import pandas as pd
import numpy as np

class LoadRecord:
    """LoadRecord models a tuple with the following fields: ts, input tokens, output tokens, and frequency."""

    def __init__(self, record: tuple) -> None:
        self.record = record

    @property
    def ts(self) -> float:
        return self.record[0]

    @property
    def input_tokens(self) -> float:
        return self.record[1]
    
    @property
    def output_tokens(self) -> float:
        return self.record[2]
    
    @property
    def freq(self) -> int:
        if len(self.record) < 4:
            return 1
        return self.record[3]

class LoadReader(Protocol):
    def read(self, ts: float=0.0) -> List[LoadRecord]:
        """Read the next batch of records from the data source."""

class DatasetLoadReader:
    """DatasetLoadReader reads the load records from a dataset."""

    def __init__(self, filepath, n_batch=100) -> None:
        self.df = pd.read_csv(filepath)
        self.df['input_tokens'] = np.log2(self.df['input_tokens'])
        self.df['output_tokens'] = np.log2(self.df['output_tokens'])
        self.n_batch = n_batch
        self.n_batchs = len(self.df) // self.n_batch
        self.n_read = 0

    def read(self, ts: float=0.0) -> List[LoadRecord]:
        """Read the next batch of records from the data source."""
        records = []
        end = self.n_read+self.n_batch
        if end > len(self.df):
            end = len(self.df)

        chunk = self.df.iloc[self.n_read:end]
        self.n_read = end
        for _, row in chunk.iterrows():
            records.append(LoadRecord((datetime.now().timestamp(), row['input_tokens'], row['output_tokens'])))

        return records
    
    def progress(self) -> float:
        return self.n_read / len(self.df)
