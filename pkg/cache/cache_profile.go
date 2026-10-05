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
package cache

import (
	"context"

	"k8s.io/klog/v2"
)

func (c *Store) updateDeploymentProfiles(ctx context.Context) {
	var cursor uint64
	var keys []string
	var err error
	for {
		select {
		case <-ctx.Done():
			klog.Errorf("quit update deployment profiles due to: %v", ctx.Err())
			return
		default:
		}

		keys, cursor, err = c.redisClient.Scan(ctx, cursor, "aibrix:profile_*", 10).Result() // 10 is the count, adjust as needed.  Use a larger count in production.
		if err != nil {
			klog.Errorf("failed to scan deploymet profileds due to: %v", err)
			return
		}

		for _, key := range keys {
			select {
			case <-ctx.Done():
				klog.Errorf("quit update deployment profiles due to: %v", ctx.Err())
				return
			default:
			}

			val, err := c.redisClient.Get(ctx, key).Bytes()
			if err != nil {
				klog.Warningf("error loading deployment profiles for %s: %v", key, err)
				continue // Skip to the next key
			}

			var updated ModelGPUProfile
			err = updated.Unmarshal(val)
			if err != nil {
				klog.Warningf("error unmarshalling deployment profiles for %s: %v, Value: %s", key, err, val)
				continue // Skip to the next key
			}

			c.UpdateModelProfile(key, &updated, false)
		}

		if cursor == 0 {
			break // No more keys to scan
		}
	}
}
