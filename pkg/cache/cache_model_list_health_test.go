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

package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestVerifiedModelListUsesAppliedPodEvents(t *testing.T) {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "served", Namespace: "default", UID: "one", ResourceVersion: "10",
			Labels: map[string]string{constants.ModelLabelName: "model"},
		},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pods") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		list := &v1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			ListMeta: metav1.ListMeta{ResourceVersion: "10"},
		}
		if !deleted.Load() {
			list.Items = []v1.Pod{pod}
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
		server.CloseClientConnections()
		server.Close()
	})

	provider := discovery.NewKubernetesProvider(&rest.Config{Host: server.URL}).
		WithModelAdapters(false).WithModelListHealth(time.Minute)
	store := NewForTest()
	result := make(chan error, 1)
	go func() { result <- initDiscoveryProvider(store, provider, stopCh) }()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not finish initial handler sync")
	}
	models, err := store.ListModelsVerified(context.Background(), true)
	require.NoError(t, err)
	assert.Equal(t, []string{"model"}, models)

	deleted.Store(true)                     // The watch has not delivered the deletion.
	provider.ModelListHealth().Invalidate() // Force the next request to verify.
	models, err = store.ListModelsVerified(context.Background(), true)
	require.ErrorIs(t, err, discovery.ErrModelListDiscoveryUnavailable)
	assert.Nil(t, models)
}
