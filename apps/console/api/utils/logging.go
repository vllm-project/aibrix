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

package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"k8s.io/klog/v2"
)

const maxLoggedBodyBytes = 8192

// loggingTransport dumps every request and response between the BFF and MDS
// at klog -v=2 (verbose). Errors and non-2xx responses always log at info.
// Enable with `--v=2` (or env KLOG_V=2) when debugging the OpenAI SDK payloads.
type LoggingTransport struct {
	Base  http.RoundTripper
	Label string
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	label := t.Label
	if label == "" {
		label = "HTTP"
	}

	verbose := klog.V(2).Enabled()

	var reqBody []byte
	if verbose && req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body for logging: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	if verbose {
		klog.V(2).Infof("[%s] %s %s\n%s", label, req.Method, req.URL.String(), prettyBody(reqBody))
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		klog.Warningf("[%s] %s %s ERROR %v", label, req.Method, req.URL.String(), err)
		return resp, err
	}

	if resp.StatusCode >= 400 || verbose {
		respBody, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			klog.Warningf("[%s] %s %s -> %d (read body failed: %v)",
				label, req.Method, req.URL.String(), resp.StatusCode, rerr)
			return resp, nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if resp.StatusCode >= 400 {
			klog.Warningf("[%s] %s %s -> %d\n%s", label, req.Method, req.URL.String(), resp.StatusCode, prettyBody(respBody))
		} else {
			klog.V(2).Infof("[%s] %s %s -> %d\n%s", label, req.Method, req.URL.String(), resp.StatusCode, prettyBody(respBody))
		}
	}
	return resp, nil
}

// prettyBody indents JSON for readability and truncates oversized bodies.
// Non-JSON bodies are returned as-is (also truncated).
func prettyBody(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, b, "", "  "); err == nil {
		out := pretty.Bytes()
		if len(out) > maxLoggedBodyBytes {
			return string(out[:maxLoggedBodyBytes]) + "\n...(truncated)"
		}
		return string(out)
	}
	if len(b) > maxLoggedBodyBytes {
		return string(b[:maxLoggedBodyBytes]) + "...(truncated)"
	}
	return string(b)
}
