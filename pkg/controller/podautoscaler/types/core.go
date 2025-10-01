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

package types

import (
	"fmt"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// MetricKey identifies a specific metric for a PodAutoscaler
type MetricKey struct {
	Namespace  string // Target workload namespace
	Name       string // Target workload name
	MetricName string // Metric name (e.g., "concurrency", "qps")
	// PodAutoscaler identification for multi-tenancy
	PaNamespace string // PodAutoscaler namespace
	PaName      string // PodAutoscaler name
}

// String returns a unique string identifier for this metric key
func (m MetricKey) String() string {
	return fmt.Sprintf("%s/%s/%s", m.PaNamespace, m.PaName, m.MetricName)
}

// ScaleTarget represents what to scale
type ScaleTarget struct {
	Namespace  string
	Name       string
	Kind       string
	APIVersion string
	MetricKey  MetricKey
}

// ScaleRequest contains everything needed for scaling
type ScaleRequest struct {
	PodAutoscaler   autoscalingv1alpha1.PodAutoscaler
	CurrentReplicas int32
	Pods            []corev1.Pod
	Timestamp       time.Time
}

// ScaleResult contains scaling outcome - extends existing ScaleResult
type ScaleResult struct {
	DesiredPodCount     int32
	ExcessBurstCapacity int32
	ScaleValid          bool
	Reason              string
	Algorithm           string
	Metadata            map[string]interface{}
}

// ScalingConstraints defines scaling limits
type ScalingConstraints struct {
	MinReplicas int32
	MaxReplicas int32
}

// CollectionSpec defines what metrics to collect
type CollectionSpec struct {
	Namespace    string // Target namespace
	TargetName   string // Target resource name
	MetricName   string // Name of the metric
	MetricSource autoscalingv1alpha1.MetricSource
	Pods         []corev1.Pod
	Timestamp    time.Time
}

// MetricSnapshot contains collected raw metrics
type MetricSnapshot struct {
	Namespace  string
	TargetName string
	MetricName string
	Values     []float64 // TODO(Jeffwan): Prefill/Decode case needs extension
	Timestamp  time.Time
	Source     string
	Error      error
}
