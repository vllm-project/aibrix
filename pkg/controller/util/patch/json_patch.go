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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type JSONPatchOperation string

const (
	// If the target location specifies an object member that does exist, that member's value is replaced.
	Add JSONPatchOperation = "add"

	// The target location MUST exist for the operation to be successful.
	Replace JSONPatchOperation = "replace"

	// The target location MUST exist for the operation to be successful.
	Remove JSONPatchOperation = "remove"

	Move JSONPatchOperation = "move"
	Copy JSONPatchOperation = "copy"

	PathSeparator = "/"
)

type JSONPatch []JSONPatchItem

type JSONPatchItem struct {
	Operation JSONPatchOperation `json:"op"`
	Path      string             `json:"path"`
	Value     interface{}        `json:"value"`
}

func NewJSONPatch(items ...JSONPatchItem) *JSONPatch {
	res := make(JSONPatch, len(items))
	res = append(res, items...)
	return &res
}

func (jp *JSONPatch) Marshal() ([]byte, error) {
	return json.Marshal(jp)
}

func (jp *JSONPatch) ToClientPatch() (client.Patch, error) {
	bytes, err := json.Marshal(jp)
	if err != nil {
		return nil, err
	}
	return client.RawPatch(types.JSONPatchType, bytes), nil
}

func (jp *JSONPatch) Append(op JSONPatchOperation, path string, val interface{}) *JSONPatch {
	switch op {
	case Add, Replace:
		*jp = append(*jp, JSONPatchItem{
			Operation: op,
			Path:      path,
			Value:     val,
		})
	case Remove:
		*jp = append(*jp, JSONPatchItem{
			Operation: op,
			Path:      path,
		})
	default:
	}
	return jp
}

func (jp *JSONPatch) Join(patch JSONPatch) {
	if jp == nil {
		return
	}
	*jp = append(*jp, patch...)
}

func Join(patches ...JSONPatch) *JSONPatch {
	res := NewJSONPatch()
	for _, patch := range patches {
		res.Join(patch)
	}
	return res
}

func (jp JSONPatch) Len() int32 {
	if jp == nil {
		return 0
	}
	return int32(len(jp))
}

func (jp *JSONPatch) JsonPatch() (client.Patch, error) {
	bytes, err := jp.Marshal()
	if err != nil {
		return nil, err
	}

	return client.RawPatch(types.JSONPatchType, bytes), nil
}

func FixPathToValid(path string) string {
	return strings.Replace(path, "/", "~1", -1)
}
