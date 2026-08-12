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

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteStatusSetsJSONContentType(t *testing.T) {
	recorder := httptest.NewRecorder()

	writeStatus(recorder, http.StatusNotFound, "NotFound", "metric path not found")

	if got := recorder.Code; got != http.StatusNotFound {
		t.Fatalf("status code = %d, want %d", got, http.StatusNotFound)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}
