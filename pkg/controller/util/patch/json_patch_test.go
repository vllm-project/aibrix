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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
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
