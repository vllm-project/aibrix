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

package patch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestJSONPatch_NewAppendLen(t *testing.T) {
	p := NewJSONPatch()
	// initial len should be 0
	assert.Equal(t, int32(0), p.Len())

	p.Append(Add, "/metadata/name", "foo")
	p.Append(Remove, "/spec/replicas", nil)
	assert.Equal(t, int32(2), p.Len())
}

func TestJSONPatch_MarshalAndToClientPatch(t *testing.T) {
	p := NewJSONPatch(JSONPatchItem{Operation: Replace, Path: "/a", Value: "b"})
	bytes, err := p.Marshal()
	assert.NoError(t, err)
	// ensure valid JSON array
	assert.True(t, json.Valid(bytes))
	assert.True(t, strings.HasPrefix(string(bytes), "["))

	patch, err := p.ToClientPatch()
	assert.NoError(t, err)
	assert.Equal(t, types.JSONPatchType, patch.Type())
}

func TestJSONPatch_Join(t *testing.T) {
	p1 := NewJSONPatch().Append(Add, "/a", 1)
	p2 := NewJSONPatch().Append(Add, "/b", 2).Append(Remove, "/c", nil)
	joined := Join(*p1, *p2)
	assert.Equal(t, int32(3), joined.Len())
}

func TestFixPathToValid(t *testing.T) {
	in := "/metadata/labels/app"
	out := FixPathToValid(in)
	assert.Equal(t, "~1metadata~1labels~1app", out)
}

func TestJSONPatch_JsonPatch(t *testing.T) {
	tests := []struct {
		name        string
		setupPatch  func() *JSONPatch
		wantPatch   client.Patch
		wantErr     bool
		expectedErr error
	}{
		{
			name: "successful json patch creation",
			setupPatch: func() *JSONPatch {
				return &JSONPatch{
					JSONPatchItem{
						Operation: "add",
						Path:      "/metadata/labels/test",
						Value:     "value",
					},
				}
			},
			wantPatch: client.RawPatch(types.JSONPatchType, []byte(`[{"op":"add","path":"/metadata/labels/test","value":"value"}]`)),
			wantErr:   false,
		},
		{
			name: "empty json patch",
			setupPatch: func() *JSONPatch {
				return &JSONPatch{}
			},
			wantPatch: client.RawPatch(types.JSONPatchType, []byte(`[]`)),
			wantErr:   false,
		},
		{
			name: "multiple operations in patch",
			setupPatch: func() *JSONPatch {
				return &JSONPatch{
					JSONPatchItem{
						Operation: "add",
						Path:      "/metadata/labels/test1",
						Value:     "value1",
					},
					JSONPatchItem{
						Operation: "remove",
						Path:      "/metadata/labels/test2",
					},
				}
			},
			// Note: our implementation didn't follow exact RFC 6902 which doesn't include a value for remove operations.
			// The output is still acceptable in kubernetes, but this should be improved in the future.
			wantPatch: client.RawPatch(types.JSONPatchType, []byte(`[{"op":"add","path":"/metadata/labels/test1","value":"value1"},{"op":"remove","path":"/metadata/labels/test2","value":null}]`)),
			wantErr:   false,
		},
		{
			name: "error during marshaling",
			setupPatch: func() *JSONPatch {
				// Create a patch with an invalid value that can't be marshaled
				return &JSONPatch{
					JSONPatchItem{
						Operation: "add",
						Path:      "/metadata/labels/test",
						Value:     make(chan int), // channels can't be marshaled to JSON
					},
				}
			},
			wantErr:     true,
			expectedErr: errors.New("json: unsupported type: chan int"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jp := tt.setupPatch()

			gotPatch, err := jp.JsonPatch()

			if tt.wantErr {
				require.Error(t, err)
				if tt.expectedErr != nil {
					assert.Contains(t, err.Error(), tt.expectedErr.Error())
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantPatch.Type(), gotPatch.Type())

			// Compare the raw data of the patches
			rawWant, err := tt.wantPatch.Data(nil)
			require.NoError(t, err)

			rawGot, err := gotPatch.Data(nil)
			require.NoError(t, err)

			// Ensure the raw data is the same
			assert.JSONEq(t, string(rawWant), string(rawGot))
			assert.Equal(t, rawWant, rawGot)
		})
	}
}
