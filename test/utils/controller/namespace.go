// Copyright 2026 The Aibrix Team.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package controller provides helpers for controller integration tests.
package controller

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateNamespace creates a namespace and waits until it can be retrieved.
func CreateNamespace(
	ctx context.Context,
	c client.Client,
	generateName string,
	timeout time.Duration,
	pollingInterval ...time.Duration,
) *corev1.Namespace {
	ginkgo.GinkgoHelper()
	if len(pollingInterval) > 1 {
		ginkgo.Fail("CreateNamespace accepts at most one polling interval")
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: generateName}}
	gomega.Expect(c.Create(ctx, ns)).To(gomega.Succeed())
	ginkgo.DeferCleanup(func() {
		DeleteNamespace(ctx, c, ns)
	})
	getNamespace := func() error {
		return c.Get(ctx, client.ObjectKeyFromObject(ns), ns)
	}
	if len(pollingInterval) == 1 {
		gomega.Eventually(getNamespace, timeout, pollingInterval[0]).Should(gomega.Succeed())
	} else {
		gomega.Eventually(getNamespace, timeout).Should(gomega.Succeed())
	}
	return ns
}

// DeleteNamespace deletes a non-nil namespace, ignoring an already absent namespace.
func DeleteNamespace(ctx context.Context, c client.Client, ns *corev1.Namespace) {
	ginkgo.GinkgoHelper()
	if ns == nil {
		return
	}
	gomega.Expect(client.IgnoreNotFound(c.Delete(ctx, ns))).To(gomega.Succeed())
}
