// Copyright 2025 The AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kvcache

import (
	"testing"
	"time"

	msgpack "github.com/shamaton/msgpack/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeEventBatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name        string
		setupBatch  func() []byte
		wantErr     bool
		errContains string
		validate    func(*testing.T, *EventBatch)
	}{
		{
			name: "successful decode with BlockStoredEvent",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":              string(EventTypeBlockStored),
							"timestamp":         now.Unix(),
							"block_hashes":      []interface{}{int64(123), int64(456)},
							"token_ids":         []interface{}{[]interface{}{int32(1), int32(2)}, []interface{}{int32(3), int32(4)}},
							"parent_block_hash": int64(100),
							"model_name":        "test-model",
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr: false,
			validate: func(t *testing.T, batch *EventBatch) {
				assert.Len(t, batch.Events, 1)
				event, ok := batch.Events[0].(*BlockStoredEvent)
				require.True(t, ok)
				assert.Equal(t, EventTypeBlockStored, event.Type)
				assert.Equal(t, now, event.Timestamp)
				assert.Equal(t, []int64{123, 456}, event.BlockHashes)
				assert.Len(t, event.TokenIDs, 2)
				assert.Equal(t, []int32{1, 2}, event.TokenIDs[0])
				assert.Equal(t, []int32{3, 4}, event.TokenIDs[1])
				assert.NotNil(t, event.ParentBlockHash)
				assert.Equal(t, int64(100), *event.ParentBlockHash)
				assert.Equal(t, "test-model", event.ModelName)
			},
		},
		{
			name: "successful decode with BlockRemovedEvent",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":         string(EventTypeBlockRemoved),
							"timestamp":    now.Unix(),
							"block_hashes": []interface{}{int64(789), int64(1011)},
							"model_name":   "test-model",
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr: false,
			validate: func(t *testing.T, batch *EventBatch) {
				assert.Len(t, batch.Events, 1)
				event, ok := batch.Events[0].(*BlockRemovedEvent)
				require.True(t, ok)
				assert.Equal(t, EventTypeBlockRemoved, event.Type)
				assert.Equal(t, now, event.Timestamp)
				assert.Equal(t, []int64{789, 1011}, event.BlockHashes)
				assert.Equal(t, "test-model", event.ModelName)
			},
		},
		{
			name: "successful decode with AllBlocksClearedEvent",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":       string(EventTypeAllCleared),
							"timestamp":  now.Unix(),
							"model_name": "test-model",
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr: false,
			validate: func(t *testing.T, batch *EventBatch) {
				assert.Len(t, batch.Events, 1)
				event, ok := batch.Events[0].(*AllBlocksClearedEvent)
				require.True(t, ok)
				assert.Equal(t, EventTypeAllCleared, event.Type)
				assert.Equal(t, now, event.Timestamp)
				assert.Equal(t, "test-model", event.ModelName)
			},
		},
		{
			name: "multiple events in batch",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":         string(EventTypeBlockStored),
							"timestamp":    now.Unix(),
							"block_hashes": []interface{}{int64(1)},
							"token_ids":    []interface{}{[]interface{}{int32(1)}},
							"model_name":   "model1",
						},
						map[string]interface{}{
							"type":         string(EventTypeBlockRemoved),
							"timestamp":    now.Add(time.Second).Unix(),
							"block_hashes": []interface{}{int64(2)},
							"model_name":   "model2",
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr: false,
			validate: func(t *testing.T, batch *EventBatch) {
				assert.Len(t, batch.Events, 2)
				_, ok1 := batch.Events[0].(*BlockStoredEvent)
				assert.True(t, ok1)
				_, ok2 := batch.Events[1].(*BlockRemovedEvent)
				assert.True(t, ok2)
			},
		},
		{
			name: "error on invalid msgpack",
			setupBatch: func() []byte {
				return []byte("invalid msgpack data")
			},
			wantErr:     true,
			errContains: "failed to unmarshal event batch",
		},
		{
			name: "error on missing events field",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"not_events": []interface{}{},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr:     true,
			errContains: "missing or invalid events field",
		},
		{
			name: "error on unknown event type",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":      "UNKNOWN_EVENT",
							"timestamp": now.Unix(),
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr:     true,
			errContains: "unknown event type: UNKNOWN_EVENT",
		},
		{
			name: "error on missing event type",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"timestamp": now.Unix(),
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr:     true,
			errContains: "missing event type",
		},
		{
			name: "timestamp as float64",
			setupBatch: func() []byte {
				batch := map[string]interface{}{
					"events": []interface{}{
						map[string]interface{}{
							"type":       string(EventTypeAllCleared),
							"timestamp":  float64(now.Unix()) + 0.5,
							"model_name": "test-model",
						},
					},
				}
				data, _ := msgpack.Marshal(batch)
				return data
			},
			wantErr: false,
			validate: func(t *testing.T, batch *EventBatch) {
				assert.Len(t, batch.Events, 1)
				event, ok := batch.Events[0].(*AllBlocksClearedEvent)
				require.True(t, ok)
				// Should parse fractional seconds
				assert.Equal(t, now.Add(500*time.Millisecond), event.Timestamp)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.setupBatch()
			batch, err := DecodeEventBatch(data)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, batch)
				if tt.validate != nil {
					tt.validate(t, batch)
				}
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name        string
		input       interface{}
		want        time.Time
		wantErr     bool
		errContains string
	}{
		{
			name:  "time.Time",
			input: now,
			want:  now,
		},
		{
			name:  "int64 seconds",
			input: now.Unix(),
			want:  now,
		},
		{
			name:  "float64 with fractional seconds",
			input: float64(now.Unix()) + 0.123,
			want:  now.Add(123 * time.Millisecond).Truncate(time.Microsecond),
		},
		{
			name:  "RFC3339 string",
			input: now.Format(time.RFC3339),
			want:  now,
		},
		{
			name:        "unsupported type",
			input:       []byte("not a timestamp"),
			wantErr:     true,
			errContains: "unsupported timestamp type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTimestamp(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				// For float64 timestamps, allow microsecond-level tolerance due to precision
				if tt.name == "float64 with fractional seconds" {
					assert.WithinDuration(t, tt.want, got, time.Microsecond,
						"timestamp should be within 1 microsecond")
				} else {
					assert.Equal(t, tt.want, got)
				}
			}
		})
	}
}

func TestParseInt64Array(t *testing.T) {
	tests := []struct {
		name        string
		input       interface{}
		want        []int64
		wantErr     bool
		errContains string
	}{
		{
			name:  "valid int64 array",
			input: []interface{}{int64(1), int64(2), int64(3)},
			want:  []int64{1, 2, 3},
		},
		{
			name:  "mixed number types",
			input: []interface{}{int(1), int32(2), float64(3.0), uint64(4)},
			want:  []int64{1, 2, 3, 4},
		},
		{
			name:  "empty array",
			input: []interface{}{},
			want:  []int64{},
		},
		{
			name:        "not an array",
			input:       "not an array",
			wantErr:     true,
			errContains: "expected array",
		},
		{
			name:        "invalid element type",
			input:       []interface{}{int64(1), "not a number"},
			wantErr:     true,
			errContains: "failed to parse element at index 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInt64Array(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestParseInt32Array(t *testing.T) {
	tests := []struct {
		name        string
		input       interface{}
		want        []int32
		wantErr     bool
		errContains string
	}{
		{
			name:  "valid int32 array",
			input: []interface{}{int32(1), int32(2), int32(3)},
			want:  []int32{1, 2, 3},
		},
		{
			name:  "mixed number types",
			input: []interface{}{int(1), int64(2), float64(3.0)},
			want:  []int32{1, 2, 3},
		},
		{
			name:  "empty array",
			input: []interface{}{},
			want:  []int32{},
		},
		{
			name:        "not an array",
			input:       123,
			wantErr:     true,
			errContains: "expected array",
		},
		{
			name:        "invalid element type",
			input:       []interface{}{int32(1), []int{2}},
			wantErr:     true,
			errContains: "unsupported int32 type at index 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInt32Array(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
