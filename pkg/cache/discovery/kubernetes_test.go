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

package discovery

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vllm-project/aibrix/pkg/client/clientset/versioned/fake"
)

func TestCanListModelClaims(t *testing.T) {
	claims := schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"listable", nil, true},
		{"the role may not list claims", apierrors.NewForbidden(claims, "", errors.New("no rule")), false},
		{"the CRD is not installed", apierrors.NewNotFound(claims, ""), false},
		// Anything else may pass, so the informer is left to retry it.
		{"a transient error", apierrors.NewServiceUnavailable("try again"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			if tc.err != nil {
				client.PrependReactor("list", "modelclaims",
					func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			}
			assert.Equal(t, tc.want, canListModelClaims(client))
		})
	}
}
