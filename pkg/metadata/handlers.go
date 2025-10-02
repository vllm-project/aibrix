/*
Copyright 2024 The Aibrix Team.

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

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/klog/v2"
)

type httpServer struct {
	redisClient *redis.Client
	cache       cache.Cache
}

func NewHTTPServer(addr string, redis *redis.Client) *http.Server {
	c, err := cache.Get()
	if err != nil {
		panic(err)
	}

	server := &httpServer{
		redisClient: redis,
		cache:       c,
	}
	r := mux.NewRouter()
	// User related handlers
	r.HandleFunc("/CreateUser", server.createUser).Methods("POST")
	r.HandleFunc("/ReadUser", server.readUser).Methods("POST")
	r.HandleFunc("/UpdateUser", server.updateUser).Methods("POST")
	r.HandleFunc("/DeleteUser", server.deleteUser).Methods("POST")
	// OpenAI API related handlers
	r.HandleFunc("/v1/models", server.models).Methods("GET")

	r.HandleFunc("/view", server.view).Methods("POST") // Changed from GET to POST

	// Health related handlers
	r.HandleFunc("/healthz", server.healthz).Methods("GET")
	r.HandleFunc("/readyz", server.readyz).Methods("GET")

	return &http.Server{
		Addr:    addr,
		Handler: r,
	}
}

// models returns base and lora adapters registered to aibrix control plane
func (s *httpServer) models(w http.ResponseWriter, r *http.Request) {
	modelNames := s.cache.ListModels()
	response := BuildModelsResponse(modelNames)
	jsonBytes, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "error in processing model list", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "%s", string(jsonBytes))
}

func (s *httpServer) view(w http.ResponseWriter, r *http.Request) {
	var req ViewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Validate required parameters
	if req.RequestID == "" {
		klog.ErrorS(errors.New("missing requestID"), "missing required parameter: requestID")
		http.Error(w, "missing required parameter: requestID", http.StatusBadRequest)
		return
	}

	// Get RequestStore struct from Redis
	data, err := s.redisClient.Get(r.Context(), req.RequestID).Result()
	if err != nil {
		klog.ErrorS(err, "Failed to get request data")
		http.Error(w, fmt.Sprintf("Failed to get request data: %v", err), http.StatusInternalServerError)
		return
	}

	// Deserialize JSON data into RequestStore struct
	var requestStore types.RequestStore
	if err = json.Unmarshal([]byte(data), &requestStore); err != nil {
		klog.ErrorS(err, "Failed to deserialize RequestStore")
		http.Error(w, fmt.Sprintf("Failed to deserialize RequestStore: %v", err), http.StatusInternalServerError)
		return
	}

	// Use the deserialized struct
	reqPath := requestStore.Path
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}

	// Construct the download URL
	downloadURL := fmt.Sprintf("http://%s:%s/view%s", requestStore.IP, requestStore.Port, reqPath)
	klog.Infof("Attempting to download file from: %s", downloadURL)

	// Make request to download the model
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Get(downloadURL)
	if err != nil {
		klog.Errorf("Failed to download file from %s: %v", downloadURL, err)
		http.Error(w, fmt.Sprintf("failed to download file: %v", err), http.StatusInternalServerError)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		klog.Errorf("Download failed with status %d from %s", resp.StatusCode, downloadURL)
		http.Error(w, fmt.Sprintf("download failed with status: %d", resp.StatusCode), resp.StatusCode)
		return
	}

	// Set appropriate headers for file download
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		w.Header().Set("Content-Length", contentLength)
	}

	// Extract filename from path for Content-Disposition
	filename := filepath.Base(requestStore.Path)
	if filename == "." || filename == "/" {
		filename = "model_file"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))

	// Stream the file content to the response
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		klog.Errorf("Failed to stream file: %v", err)
		// Note: Can't send HTTP error here as headers are already written
		return
	}

	klog.Infof("Successfully downloaded file from %s", downloadURL)
}

func (s *httpServer) createUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v exists", u.Name)
		return
	}

	if err := utils.SetUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on creating user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Created User: %+v", u)
}

func (s *httpServer) readUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	user, err := utils.GetUser(r.Context(), u, s.redisClient)
	if err != nil {
		fmt.Fprint(w, "user does not exists")
		return
	}

	fmt.Fprintf(w, "User: %+v", user)
}

func (s *httpServer) updateUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := utils.SetUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on updating user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Updated User: %+v", u)
}

func (s *httpServer) deleteUser(w http.ResponseWriter, r *http.Request) {
	var u utils.User

	err := decodeJSONBody(w, r, &u)
	if err != nil {
		var mr *malformedRequest
		if errors.As(err, &mr) {
			http.Error(w, mr.msg, mr.status)
		} else {
			klog.Info(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	if !utils.CheckUser(r.Context(), u, s.redisClient) {
		fmt.Fprintf(w, "User: %+v does not exists", u.Name)
		return
	}

	if err := utils.DelUser(r.Context(), u, s.redisClient); err != nil {
		http.Error(w, fmt.Sprintf("error occurred on deleting user: %+v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Deleted User: %+v", u)
}

func (s *httpServer) healthz(w http.ResponseWriter, r *http.Request) {
	// Simple check to verify the service is running
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "healthy")
}

func (s *httpServer) readyz(w http.ResponseWriter, r *http.Request) {
	err := utils.CheckRedisHealth(r.Context(), s.redisClient)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ready")
}

// Add this struct definition near the top of the file after the httpServer struct
type ViewRequest struct {
	RequestID string `json:"request-id"`
}
