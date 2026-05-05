/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package routingalgorithms

import (
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

// podServerKey represents a pod with specific port
type podServerKey struct {
	pod  *v1.Pod
	port int
}

func (k podServerKey) String() string {
	if k.port == 0 {
		return fmt.Sprintf("%s/%s", k.pod.Namespace, k.pod.Name)
	}
	return fmt.Sprintf("%s/%s/%d", k.pod.Namespace, k.pod.Name, k.port)
}

// revert podServerKey.String to (namespace, name, port)
func parseServerKey(key string) (namespace string, name string, port int) {
	parts := strings.Split(key, "/")
	if len(parts) == 2 {
		namespace = parts[0]
		name = parts[1]
		port = 0
	} else if len(parts) == 3 {
		namespace = parts[0]
		name = parts[1]
		var err error
		if port, err = strconv.Atoi(parts[2]); err != nil {
			klog.ErrorS(err, "failed to parse port from server key", "key", key)
		}
	}
	return
}
