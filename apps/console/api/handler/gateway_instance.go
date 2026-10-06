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

package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/vllm-project/aibrix/apps/console/api/gateway/contract"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const gatewayInstancePath = "/api/v1/gateway-instances"

// GatewayInstanceHandler exposes the same namespaced resource operations for
// each provider. Authentication is checked here as well as by the server.
type GatewayInstanceHandler struct {
	provider contract.Provider
}

func NewGatewayInstanceHandler(provider contract.Provider) *GatewayInstanceHandler {
	return &GatewayInstanceHandler{provider: provider}
}

func (h *GatewayInstanceHandler) RegisterRoutes(mux *runtime.ServeMux) error {
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, gatewayInstancePath},
		{http.MethodGet, gatewayInstancePath + "/{name}"},
		{http.MethodPost, gatewayInstancePath},
		{http.MethodPut, gatewayInstancePath + "/{name}"},
	} {
		if err := mux.HandlePath(route.method, route.path, h.serve); err != nil {
			return fmt.Errorf("register %s %s: %w", route.method, route.path, err)
		}
	}
	return nil
}

func (h *GatewayInstanceHandler) serve(w http.ResponseWriter, r *http.Request, params map[string]string) {
	user := middleware.GetUser(r.Context())
	if user == nil {
		writeGatewayInstanceError(w, apierrors.NewUnauthorized("authentication is required"))
		return
	}
	if r.Method != http.MethodGet && !strings.EqualFold(strings.TrimSpace(user.Role), "admin") {
		writeGatewayInstanceJSON(w, http.StatusForbidden, map[string]string{"error": "admin role is required to manage Gateways"})
		return
	}
	if h.provider == nil {
		writeGatewayInstanceError(w, apierrors.NewServiceUnavailable("Gateway provider is not configured"))
		return
	}

	var result any
	var err error
	code := http.StatusOK
	switch r.Method {
	case http.MethodGet:
		if name := params["name"]; name != "" {
			result, err = h.provider.Get(r.Context(), name)
		} else {
			result, err = h.provider.List(r.Context())
		}
	case http.MethodPost, http.MethodPut:
		var request gatewayv1.Gateway
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err == nil {
			if decoder.Decode(new(any)) != io.EOF {
				err = fmt.Errorf("expected one JSON object")
			}
		}
		if err != nil {
			writeGatewayInstanceError(w, apierrors.NewBadRequest("invalid Gateway body: "+err.Error()))
			return
		}
		if r.Method == http.MethodPost {
			result, err = h.provider.Create(r.Context(), &request)
			code = http.StatusCreated
		} else {
			if request.Name != params["name"] {
				writeGatewayInstanceError(w, apierrors.NewBadRequest("metadata.name must match the URL"))
				return
			}
			result, err = h.provider.Update(r.Context(), &request)
		}
	}
	if err != nil {
		writeGatewayInstanceError(w, err)
		return
	}
	writeGatewayInstanceJSON(w, code, result)
}

func writeGatewayInstanceError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	message := err.Error()
	if apiStatus, ok := err.(apierrors.APIStatus); ok {
		code = int(apiStatus.Status().Code)
	} else {
		switch status.Code(err) {
		case codes.InvalidArgument:
			code = http.StatusBadRequest
		case codes.Unavailable:
			code = http.StatusServiceUnavailable
		}
	}
	if code < 400 || code > 599 {
		code = http.StatusInternalServerError
	}
	if code >= 500 {
		klog.Errorf("Gateway provider request failed: %v", err)
		message = "Gateway provider request failed"
	}
	writeGatewayInstanceJSON(w, code, map[string]string{"error": message})
}

func writeGatewayInstanceJSON(w http.ResponseWriter, code int, value any) {
	// client-go clears TypeMeta while decoding typed resources. Restore the
	// wire types so responses remain usable as native Kubernetes manifests.
	switch object := value.(type) {
	case *gatewayv1.Gateway:
		resource := object.DeepCopy()
		resource.APIVersion, resource.Kind = gatewayv1.GroupVersion.String(), "Gateway"
		value = resource
	case *gatewayv1.GatewayList:
		list := object.DeepCopy()
		list.APIVersion, list.Kind = gatewayv1.GroupVersion.String(), "GatewayList"
		for i := range list.Items {
			list.Items[i].APIVersion, list.Items[i].Kind = gatewayv1.GroupVersion.String(), "Gateway"
		}
		value = list
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		klog.Errorf("write Gateway response: %v", err)
	}
}
