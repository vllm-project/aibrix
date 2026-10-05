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

package pd

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EngineTRTLLM is the TensorRT-LLM engine identifier. It is defined here so
// that packages below routingalgorithms (prefill, engine) can special-case
// TRT-LLM without importing routingalgorithms (circular).
const EngineTRTLLM = "trtllm"

// JSONEditor chains sjson operations on a JSON body and records the first
// error. sjson.SetBytes / DeleteBytes return new slices, so the body passed to
// NewJSONEditor is never mutated.
//
// PD request/response mutation must go through byte-level edits like these
// instead of a map[string]any round trip: sonic re-serialises nested objects
// (messages, tools, tool parameter schemas) in random key order, which makes
// prompt_token_ids unstable across requests and between the prefill and
// decode bodies of the same request, defeating KV-cache prefix reuse.
type JSONEditor struct {
	body []byte
	err  error
}

// NewJSONEditor starts an edit chain on body.
func NewJSONEditor(body []byte) *JSONEditor {
	return &JSONEditor{body: body}
}

// Set writes value at path. A nil value is written as JSON null.
func (e *JSONEditor) Set(path string, value any) *JSONEditor {
	if e.err != nil {
		return e
	}
	e.body, e.err = sjson.SetBytes(e.body, path, value)
	if e.err != nil {
		e.err = fmt.Errorf("failed to set %s: %w", path, e.err)
	}
	return e
}

// SetRaw writes a pre-encoded JSON fragment at path without re-encoding it.
func (e *JSONEditor) SetRaw(path string, raw []byte) *JSONEditor {
	if e.err != nil {
		return e
	}
	e.body, e.err = sjson.SetRawBytes(e.body, path, raw)
	if e.err != nil {
		e.err = fmt.Errorf("failed to set %s: %w", path, e.err)
	}
	return e
}

// Delete removes path; deleting a missing path is a no-op.
func (e *JSONEditor) Delete(path string) *JSONEditor {
	if e.err != nil {
		return e
	}
	e.body, e.err = sjson.DeleteBytes(e.body, path)
	if e.err != nil {
		e.err = fmt.Errorf("failed to delete %s: %w", path, e.err)
	}
	return e
}

// Result returns the edited body, or nil and the first recorded error.
func (e *JSONEditor) Result() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.body, nil
}

// ValidateJSONObject rejects bodies that are not syntactically valid JSON or
// whose top-level value is not an object, so the sjson edits applied
// afterwards are well-defined. gjson.ValidBytes alone accepts arrays/scalars
// and gjson.ParseBytes on malformed input yields Null without an error, hence
// both checks. what names the body in the error message.
func ValidateJSONObject(body []byte, what string) error {
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("%s is not valid JSON", what)
	}
	if root := gjson.ParseBytes(body); !root.IsObject() {
		return fmt.Errorf("%s is not a JSON object (got %s)", what, root.Type.String())
	}
	return nil
}

// CommonControlledFields lists the top-level keys ApplyPrefillControlFields
// sets or deletes on every engine's prefill body. Engine handlers and KV
// transfer agents add their own keys on top (see ControlledFields).
var CommonControlledFields = []string{
	"max_tokens",
	"max_completion_tokens",
	"stream",
	"stream_options",
	"min_tokens",
}

// FindDuplicateTopLevelKey scans the root object of body once and returns the
// first entry of controlledFields (in slice order) that occurs more than once
// at the top level, or "" and false when there is none. body must already be
// a valid JSON object (see ValidateJSONObject).
//
// sjson edits only the first occurrence of a key, so a duplicated controlled
// key would let the client's second value survive and override whatever the
// gateway wrote; callers reject such bodies instead.
func FindDuplicateTopLevelKey(body []byte, controlledFields []string) (string, bool) {
	controlled := make(map[string]bool, len(controlledFields))
	for _, f := range controlledFields {
		controlled[f] = true
	}
	counts := make(map[string]int, len(controlledFields))
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if k := key.String(); controlled[k] {
			counts[k]++
		}
		return true
	})
	for _, f := range controlledFields {
		if counts[f] > 1 {
			return f, true
		}
	}
	return "", false
}

// ApplyPrefillControlFields overrides the generation control fields so the
// prefill pod returns right after filling the KV cache:
//   - max_tokens=1 and max_completion_tokens=1 (TRT-LLM only supports
//     max_tokens, so max_completion_tokens is removed instead)
//   - stream=false, stream_options and min_tokens removed
//
// Only top-level keys are touched; every other byte of body is preserved.
func ApplyPrefillControlFields(body []byte, llmEngine string) ([]byte, error) {
	e := NewJSONEditor(body).Set("max_tokens", 1)
	if llmEngine == EngineTRTLLM {
		e.Delete("max_completion_tokens")
	} else {
		e.Set("max_completion_tokens", 1)
	}
	return e.Set("stream", false).
		Delete("stream_options").
		Delete("min_tokens").
		Result()
}
