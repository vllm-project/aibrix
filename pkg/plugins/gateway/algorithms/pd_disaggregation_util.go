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

package routingalgorithms

import (
	"os"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
)

func (r *pdRouter) startCounterPrinter() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			r.countersMu.RLock()
			if len(r.selectionCounts) == 0 {
				r.countersMu.RUnlock()
				continue
			}
			snapshot := make(map[string]int64, len(r.selectionCounts))
			for k, v := range r.selectionCounts {
				snapshot[k] = v
			}
			r.countersMu.RUnlock()

			printable := make(map[string]interface{}, len(snapshot))
			for k, v := range snapshot {
				printable[k] = float64(v)
			}

			utils.PrintMapTableAligned("pd_request_distribution", os.Getenv("POD_NAME"), printable)
		}
	}()
}
