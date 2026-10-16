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

package benchmark

import "testing"

func TestNormalizeTOSS3Endpoint(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"https://tos-s3-cn-beijing.volces.com", "https://tos-s3-cn-beijing.volces.com"},
		{"https://tos-cn-beijing.volces.com", "https://tos-s3-cn-beijing.volces.com"},
		{"tos-cn-beijing.volces.com", "https://tos-s3-cn-beijing.volces.com"},
		{"https://tos-cn-shanghai.volces.com", "https://tos-s3-cn-shanghai.volces.com"},
		{"https://tosv.byted.org", "https://tosv.byted.org"},
		{"http://internal-tos.example:8080", "http://internal-tos.example:8080"},
	}
	for _, tc := range cases {
		got := normalizeTOSS3Endpoint(tc.in)
		if got != tc.want {
			t.Fatalf("normalizeTOSS3Endpoint(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseTOSURI(t *testing.T) {
	b, k, err := parseTOSURI("tos://bucket/path/to/obj")
	if err != nil || b != "bucket" || k != "path/to/obj" {
		t.Fatalf("got bucket=%q key=%q err=%v", b, k, err)
	}
	if _, _, err := parseTOSURI("s3://bucket/key"); err == nil {
		t.Fatal("expected error for non-tos scheme")
	}
}
