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

package impl

import (
	"reflect"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/vllm-project/aibrix/apps/console/api/common"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
)

func TestJobModelRoundTripPreservesTemplateRegionsWithoutMutatingRequestMetadata(t *testing.T) {
	requestMetadata := map[string]string{"user-key": "user-value"}
	job := &queuedJob{
		req: &plannerapi.EnqueueRequest{
			JobID: "job-region",
			ModelTemplate: &plannerapi.ModelTemplateRef{
				Name:    "template",
				Version: "v1",
				Regions: []string{"USWEST2", "USEAST1"},
			},
			BatchParams: openai.BatchNewParams{Metadata: requestMetadata},
		},
	}

	record := jobToModel(job)

	if _, ok := requestMetadata[common.MetadataConsoleTemplateRegions]; ok {
		t.Fatalf("request metadata contains internal region key: %v", requestMetadata)
	}

	restored := modelToJob(record)
	if !reflect.DeepEqual(restored.req.ModelTemplate.Regions, []string{"USWEST2", "USEAST1"}) {
		t.Fatalf("restored regions = %v, want [USWEST2 USEAST1]", restored.req.ModelTemplate.Regions)
	}
	if restored.req.BatchParams.Metadata["user-key"] != "user-value" {
		t.Fatalf("restored metadata = %v, want user-key preserved", restored.req.BatchParams.Metadata)
	}
	if _, ok := restored.req.BatchParams.Metadata[common.MetadataConsoleTemplateRegions]; ok {
		t.Fatalf("restored request metadata contains internal region key: %v", restored.req.BatchParams.Metadata)
	}
}
