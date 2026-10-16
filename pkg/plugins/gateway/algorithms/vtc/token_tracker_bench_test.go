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

package vtc

import (
	"context"
	"fmt"
	"testing"
)

// The same benchmark runs on the base and fix. Each operation mirrors the
// tracker's three reads in VTC scoring; no buckets expire during measurement.
func BenchmarkTokenTrackerRoutingReads(b *testing.B) {
	for _, users := range []int{128, 4096} {
		b.Run(fmt.Sprint(users), func(b *testing.B) {
			config := DefaultVTCConfig()
			tracker := NewInMemorySlidingWindowTokenTracker(&config, WithWindowSize(60), WithTimeUnit(Minutes))
			ctx := context.Background()
			for i := 0; i < users; i++ {
				if err := tracker.UpdateTokenCount(ctx, fmt.Sprint(i), float64(i+1), 0); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := tracker.GetTokenCount(ctx, "0"); err != nil {
					b.Fatal(err)
				}
				if _, err := tracker.GetMinTokenCount(ctx); err != nil {
					b.Fatal(err)
				}
				if _, err := tracker.GetMaxTokenCount(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
