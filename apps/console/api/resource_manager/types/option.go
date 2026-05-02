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

package types

// ListOptions contains options for listing provisions.
type ListOptions struct {
	// ProvisionIDs filters by provision IDs.
	ProvisionIDs *[]string `json:"provisionIDs,omitempty"`

	// Regions filters by regions.
	Regions *[]RegionSpec `json:"regions,omitempty"`

	// Status filters by provision status.
	Status ProvisionStatus `json:"status,omitempty"`

	// Offset is the pagination offset.
	Offset int `json:"offset,omitempty"`

	// Limit limits the number of results.
	Limit int `json:"limit,omitempty"`
}
