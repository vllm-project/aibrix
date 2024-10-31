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
from typing import List, Optional, Iterable
from .loadreader import LoadReader, DatasetLoadReader
from .clusterer import Clusterer, DBSCANClusterer, MovingDBSCANClusterer
from functools import reduce
from .helpers import DataPoint, DataBuffer, Centeroid
from datetime import datetime
import os
import time
import pandas as pd
import numpy as np
import logging

Empty_Array = []

logger = logging.getLogger("ModelMonitor")

class DeploymentStates:
    """States of a deployment with resource version."""
    def __init__(self, replicas: int, watch_ver: str):
        self.replicas = replicas
        self.watch_ver = watch_ver

class ModelMonitor:
    def __init__(self, model_name: str, watch_ver: str, deployment_name: str = None, namespace: str = None, replicas: int = 0):
        self.model_name = model_name
        self.deployments = {}
        self.thread = None
        self.outdated_watch_version = None
        self.last_watch_version = None
        self.done = False
        self.interval = 15 # update every 15 seconds
        if deployment_name is not None and namespace is not None:
            self.add_deployment(watch_ver, deployment_name, namespace, replicas)

        # Monitor states
        self._centers: Iterable[Centeroid] = Empty_Array
        self._labels: Iterable[int] = Empty_Array
        self._data: Optional[DataBuffer] = None
        self._progress: float = 0.0
        

    def add_deployment(self, watch_ver: str, deployment_name: str, namespace: str, replicas: int = 0):
        key = self._deployment_entry_point(deployment_name, namespace)
        if key in self.deployments:
            self.deployments[key].watch_ver = watch_ver
        else:
            self.deployments[key] = DeploymentStates(replicas, watch_ver)
        self.last_resource_version = watch_ver

    def remove_deployment(self, deployment_name: str, namespace: str) -> int:
        """remove deployment from monitor, return the number of deployments left."""
        del self.deployments[self._deployment_entry_point(deployment_name, namespace)]
        return len(self.deployments)

    def read_deployment_num_replicas(self, deployment_name: str, namespace: str) -> int:
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(f"Deployment {namespace}:{deployment_name} of model {self.model_name} is not monitored")
        return self.deployments[key].replicas
    
    def update_deployment_num_replicas(self, deployment_name: str, namespace: str, replicas: int):
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(f"Deployment {namespace}:{deployment_name} of model {self.model_name} is not monitored")

        self.deployments[key].replicas = replicas

    def mark_deployments_outdated(self):
        """Save last resource version and start the validation"""
        self.outdated_watch_version = self.last_watch_version

    def clear_outdated_deployments(self) -> int:
        """Remove outdated deployments from the monitor.
        Return the number of deployments left."""
        for key, states in self.deployments.items():
            if states.watch_ver == self.outdated_watch_version:
                del self.deployments[key]
        return len(self.deployments)

    def start(self):
        """Start the model monitor thread"""
        self.thread = threading.Thread(target=self._run)
        self.thread.daemon = True
        self.thread.start()

    def _run(self):
        """Monitor the model"""
        logger.debug(f"{self.model_name} started")
        try:
            next(self._run_yieldable(False))
        except StopIteration:
            pass
        logger.info(f"{self.model_name} stopped")
        return
    
    def _run_yieldable(self, yieldable):
        """_run implementation. Using a separate yieldable implementation for _run being accepted by Threading"""
        window = 4000
        # Load dataset reader
        directory = os.path.dirname(os.path.abspath(__file__))
        reader: LoadReader = DatasetLoadReader(directory + '/data/sharegpt.csv', n_batch=100)
        span = window / reader.n_batch
        # Define clusterer
        clusterers: List[Clusterer] = [MovingDBSCANClusterer(0.8, 100, 4, window), DBSCANClusterer(0.5, 10)]
        # Simulated data source for display
        self._data = DataBuffer(window)
        # lvl2data = DataBuffer(window)

        logger.debug(f"{self.model_name} initialized")
       
        n = 0
        while not self.done:
            start = datetime.now().timestamp()

            # Keep window rotating
            movingCluster: MovingDBSCANClusterer = clusterers[0]
            if movingCluster.validate():
                # Data refreshing 
                self._data.trim_head(-movingCluster.length)

            # Read new tokens
            tokens = [DataPoint([record.input_tokens, record.output_tokens],n) for record in reader.read()]  # read data
            movingCluster.insert(tokens)
            self._data.append(tokens)

            # track domanent token patterns
            uncategorized = None # Set to [] for further analysis
            self._labels, self._centers = movingCluster.get_cluster_labels(self._data.xy, uncategorized=uncategorized)

            self._progress = round(reader.progress()*100, 2)
            n += 1
            duration = (datetime.now().timestamp() - start) * 1000
            logger.debug(f"{self.model_name} batch {n} took {duration}ms ")
            if yieldable:
                # If yieldable, return the _run it self for further processing
                # This allows caller controls the progress.
                yield self._run_yieladble(yieldable)
            else:
                time.sleep(self.interval)

    def stop(self):
        """Stop the model monitor thread"""
        self.done = True
        logger.debug(f"Model monitor {self.model_name} stop signaled")
        pass

    def _deployment_entry_point(self, deployment_name: str, namespace: str):
        """Entry point for each deployment"""
        return f"{namespace}/{deployment_name}"
    
    @property
    def centers(self):
        return self._centers
    
    @property
    def dataframe(self):
        if self._data is None:
            return None
        return pd.DataFrame(
            data=np.array([self._data.x, self._data.y, self._labels]).transpose(), columns=['input_tokens', 'output_tokens', 'label'])
    
    @property
    def labeled(self):
        return reduce(lambda cnt, center: cnt+center.size, self._centers, 0)
    
    @property
    def progress(self):
        """A progress indicator of the data source.
        For dataset, it is the percentage of the data read.
        For stream, it is the time elapsed since the start of the monitor."""
        return self._progress
    
