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

package monitor

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	autoscalerScaleAction = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aibrix_autoscaler_scale_action",
			Help: "Records autoscaler scaling decisions with desired replica count",
		},
		[]string{"namespace", "name", "algorithm", "reason"},
	)
)

func init() {
	// Register with controller-runtime metrics registry
	metrics.Registry.MustRegister(autoscalerScaleAction)
}
