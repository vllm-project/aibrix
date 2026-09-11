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

package e2eframework

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// MockRequestRecord is a JSON-compatible request record returned by the mock app.
type MockRequestRecord struct {
	Sequence      int             `json:"sequence"`
	RequestID     string          `json:"request_id"`
	Path          string          `json:"path"`
	RawBodyBase64 string          `json:"raw_body_base64"`
	ParsedJSON    json.RawMessage `json:"parsed_json"`
	Pod           string          `json:"pod"`
	Engine        string          `json:"engine"`
	Role          string          `json:"role"`
	Outcome       string          `json:"outcome"`
	StatusCode    int             `json:"status_code"`
	Response      json.RawMessage `json:"response"`
}

// DecodeMockRequestRecords decodes and orders records returned by the mock app.
func DecodeMockRequestRecords(body []byte) ([]MockRequestRecord, error) {
	var records []MockRequestRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decode mock request records: %w", err)
	}

	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Sequence < records[j].Sequence
	})
	return records, nil
}

// SelectSuccessfulPDLegs returns the unique successful prefill and decode legs.
//
// The contract argument is retained for caller compatibility, but PR2 does not
// emit a contract in recorder records. Contract validation belongs to the
// caller's mock configuration and cannot be performed from this endpoint.
func SelectSuccessfulPDLegs(records []MockRequestRecord, requestID, contract, engine string) (prefill, decode MockRequestRecord, err error) {
	var prefills, decodes []MockRequestRecord
	for _, record := range records {
		if record.RequestID != requestID ||
			record.Engine != engine ||
			record.Outcome != "success" ||
			record.StatusCode != http.StatusOK {
			continue
		}

		switch record.Role {
		case "prefill":
			prefills = append(prefills, record)
		case "decode":
			decodes = append(decodes, record)
		}
	}

	if len(prefills) != 1 || len(decodes) != 1 {
		return MockRequestRecord{}, MockRequestRecord{}, fmt.Errorf(
			"expected exactly one successful prefill and decode leg, got %d prefill and %d decode",
			len(prefills), len(decodes),
		)
	}
	return prefills[0], decodes[0], nil
}
