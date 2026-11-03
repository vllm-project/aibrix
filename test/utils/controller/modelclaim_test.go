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
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestTriggerReconcileRetriesConflict(t *testing.T) {
	gomega.RegisterTestingT(t)

	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := modelapi.AddToScheme(scheme); err != nil {
		t.Fatalf("add ModelClaim scheme: %v", err)
	}
	claim := &modelapi.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default"}}
	patchAttempts := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				cli client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				patchAttempts++
				if patchAttempts == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: modelapi.GroupVersion.Group, Resource: "modelclaims"},
						obj.GetName(),
						errors.New("simulated concurrent status update"),
					)
				}
				return cli.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	fixture := &ModelClaimFixture{
		ctx:      ctx,
		client:   fakeClient,
		timeout:  100 * time.Millisecond,
		interval: time.Millisecond,
	}

	fixture.TriggerReconcile(claim)

	if patchAttempts != 2 {
		t.Fatalf("expected 2 patch attempts, got %d", patchAttempts)
	}
	updated := &modelapi.ModelClaim{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(claim), updated); err != nil {
		t.Fatalf("get patched ModelClaim: %v", err)
	}
	if updated.Annotations["test.aibrix.ai/reconcile"] == "" {
		t.Fatal("expected reconcile annotation to be set")
	}
}

func TestDecodeModelClaimRouteAnnotationPreservesExactValues(t *testing.T) {
	route, err := decodeModelClaimRouteAnnotation(`{"model":"qwen","port":19000,"state":"active"}`)
	if err != nil {
		t.Fatalf("decode route annotation: %v", err)
	}
	if route.Port != 19000 {
		t.Fatalf("expected exact port 19000, got %d", route.Port)
	}
	if route.State != "active" {
		t.Fatalf("expected exact state active, got %q", route.State)
	}
}
