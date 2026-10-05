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

package pd

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/types"
)

// TestDecodeWatchdogTimeoutEnvParsing pins the loader, which deliberately
// differs from utils.LoadEnvInt: 0 must get through (it is the documented off
// switch), while a negative or unparseable value is a typo and falls back.
func TestDecodeWatchdogTimeoutEnvParsing(t *testing.T) {
	const (
		firstKey    = "AIBRIX_DECODE_FIRST_RESPONSE_TIMEOUT"
		responseKey = "AIBRIX_DECODE_RESPONSE_TIMEOUT"
	)
	unset := func(t *testing.T, key string) {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}

	t.Run("defaults", func(t *testing.T) {
		unset(t, firstKey)
		unset(t, responseKey)
		got := loadDecodeWatchdogTimeouts()
		assert.Equal(t, 60*time.Second, got.FirstResponseTimeout)
		// No universal value can be safe when the wait covers a whole
		// generation, so the non-streaming budget ships disabled.
		assert.Equal(t, time.Duration(0), got.ResponseTimeout)
	})

	cases := []struct {
		name     string
		raw      string
		expected time.Duration
	}{
		{"empty", "", 60 * time.Second},
		{"zero disables", "0", 0},
		{"positive", "30", 30 * time.Second},
		{"negative falls back", "-1", 60 * time.Second},
		{"garbage falls back", "abc", 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(firstKey, tc.raw)
			assert.Equal(t, tc.expected, loadDecodeWatchdogTimeouts().FirstResponseTimeout)
		})
	}

	t.Run("response timeout", func(t *testing.T) {
		t.Setenv(responseKey, "180")
		assert.Equal(t, 180*time.Second, loadDecodeWatchdogTimeouts().ResponseTimeout)
	})
}

// TestAbortDecodeLegOnWatchdogSkips: an abort that cannot be sent is logged
// and counted as skipped, and still releases the leg's abort join point so
// nothing waiting on it hangs.
func TestAbortDecodeLegOnWatchdogSkips(t *testing.T) {
	cases := []struct {
		name       string
		rid        string
		decodeAddr string
		timeout    time.Duration
	}{
		{"no rid", "", "10.0.0.1:8000", time.Second},
		{"disabled", "rid-1", "10.0.0.1:8000", 0},
		{"no target", "rid-1", "", time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "pd", "model", "message", "req-watchdog", "user")
			t.Cleanup(ctx.Delete)
			if tc.rid != "" {
				ctx.SetPDRequestID(tc.rid)
			}
			ctx.SetDecodeTarget(tc.decodeAddr, "decode-1")
			overrides := *types.DefaultPDOverrides()
			overrides.Abort.Timeout = tc.timeout
			ctx.SetPDOverrides(&overrides)
			leg := ctx.PDLeg()

			// A client whose transport fails the test if it is ever used.
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("a skipped abort must not reach the decode pod")
				return nil, context.Canceled
			})}
			AbortDecodeLegOnWatchdog(client, leg, "req-watchdog", "model", AbortTriggerWatchdogFirstResponse)

			select {
			case <-leg.AbortDone():
			case <-time.After(time.Second):
				t.Fatal("a skipped abort must release the join point")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
