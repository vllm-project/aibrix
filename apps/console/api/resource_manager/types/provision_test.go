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

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProvisionResultToProvisionRecordRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	publicIP := "1.2.3.4"

	input := &ProvisionResult{
		ProvisionID:    "prov-123",
		IdempotencyKey: "idem-123",
		Status:         ProvisionStatusRunning,
		CreatedAt:      now,
		UpdatedAt:      now,
		AWS: &AWSProvisionDetail{
			Region: AWSRegion{Region: "us-east-1"},
			GroupResults: []AWSGroupResult{
				{
					Replicas: 1,
					Instances: []AWSInstanceDetail{
						{InstanceId: "i-1", InstanceType: "p5.48xlarge", State: "running", PublicIp: &publicIP},
					},
				},
			},
		},
	}

	record, err := input.ToProvisionRecord()
	if err != nil {
		t.Fatalf("ToProvisionRecord() unexpected error: %v", err)
	}

	if record.ProvisionID != input.ProvisionID {
		t.Fatalf("ProvisionID mismatch: got %q want %q", record.ProvisionID, input.ProvisionID)
	}
	if record.Status != string(input.Status) {
		t.Fatalf("Status mismatch: got %q want %q", record.Status, input.Status)
	}
	if record.Deleted {
		t.Fatalf("Deleted should be false")
	}
	if len(record.Payload) == 0 {
		t.Fatalf("Payload should not be empty")
	}
	if !json.Valid(record.Payload) {
		t.Fatalf("Payload should be valid JSON")
	}

	roundTrip, err := record.ToProvisionResult()
	if err != nil {
		t.Fatalf("ToProvisionResult() unexpected error: %v", err)
	}

	if roundTrip.ProvisionID != input.ProvisionID {
		t.Fatalf("ProvisionID mismatch after round-trip: got %q want %q", roundTrip.ProvisionID, input.ProvisionID)
	}
	if roundTrip.Status != input.Status {
		t.Fatalf("Status mismatch after round-trip: got %q want %q", roundTrip.Status, input.Status)
	}
	if roundTrip.UpdatedAt != record.UpdatedAt {
		t.Fatalf("UpdatedAt mismatch after round-trip: got %v want %v", roundTrip.UpdatedAt, record.UpdatedAt)
	}
	if roundTrip.AWS == nil || roundTrip.AWS.Region.Region != "us-east-1" {
		t.Fatalf("AWS detail missing after round-trip")
	}
}

func TestProvisionRecordToProvisionResultErrors(t *testing.T) {
	tests := []struct {
		name    string
		record  ProvisionRecord
		wantErr string
	}{
		{
			name: "nil payload",
			record: ProvisionRecord{
				ProvisionID: "prov-1",
				Status:      string(ProvisionStatusPending),
				Payload:     nil,
			},
			wantErr: "provision payload is nil",
		},
		{
			name: "invalid payload",
			record: ProvisionRecord{
				ProvisionID: "prov-2",
				Status:      string(ProvisionStatusPending),
				Payload:     []byte("{invalid-json"),
			},
			wantErr: "unmarshal provision payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.record.ToProvisionResult()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error mismatch: got %q, want contains %q", err.Error(), tt.wantErr)
			}
		})
	}
}
