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

import logging
import threading
import time
from datetime import datetime
from functools import reduce
from typing import Callable, Dict, Iterable, List, Optional, Union

import numpy as np
import pandas as pd

from aibrix.gpu_optimizer.optimizer import GPUProfile, Optimizer
from aibrix.gpu_optimizer.utils import DelayedLog

from .clusterer import Clusterer, MovingDBSCANClusterer
from .helpers import Centeroid, DataBuffer, DataPoint
from .load_reader import LoadReader, LoadRecord
from .profile_reader import ProfileReader

Empty_Array: Iterable = []

logger = logging.getLogger("aibrix.gpuoptimizer.loadmonitor")

debug_gpu_profile = GPUProfile(
    gpu="default", cost=1.0, tputs=[[100]], indexes=[[10], [10]]
)

DeploymentStates_Replicas_No_Overriden = -1


class DeploymentStates:
    """States of a deployment with resource version."""

    def __init__(self, name: str, replicas: int = 1, min_replicas: int = 0):
        self.name = name

        # _replicas stores optimized value
        self._replicas = replicas
        # _replicas_overriden stores replicas value for debugging
        self._replicas_overriden = DeploymentStates_Replicas_No_Overriden

        """The replicas output, ignore min_replicas in the normal mode."""
        self.min_replicas = min_replicas
        """The replicas for minimum mode. Ignore in normal optimization mode."""
        self.profile: Optional[GPUProfile] = None
        self.watch_ver: Optional[str] = None

    @property
    def cost(self):
        return 0.0 if self.profile is None else self.profile.cost * self.replicas

    @property
    def replicas(self):
        return self._replicas_overriden if self.overriden else self._replicas

    @replicas.setter
    def replicas(self, value):
        self._replicas = value

    @property
    def overriden(self):
        return self._replicas_overriden != DeploymentStates_Replicas_No_Overriden

    def override_replicas(self, value):
        self._replicas_overriden = value

    def minimize(self):
        """Set replica to minimum mode."""
        self.replicas = max(0, self.min_replicas)

    def __repr__(self):
        if self.overriden:
            return f"overriden {self.name}: {self.replicas}(${self.cost}), should be {self._replicas}"
        else:
            return f"{self.name}: {self.replicas}(${self.cost})"


class ModelMonitor:
    def __init__(
        self,
        model_name: str,
        watch_ver: str,
        load_reader: LoadReader,
        window: int = 240,
        deployment: Optional[DeploymentStates] = None,
        namespace: Optional[str] = None,
        profile_reader: Optional[ProfileReader] = None,
        debug: bool = False,
    ):
        """Initialize the model monitor.

        Args:
            model_name: The name of the model to monitor. This should be a unique identifier for the model.
            watch_ver: The k8s resource version of the deployment watching. This is used to keep track of the deployment's state.
            load_reader: An instance of the LoadReader class, used to retrieve workload information for the model.
            deployment_name: (optional) The name of the deployment associated with the model. Each deployment is designated to a specific GPU model.
            namespace: (optional) The Kubernetes namespace where the model deployment resides.
            replicas: (optional) The initial number of replicas for the model deployment.
            interval: (optional) The interval (in seconds) at which to monitor the model. Defaults to 10 seconds.
            window: (optional) The window (in seconds) to consider for clustering. Defaults to 300 seconds.
            debug: (optional) Whether to enable debugging behavior. Defaults to False.
        """
        self.model_name = model_name
        self.deployments: Dict[str, DeploymentStates] = {}
        self.thread = None
        self.outdated_watch_version = None
        self.last_watch_version = None
        self.debug = debug
        self.done = False
        self.window = float(window)
        self._lock = threading.Lock()

        # Load reader
        self._load_reader: LoadReader = load_reader

        # Profile reader
        self._profile_reader: Optional[ProfileReader] = profile_reader

        # Optimizer
        self._profiles: Dict[str, GPUProfile] = {}
        self._optimizer = Optimizer()

        # Monitor states
        self._centers: Iterable[Centeroid] = Empty_Array
        self._labels: Iterable[int] = Empty_Array
        self._data: Optional[DataBuffer] = None
        self._progress: float = 0.0
        self._cost = 0.0

        if profile_reader is not None:
            self.load_profiles(profile_reader)
        elif self.debug:
            # Add debug_gpu_profile anyway if debugging
            self._optimizer.set_profile(debug_gpu_profile)

        # Add first deployment
        if deployment is not None:
            self.add_deployment(watch_ver, deployment.name, namespace, deployment)

    def add_deployment(
        self,
        watch_ver: str,
        deployment_name: str,
        namespace: Optional[str],
        deployment: Union[DeploymentStates, Callable[[], DeploymentStates]],
    ):
        # Update optimizer
        key = self._deployment_entry_point(deployment_name, namespace)
        profile = self._match_profile(key, deployment_name)
        if profile is not None:
            # No lock required here since the deployment has not been added to deployments.
            try:
                self._optimizer.set_profile(profile)
            except Exception as e:
                logger.warning(
                    f"Failed to set GPU profile for {key}. Optimizer will skip the GPU: {e}"
                )
        else:
            logger.warning(
                f"No GPU profile found for {key}. Optimizer will skip the GPU."
            )

        # add to deployment registry
        self._lock.acquire(blocking=True)
        if key not in self.deployments:
            self.deployments[key] = deployment() if callable(deployment) else deployment

        old_cost = self.deployments[key].cost
        self.deployments[key].profile = profile
        self.deployments[key].watch_ver = watch_ver
        self._cost += self.deployments[key].cost - old_cost
        self.last_resource_version = watch_ver
        self._lock.release()

    def remove_deployment(self, deployment_name: str, namespace: str) -> int:
        """remove deployment from monitor, return the number of deployments left."""
        key = self._deployment_entry_point(deployment_name, namespace)

        self._lock.acquire(blocking=True)
        self._optimizer.delete_profile(key)
        del self.deployments[key]
        self._lock.release()

        return len(self.deployments)

    def read_deployment_num_replicas(self, deployment_name: str, namespace: str) -> int:
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(
                f"Deployment {namespace}:{deployment_name} of model {self.model_name} is not monitored"
            )
        return self.deployments[key].replicas

    def update_deployment_num_replicas(
        self,
        deployment_name: str,
        namespace: str,
        replicas: int,
        overriding: bool = False,
    ):
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(
                f"Deployment {namespace}:{deployment_name} of model {self.model_name} is not monitored"
            )

        if overriding:
            # Disable all override once for all if no overriden is detected.
            if replicas == DeploymentStates_Replicas_No_Overriden:
                for states in self.deployments.values():
                    states.override_replicas(replicas)
                return

            self.deployments[key].override_replicas(replicas)
        else:
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

    def load_profiles(self, profile_reader: Optional[ProfileReader] = None) -> bool:
        """Load profiles from a file"""
        try:
            if profile_reader is None:
                if self._profile_reader is None:
                    logger.error("Profile reader not initialized")
                    return False
                profile_reader = self._profile_reader
            else:
                self._profile_reader = profile_reader

            profiles = profile_reader.read()
            for profile in profiles:
                if self._update_profile(profile):
                    logger.debug(f"Profile of {profile.gpu} updated.")

            return True
        except Exception as e:
            logger.error(f"Failed to load profiles: {e}")

            return False

    def _update_profile(self, profile: GPUProfile) -> bool:
        """Update a profile, will update the formal alias copy, too."""
        key = profile.gpu
        cost_diff = profile.cost
        log_event = (
            True  # log event if the profile is added to non-profile deployments.
        )
        if key in self._profiles:
            # profile already exists, check if it is updated
            if profile.created <= self._profiles[key].created:
                return False

            cost_diff -= self._profiles[
                key
            ].cost  # We can safely assume the existing profile has been added to the optimizer if any deployments match it.
            log_event = False

            if self._profiles[key].gpu != key:
                # key is a abbreviation copy, update the formal copy
                profile.gpu = self._profiles[key].gpu
                if profile.gpu in self._profiles:
                    self._profiles[profile.gpu] = profile

        # update the profile of key, note that the profile.gpu is already formalized.
        self._profiles[key] = profile

        # apply update to optimizer for existing deployments.
        deployment_key: Optional[str] = (
            profile.gpu
        )  # Fast path, note that the profile.gpu is already formalized if it match any deployments.
        if profile.gpu not in self.deployments:
            deployment_key = None
            # slow path, find deployment by deployment_name
            # noted that the profile.gpu is not formalized if the code reaches here.
            for key, states in self.deployments.items():
                if states.name != profile.gpu:
                    continue

                deployment_key = profile.gpu = key  # formalize the gpu field
                break
        # deployment existed
        if deployment_key is not None:
            self._lock.acquire(blocking=True)
            if profile.gpu in self.deployments:  # double check
                self._optimizer.set_profile(profile)
                self._cost += cost_diff * self.deployments[key].replicas
            else:
                log_event = False
            self._lock.release()
            if log_event:
                logger.info(
                    f"Profile added to {profile.gpu}. Optimizer will consider corresponding GPU."
                )

        return True

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
        except Exception as e:
            logger.error(f"Unexpected error on monitoring {self.model_name}: {e}")
        logger.info(f"{self.model_name} stopped")
        return

    def _run_yieldable(self, yieldable: bool, window_scaling: float = 1.0):
        """_run implementation. Using a separate yieldable implementation for _run being accepted by Threading"""
        # Define clusterer
        clusterers: List[Clusterer] = [
            MovingDBSCANClusterer(0.5, 10, 4, self.window * window_scaling)
            # MovingDBSCANClusterer(0.8, 100, 4, self.window * window_scaling),
            # DBSCANClusterer(0.5, 10),
        ]
        self._data = DataBuffer(
            int(self.window) * 10
        )  # Assume 10 RPS, will expand according to the actual RPS
        # lvl2data = DataBuffer(window)

        logger.debug(f"{self.model_name} initialized")

        n = 0
        while not self.done:
            start = datetime.now().timestamp()

            # Keep window rotating
            movingCluster: MovingDBSCANClusterer = clusterers[0]  # type: ignore
            if movingCluster.validate():
                # Data refreshing
                self._data.trim_head(-movingCluster.length)

            # Read new tokens
            tokens = list(
                self._expand_records(self._load_reader.read(datetime.now().timestamp()))
            )  # read data
            if len(tokens) > 0:
                self._data.reconcile(
                    movingCluster.length + len(tokens)
                )  # since databuffer.append will not expand the buffer automatically, we need to reconcile ourself.
                dps = self._data.append(tokens)
                movingCluster.insert(dps)
                self._data.commit()

            # track domanent token patterns
            if self._data.len > 0:
                uncategorized = None  # Set to [] for further analysis
                self._labels, self._centers = movingCluster.get_cluster_labels(
                    self._data.datapoints, uncategorized=uncategorized
                )
            else:
                self._labels, self._centers = Empty_Array, Empty_Array

            n += 1
            duration = (datetime.now().timestamp() - start) * 1000
            centers = list(self._centers)
            logger.debug(
                "%s batch %d took %d ms: %d centers: %s",
                self.model_name,
                n,
                round(duration),
                len(centers),
                DelayedLog(lambda: str([str(center) for center in self._centers])),
            )

            if len(centers) > 0:
                # Optimize
                self._optimize(centers, self._data.len)
            elif self._data.len == 0:
                self._minimize()
            else:
                logger.info("Skip optimization, insufficient data")

            if yieldable:
                # If yieldable, return the _run it self for further processing
                # This allows caller controls the progress.
                yield
            else:
                wait = self._load_reader.next_available() - datetime.now().timestamp()
                if wait > 0:
                    time.sleep(wait)
                    # Validate time elapsed
                    while (
                        datetime.now().timestamp() < self._load_reader.next_available()
                    ):
                        time.sleep(1)

    def stop(self):
        """Stop the model monitor thread"""
        self.done = True
        logger.debug(f"Model monitor {self.model_name} stop signaled")
        pass

    def _expand_records(self, records: Iterable[LoadRecord]):
        for record in records:
            for i in range(record.freq):
                yield DataPoint(
                    record.input_tokens, record.output_tokens, age=record.ts
                )

    def _deployment_entry_point(self, deployment_name: str, namespace: Optional[str]):
        """Entry point for each deployment"""
        if namespace is None:
            return deployment_name

        return f"{namespace}/{deployment_name}"

    def _match_profile(self, key, deployment_name) -> Optional[GPUProfile]:
        if key in self._profiles:
            return self._profiles[key]
        elif deployment_name in self._profiles:
            # Update the gpu name to formalized key.
            profile: GPUProfile = self._profiles[deployment_name]
            profile.gpu = key
            return profile
        elif self.debug:
            # Copy the debug profile and override the gpu name with given key
            copy = GPUProfile(**debug_gpu_profile.__dict__)
            copy.gpu = key
            return copy

        return None

    def _optimize(self, centers: Iterable[Centeroid], total_request_rate: int):
        # Update profiles.
        self.load_profiles()

        if not self._optimizer.set_workload_distribution(centers, total_request_rate):
            return

        start = datetime.now().timestamp()
        result = self._optimizer.run()
        duration = (datetime.now().timestamp() - start) * 1000
        if result is None:
            logger.info(
                f"{self.model_name} optimization took {duration} ms, unexpected void rsult, skip."
            )
            return

        cost = result["cost"]
        del result["cost"]

        # Update deployment
        self._lock.acquire(blocking=True)
        # Insure all deployments are up to date.
        for key, replicas in result.items():
            if key not in self.deployments and replicas > 0:
                logger.warning(
                    f"Not all deployments in optimization result available: {key}, discard result"
                )
                self._lock.release()
                return
        # Reset replicas of all deployments.
        for key, states in self.deployments.items():
            states.replicas = result[key] if key in result else 0
        self._cost = cost
        self._lock.release()

        logger.info(
            f"{self.model_name} optimization took {duration} ms, cost ${self._cost}, coverage: {self.coverage}%: {list(self.deployments.values())}"
        )

    def _minimize(self):
        # Update deployment
        self._lock.acquire(blocking=True)
        # Reset replicas of all deployments.
        cost = 0.0
        for states in self.deployments.values():
            states.minimize()  # Will apply min_replicas obligation.
            cost += states.cost
        self._cost = cost
        self._lock.release()
        logger.info(
            f"{self.model_name} scaled to minimum, cost ${self._cost}: {list(self.deployments.values())}"
        )

    @property
    def centers(self):
        return self._centers

    @property
    def dataframe(self):
        if self._data is None:
            return None

        df = pd.DataFrame(
            data=np.array([self._data.x, self._data.y, self._labels]).transpose(),
            columns=["input_tokens", "output_tokens", "label"],
        )
        return df

    @property
    def labeled(self):
        return reduce(lambda cnt, center: cnt + center.size, self._centers, 0)

    @property
    def progress(self) -> str:
        """A progress indicator of the data source.
        For dataset, it is the percentage of the data read.
        For stream, it is the time elapsed since the start of the monitor."""
        return self._load_reader.progress()

    @property
    def cost(self) -> float:
        """The total cost of the model."""
        return self._cost

    @property
    def coverage(self) -> float:
        """The coverage of the model."""
        if self._data is None:
            return 0.0

        return self.labeled / self._data.len * 100
