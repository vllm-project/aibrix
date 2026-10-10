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

package e2eframework

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// UpdatePodLabels updates selected labels and restores their previous values
// after the test. Both operations retry resource-version conflicts because Pods
// can be updated concurrently by Kubernetes and AIBrix controllers.
func UpdatePodLabels(
	t *testing.T,
	ctx context.Context,
	client kubernetes.Interface,
	namespace, podName string,
	updates map[string]string,
) {
	t.Helper()

	pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s before updating labels: %v", podName, err)
	}
	previous := make(map[string]string, len(updates))
	for key := range updates {
		previous[key] = pod.Labels[key]
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Labels == nil {
			current.Labels = make(map[string]string)
		}
		for key, value := range updates {
			current.Labels[key] = value
		}
		_, err = client.CoreV1().Pods(namespace).Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		t.Fatalf("update labels for pod %s: %v", podName, err)
	}

	t.Cleanup(func() {
		restoreCtx := context.Background()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := client.CoreV1().Pods(namespace).Get(restoreCtx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			for key, value := range previous {
				if value == "" {
					delete(current.Labels, key)
				} else {
					current.Labels[key] = value
				}
			}
			_, err = client.CoreV1().Pods(namespace).Update(restoreCtx, current, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			t.Errorf("restore labels for pod %s: %v", podName, err)
		}
	})
}
