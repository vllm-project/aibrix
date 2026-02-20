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

package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MemoryStore implements Store with in-memory storage.
type MemoryStore struct {
	mu          sync.RWMutex
	deployments []*pb.Deployment
	jobs        []*pb.Job
	models      []*pb.Model
	apiKeys     []*apiKeyEntry
	secrets     []*secretEntry
	quotas      []*pb.Quota
	nextID      int
}

type apiKeyEntry struct {
	key    *pb.APIKey
	secret string // full unmasked key
}

type secretEntry struct {
	secret *pb.Secret
	value  string
}

// NewMemoryStore creates a new in-memory store pre-populated with sample data.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{nextID: 100}
	s.seedData()
	return s
}

func (s *MemoryStore) genID() string {
	s.nextID++
	return fmt.Sprintf("%d", s.nextID)
}

func randomString(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[idx.Int64()]
	}
	return string(b)
}

// --- Deployments ---

func (s *MemoryStore) ListDeployments(_ context.Context, search string) ([]*pb.Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if search == "" {
		return s.deployments, nil
	}
	q := strings.ToLower(search)
	var result []*pb.Deployment
	for _, d := range s.deployments {
		if strings.Contains(strings.ToLower(d.Name), q) ||
			strings.Contains(strings.ToLower(d.BaseModel), q) ||
			strings.Contains(strings.ToLower(d.CreatedBy), q) {
			result = append(result, d)
		}
	}
	return result, nil
}

func (s *MemoryStore) GetDeployment(_ context.Context, id string) (*pb.Deployment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, d := range s.deployments {
		if d.Id == id {
			return d, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "deployment %q not found", id)
}

func (s *MemoryStore) CreateDeployment(_ context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	d := &pb.Deployment{
		Id:             s.genID(),
		Name:           req.Name,
		DeploymentId:   randomString(8),
		BaseModel:      req.BaseModel,
		BaseModelId:    strings.ToLower(strings.ReplaceAll(req.BaseModel, " ", "-")),
		Replicas:       fmt.Sprintf("%d", req.MinReplicas),
		GpusPerReplica: req.AcceleratorCount,
		GpuType:        req.AcceleratorType,
		Region:         req.Region,
		Status:         "Deploying",
	}
	s.deployments = append(s.deployments, d)
	return d, nil
}

func (s *MemoryStore) DeleteDeployment(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, d := range s.deployments {
		if d.Id == id {
			s.deployments = append(s.deployments[:i], s.deployments[i+1:]...)
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "deployment %q not found", id)
}

// --- Jobs ---

func (s *MemoryStore) ListJobs(_ context.Context, search, statusFilter string) ([]*pb.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*pb.Job
	for _, j := range s.jobs {
		if statusFilter != "" && !strings.EqualFold(j.Status, statusFilter) {
			continue
		}
		if search != "" {
			q := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(j.Name), q) &&
				!strings.Contains(strings.ToLower(j.InferenceId), q) &&
				!strings.Contains(strings.ToLower(j.CreatedBy), q) {
				continue
			}
		}
		result = append(result, j)
	}
	if result == nil {
		return s.jobs, nil
	}
	return result, nil
}

func (s *MemoryStore) GetJob(_ context.Context, id string) (*pb.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, j := range s.jobs {
		if j.Id == id {
			return j, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "job %q not found", id)
}

func (s *MemoryStore) CreateJob(_ context.Context, req *pb.CreateJobRequest) (*pb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	j := &pb.Job{
		Id:             s.genID(),
		Name:           req.DisplayName,
		InferenceId:    randomString(8),
		Model:          req.Model,
		ModelId:        strings.ToLower(strings.ReplaceAll(req.Model, " ", "-")),
		InputDataset:   req.DatasetId,
		InputDatasetId: req.DatasetId,
		CreateDate:     now.Format("Jan 02, 2006"),
		CreateTime:     now.Format("3:04 PM"),
		Status:         "Validating",
		FullPath:       fmt.Sprintf("accounts/aibrix/batchInference/%s", randomString(8)),
	}
	s.jobs = append(s.jobs, j)
	return j, nil
}

// --- Models ---

func (s *MemoryStore) ListModels(_ context.Context, search, category string) ([]*pb.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*pb.Model
	for _, m := range s.models {
		if category != "" {
			found := false
			for _, c := range m.Categories {
				if strings.EqualFold(c, category) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if search != "" {
			q := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(m.Name), q) &&
				!strings.Contains(strings.ToLower(m.Provider), q) {
				continue
			}
		}
		result = append(result, m)
	}
	if result == nil && search == "" && category == "" {
		return s.models, nil
	}
	return result, nil
}

func (s *MemoryStore) GetModel(_ context.Context, id string) (*pb.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, m := range s.models {
		if m.Id == id {
			return m, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "model %q not found", id)
}

// --- API Keys ---

func (s *MemoryStore) ListAPIKeys(_ context.Context) ([]*pb.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*pb.APIKey
	for _, e := range s.apiKeys {
		result = append(result, e.key)
	}
	return result, nil
}

func (s *MemoryStore) CreateAPIKey(_ context.Context, name string) (*pb.APIKey, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fullKey := "fw_" + randomString(24)
	key := &pb.APIKey{
		Id:        "key_" + randomString(16),
		Name:      name,
		SecretKey: fullKey[:10] + "...",
		CreatedAt: time.Now().Format("Jan 02, 2006 3:04 PM"),
	}
	s.apiKeys = append(s.apiKeys, &apiKeyEntry{key: key, secret: fullKey})
	return key, fullKey, nil
}

func (s *MemoryStore) DeleteAPIKey(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, e := range s.apiKeys {
		if e.key.Id == id {
			s.apiKeys = append(s.apiKeys[:i], s.apiKeys[i+1:]...)
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "api key %q not found", id)
}

// --- Secrets ---

func (s *MemoryStore) ListSecrets(_ context.Context, search string) ([]*pb.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*pb.Secret
	for _, e := range s.secrets {
		if search != "" && !strings.Contains(strings.ToLower(e.secret.Name), strings.ToLower(search)) {
			continue
		}
		result = append(result, e.secret)
	}
	if result == nil && search == "" {
		result = make([]*pb.Secret, 0, len(s.secrets))
		for _, e := range s.secrets {
			result = append(result, e.secret)
		}
	}
	return result, nil
}

func (s *MemoryStore) CreateSecret(_ context.Context, name, value string) (*pb.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sec := &pb.Secret{
		Id:   s.genID(),
		Name: name,
	}
	s.secrets = append(s.secrets, &secretEntry{secret: sec, value: value})
	return sec, nil
}

func (s *MemoryStore) DeleteSecret(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, e := range s.secrets {
		if e.secret.Id == id {
			s.secrets = append(s.secrets[:i], s.secrets[i+1:]...)
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "secret %q not found", id)
}

// --- Quotas ---

func (s *MemoryStore) ListQuotas(_ context.Context, search string) ([]*pb.Quota, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if search == "" {
		return s.quotas, nil
	}
	q := strings.ToLower(search)
	var result []*pb.Quota
	for _, quota := range s.quotas {
		if strings.Contains(strings.ToLower(quota.Name), q) ||
			strings.Contains(strings.ToLower(quota.QuotaId), q) {
			result = append(result, quota)
		}
	}
	return result, nil
}
