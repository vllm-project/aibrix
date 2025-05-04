/*
Copyright 2025 The Aibrix Team.

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

package utils

import (
	"strconv"

	"k8s.io/klog/v2"
)

func GetStringAnnotationOrDefault(annotations map[string]string, key string, defaultValue string) string {
	if val, ok := annotations[key]; ok && val != "" {
		return val
	}
	klog.V(4).Infof("Annotation %s not set or empty, using default: %s. All annotations: %+v", key, defaultValue, annotations)
	return defaultValue
}

func GetPortAnnotationOrDefault(annotations map[string]string, key string, defaultValue int) int {
	if val, ok := annotations[key]; ok {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 && parsed <= 65535 {
			return parsed
		}
		klog.V(4).Infof("Invalid port for annotation %s: %s, using default %d. All annotations: %+v", key, val, defaultValue, annotations)
	}
	return defaultValue
}

func GetPositiveIntAnnotationOrDefault(annotations map[string]string, key string, defaultValue int) int {
	if val, ok := annotations[key]; ok {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			return parsed
		}
		klog.V(4).Infof("Invalid integer for annotation %s: %s, using default %d. All annotations: %+v", key, val, defaultValue, annotations)
	}
	return defaultValue
}
