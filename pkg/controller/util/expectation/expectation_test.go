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

// Note: The code is synced from https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/controller_utils_test.go
// with minor changes to fix linter issues.

package expectation

import (
	"sync"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/cache"
	testingclock "k8s.io/utils/clock/testing"
)

var (
	// KeyFunc is the short name to DeletionHandlingMetaNamespaceKeyFunc.
	// IndexerInformer uses a delta queue, therefore for deletes we have to use this
	// key function but it should be just fine for non delete events.
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

// NewFakeControllerExpectationsLookup creates a fake store for PodExpectations.
func NewFakeControllerExpectationsLookup(ttl time.Duration) (*ControllerExpectations, *testingclock.FakeClock) {
	fakeTime := time.Date(2009, time.November, 10, 23, 0, 0, 0, time.UTC)
	fakeClock := testingclock.NewFakeClock(fakeTime)
	ttlPolicy := &cache.TTLPolicy{TTL: ttl, Clock: fakeClock}
	ttlStore := cache.NewFakeExpirationStore(
		ExpKeyFunc, nil, ttlPolicy, fakeClock)
	return &ControllerExpectations{ttlStore}, fakeClock
}

func newReplicationController(replicas int) *v1.ReplicationController {
	rc := &v1.ReplicationController{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			UID:             uuid.NewUUID(),
			Name:            "foobar",
			Namespace:       metav1.NamespaceDefault,
			ResourceVersion: "18",
		},
		Spec: v1.ReplicationControllerSpec{
			Replicas: ptr.To[int32](int32(replicas)),
			Selector: map[string]string{"foo": "bar"},
			Template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"name": "foo",
						"type": "production",
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Image:                  "foo/bar",
							TerminationMessagePath: v1.TerminationMessagePathDefault,
							ImagePullPolicy:        v1.PullIfNotPresent,
							// skip the security context setting to avoid importing k8s.io/kubernetes/pkg/securitycontext
							//SecurityContext:        securitycontext.ValidSecurityContextWithContainerDefaults(),
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
					DNSPolicy:     v1.DNSDefault,
					NodeSelector: map[string]string{
						"baz": "blah",
					},
				},
			},
		},
	}
	return rc
}

func TestControllerExpectations(t *testing.T) {
	ttl := 30 * time.Second
	e, fakeClock := NewFakeControllerExpectationsLookup(ttl)
	// In practice we can't really have add and delete expectations since we only either create or
	// delete replicas in one rc pass, and the rc goes to sleep soon after until the expectations are
	// either fulfilled or timeout.
	adds, dels := 10, 30
	rc := newReplicationController(1)

	// RC fires off adds and deletes at apiserver, then sets expectations
	rcKey, err := KeyFunc(rc)
	require.NoError(t, err, "Couldn't get key for object %#v: %v", rc, err)

	err = e.SetExpectations(rcKey, adds, dels)
	assert.Nil(t, err)
	var wg sync.WaitGroup
	for i := 0; i < adds+1; i++ {
		wg.Add(1)
		go func() {
			// In prod this can happen either because of a failed create by the rc
			// or after having observed a create via informer
			e.CreationObserved(rcKey)
			wg.Done()
		}()
	}
	wg.Wait()

	// There are still delete expectations
	assert.False(t, e.SatisfiedExpectations(rcKey), "Rc will sync before expectations are met")

	for i := 0; i < dels+1; i++ {
		wg.Add(1)
		go func() {
			e.DeletionObserved(rcKey)
			wg.Done()
		}()
	}
	wg.Wait()

	tests := []struct {
		name                      string
		expectationsToSet         []int
		expireExpectations        bool
		wantPodExpectations       []int64
		wantExpectationsSatisfied bool
	}{
		{
			name:                      "Expectations have been surpassed",
			expireExpectations:        false,
			wantPodExpectations:       []int64{int64(-1), int64(-1)},
			wantExpectationsSatisfied: true,
		},
		{
			name:                      "Old expectations are cleared because of ttl",
			expectationsToSet:         []int{1, 2},
			expireExpectations:        true,
			wantPodExpectations:       []int64{int64(1), int64(2)},
			wantExpectationsSatisfied: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.expectationsToSet) > 0 {
				err = e.SetExpectations(rcKey, test.expectationsToSet[0], test.expectationsToSet[1])
				assert.Nil(t, err)
			}
			podExp, exists, err := e.GetExpectations(rcKey)
			require.NoError(t, err, "Could not get expectations for rc, exists %v and err %v", exists, err)
			assert.True(t, exists, "Could not get expectations for rc, exists %v and err %v", exists, err)

			add, del := podExp.GetExpectations()
			assert.Equal(t, test.wantPodExpectations[0], add, "Unexpected pod expectations %#v", podExp)
			assert.Equal(t, test.wantPodExpectations[1], del, "Unexpected pod expectations %#v", podExp)
			assert.Equal(t, test.wantExpectationsSatisfied, e.SatisfiedExpectations(rcKey), "Expectations are met but the rc will not sync")

			if test.expireExpectations {
				fakeClock.Step(ttl + 1)
				assert.True(t, e.SatisfiedExpectations(rcKey), "Expectations should have expired but didn't")
			}
		})
	}
}
