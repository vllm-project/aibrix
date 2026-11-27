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

package types

// SeededPrefixMatch is the raw prefix-index result computed while seeding a
// routing decision's candidates. Pods records coverage, including zero matches.
// Readers project Matches onto their own pod set and normalize scores there;
// scores normalized over the original fleet must not be reused.
//
// Index identifies the concrete index by pointer. Model and LoRAID identify its
// partition (the base model and adapter ID for KV sync). All fields are immutable
// after publication. Only candidate seeding publishes a result, so ordinary
// routing without seeding does not retain a fleet-sized snapshot.
// Index lookups must not mutate the coverage map, and a pod's raw match must
// be independent of which other pods were queried. Returned hashes are read-only;
// reuse limits their capacity to prevent append from changing retained storage.
type SeededPrefixMatch struct {
	Index   any
	Model   string
	LoRAID  int64
	Pods    map[string]struct{}
	Matches map[string]int
	Hashes  []uint64
}

// SeededPrefixMatch returns the current decision's immutable seeding result.
func (r *RoutingContext) SeededPrefixMatch() *SeededPrefixMatch {
	if r == nil {
		return nil
	}
	return r.seededPrefixMatch.Load()
}

// SetSeededPrefixMatch publishes a seeding result, or clears it when match is nil.
// The gateway clears it at both ends of each decision, including routing retries.
func (r *RoutingContext) SetSeededPrefixMatch(match *SeededPrefixMatch) {
	if r != nil {
		r.seededPrefixMatch.Store(match)
	}
}
