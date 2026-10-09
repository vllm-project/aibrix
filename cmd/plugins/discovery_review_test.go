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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadEtcdConfigPrefixAndAuth(t *testing.T) {
	for _, tc := range []struct{ name, body, prefix, wantError string }{
		{"prefix delimiter", "prefix: /workers", "/workers/", ""},
		{"root prefix", "prefix: /", "", "prefix"},
		{"blank prefix", "prefix: ' '", "", "prefix"},
		{"plaintext credentials", "username: gateway\npassword: secret", "", "encrypted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEtcdTestFile(t, t.TempDir(), "etcd.yaml", "endpoints: [http://localhost:2379]\n"+tc.body)
			config, err := loadEtcdConfig(path)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.prefix, config.Prefix)
		})
	}
}
