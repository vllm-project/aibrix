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

class ModelMonitor:
    def __init__(self, model_name: str, deployment_name: str = None, namespace: str = None, replicas: int = 0):
        self.model_name = model_name
        self.deployments = {}
        self.thread = None
        if deployment_name is not None and namespace is not None:
            self.add_deployment(deployment_name, namespace, replicas)

    def add_deployment(self, deployment_name: str, namespace: str, replicas: int):
        self.deployments[self._deployment_entry_point(deployment_name, namespace)] = replicas

    def remove_deployment(self, deployment_name: str, namespace: str) -> int:
        """remove deployment from monitor, return the number of deployments left."""
        del self.deployments[self._deployment_entry_point(deployment_name, namespace)]
        return len(self.deployments)

    def read_deployment_num_replicas(self, deployment_name: str, namespace: str) -> int:
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(f"Deployment {namespace}:{deployment_name} of model {self.model_name} not monitored")
        return self.deployments[key]
    
    def update_deployment_num_replicas(self, deployment_name: str, namespace: str, replicas: int):
        key = self._deployment_entry_point(deployment_name, namespace)
        if key not in self.deployments:
            raise Exception(f"Deployment {namespace}:{deployment_name} of model {self.model_name} not monitored")

        self.deployments[key] = replicas

    def start(self):
        """Start the model monitor thread"""
        pass

    def stop(self):
        """Stop the model monitor thread"""
        pass

    def _deployment_entry_point(self, deployment_name: str, namespace: str):
        """Entry point for each deployment"""
        return f"{namespace}/{deployment_name}"