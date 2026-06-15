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

package utils

import "time"

// TimeToUnix converts *time.Time to unix seconds, returning 0 for nil.
// Useful for converting nullable timestamps to protobuf int64 fields.
func TimeToUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

// UnixOrZero returns unix seconds for non-zero time.Time, or 0 for zero time.Time.
func UnixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// TimeOrZero dereferences *time.Time, returning zero value for nil.
// Useful for converting nullable DB timestamps to in-memory structs.
func TimeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// TimeToPtr converts time.Time to *time.Time, returning nil for zero values.
// Useful for converting in-memory timestamps to nullable DB fields.
func TimeToPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// UnixToTimePtr converts unix seconds to *time.Time, returning nil for zero/negative values.
// Useful for converting protobuf int64 timestamps to nullable DB fields.
func UnixToTimePtr(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}
