package models

import (
	"encoding/json"
	"reflect"
	"testing"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
)

func TestModelDeploymentTemplateSpecStoresRegionsInSpecJSON(t *testing.T) {
	regions := []string{
		"US-East/USEAST1/Federation/default",
		"US-West/USWEST2/Cloudnative/ai",
	}
	record := &ModelDeploymentTemplate{}
	err := record.FromPB(&pb.ModelDeploymentTemplate{
		Spec: &pb.ModelDeploymentTemplateSpec{
			Accelerator: &pb.AcceleratorSpec{
				Type:  "NVIDIA-L20",
				Count: 1,
			},
			Regions: regions,
		},
	})
	if err != nil {
		t.Fatalf("FromPB: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(record.Spec, &raw); err != nil {
		t.Fatalf("unmarshal stored spec: %v", err)
	}
	if _, ok := raw["accelerator"]; !ok {
		t.Fatal("stored spec is missing accelerator")
	}
	var storedRegions []string
	if err := json.Unmarshal(raw["regions"], &storedRegions); err != nil {
		t.Fatalf("unmarshal stored regions: %v", err)
	}
	if !reflect.DeepEqual(storedRegions, regions) {
		t.Fatalf("stored regions = %v, want %v", storedRegions, regions)
	}

	template, err := record.ToPB()
	if err != nil {
		t.Fatalf("ToPB: %v", err)
	}
	if template.GetSpec().GetAccelerator().GetType() != "NVIDIA-L20" {
		t.Fatalf("accelerator type = %q, want NVIDIA-L20", template.GetSpec().GetAccelerator().GetType())
	}
	if !reflect.DeepEqual(template.GetSpec().GetRegions(), regions) {
		t.Fatalf("regions = %v, want %v", template.GetSpec().GetRegions(), regions)
	}
}
