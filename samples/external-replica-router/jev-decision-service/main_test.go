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

package main

import (
	"testing"
	"time"
)

func TestLoadConfigFrom(t *testing.T) {
	values := map[string]string{"JEV_ENDPOINT": "http://jev.example/v1/systemone"}
	got, err := loadConfigFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("loadConfigFrom() error = %v", err)
	}
	if got.listenAddr != ":8080" ||
		got.jevEndpoint != values["JEV_ENDPOINT"] ||
		got.jevTimeout != 10*time.Second ||
		got.routingInstructions != defaultRoutingInstructions {
		t.Fatalf("config = %#v", got)
	}

	values["LISTEN_ADDR"] = "127.0.0.1:9090"
	values["JEV_TIMEOUT"] = "3s"
	values["JEV_API_KEY"] = "secret"
	values["ROUTING_INSTRUCTIONS"] = "custom rule"
	got, err = loadConfigFrom(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("loadConfigFrom() error = %v", err)
	}
	if got.listenAddr != values["LISTEN_ADDR"] ||
		got.jevTimeout != 3*time.Second ||
		got.jevAPIKey != "secret" ||
		got.routingInstructions != "custom rule" {
		t.Fatalf("custom config = %#v", got)
	}
}

func TestLoadConfigFromRejectsInvalidValues(t *testing.T) {
	tests := []map[string]string{
		{},
		{"JEV_ENDPOINT": "://bad"},
		{"JEV_ENDPOINT": "file:///tmp/socket"},
		{"JEV_ENDPOINT": "http://user:pass@example.com/v1/systemone"},
		{"JEV_ENDPOINT": "http://example.com/v1/systemone#fragment"},
		{"JEV_ENDPOINT": "http://example.com/v1/systemone", "JEV_TIMEOUT": "0s"},
		{"JEV_ENDPOINT": "http://example.com/v1/systemone", "JEV_TIMEOUT": "soon"},
	}
	for _, values := range tests {
		if _, err := loadConfigFrom(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("loadConfigFrom(%v) error = nil", values)
		}
	}
}
