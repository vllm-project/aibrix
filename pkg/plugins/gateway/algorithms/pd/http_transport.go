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

package pd

import (
	"net/http"
	"time"
)

// DefaultHTTPMaxIdleConns, DefaultHTTPMaxIdleConnsPerHost, and
// DefaultHTTPIdleConnTimeout size the shared outbound HTTP connection pools
// used for PD prefill, EPD encode, and decode-abort calls so high-concurrency
// multi-pod deployments reuse TCP connections instead of churning sockets.
const (
	DefaultHTTPMaxIdleConns        = 1024
	DefaultHTTPMaxIdleConnsPerHost = 128
	DefaultHTTPIdleConnTimeout     = 90 * time.Second
)

// NewHTTPTransport returns an *http.Transport configured with the PD/EPD
// outbound connection-pool defaults.
func NewHTTPTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        DefaultHTTPMaxIdleConns,
		MaxIdleConnsPerHost: DefaultHTTPMaxIdleConnsPerHost,
		IdleConnTimeout:     DefaultHTTPIdleConnTimeout,
	}
}
