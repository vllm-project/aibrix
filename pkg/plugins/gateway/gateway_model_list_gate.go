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

package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// ModelListGate serves a 503 while the gateway cache is initially syncing,
// then delegates to the normal model-list handler. It is used only when
// discovery verification is explicitly enabled.
type ModelListGate struct {
	mu         sync.RWMutex
	server     *Server
	httpServer *http.Server
}

// SetServer publishes the ready gateway handler after cache initialization.
func (g *ModelListGate) SetServer(server *Server) {
	g.mu.Lock()
	g.server = server
	g.mu.Unlock()
}

// StartHTTPServer opens the model-list and metrics listener before discovery.
func (g *ModelListGate) StartHTTPServer(addr string) error {
	if g.httpServer != nil {
		return nil
	}
	server, err := startModelListHTTPServer(addr, g.handleListModels)
	if err != nil {
		return err
	}
	g.httpServer = server
	return nil
}

func (g *ModelListGate) handleListModels(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	server := g.server
	g.mu.RUnlock()
	if server != nil {
		server.handleListModels(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"error":"method not allowed"}`)
		return
	}
	writeModelListUnavailable(w)
}

func writeModelListUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, generateErrorMessageWithHTTPCode(
		"model discovery is unavailable", http.StatusServiceUnavailable,
		ErrorCodeServiceUnavailable, ""))
}

// Shutdown stops the early listener after gateway shutdown.
func (g *ModelListGate) Shutdown() {
	if g == nil || g.httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.httpServer.Shutdown(ctx); err != nil {
		klog.ErrorS(err, "Error stopping model-list HTTP server")
	}
}
