/*
Copyright 2026 The Aibrix Team.

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

package controller

import (
	"context"

	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelwarmup "github.com/vllm-project/aibrix/pkg/controller/modelwarmup"
)

func NewModelWarmup(namespace, name, node string) *modelapi.ModelWarmup {
	return &modelapi.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace,
	}, Spec: modelapi.ModelWarmupSpec{
		Targets: []modelapi.ModelWarmupTarget{{Nodes: &modelapi.ModelWarmupNodesTarget{Names: []string{node}}}},
		ImagePreload: modelapi.ModelWarmupImagePreload{Images: []modelapi.ModelWarmupImage{{
			Image: "busybox:1.36", Command: []string{"sh", "-c", "exit 0"}, ImagePullPolicy: corev1.PullIfNotPresent,
		}}},
		Policies: &modelapi.ModelWarmupPolicies{Parallelism: ptr.To[int32](1), GlobalTimeoutSeconds: ptr.To[int64](60),
			RetryLimit: ptr.To[int32](1), TTLSecondsAfterFinished: ptr.To[int32](60)},
	}}
}

func ListModelWarmupJobs(g gomega.Gomega, ctx context.Context, c client.Client, namespace, name string) []batchv1.Job {
	var jobs batchv1.JobList
	g.Expect(c.List(ctx, &jobs, client.InNamespace(namespace),
		client.MatchingLabels{modelwarmup.WarmupLabelKey: name})).To(gomega.Succeed())
	return jobs.Items
}
