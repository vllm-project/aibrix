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
	"strconv"
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
	jobs        map[string]*pb.Job // id → Console-side fields only
	models      []*pb.Model
	templates   []*pb.ModelDeploymentTemplate
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

// NewMemoryStore creates a new in-memory store pre-populated with demo data.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{nextID: 100}
	s.loadDemoData()
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

// --- Jobs (Console-side fields only) ---

func (s *MemoryStore) UpsertJob(_ context.Context, job *pb.Job) error {
	if job == nil || job.Id == "" {
		return status.Error(codes.InvalidArgument, "job id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]*pb.Job{}
	}
	// Persist only Console-owned fields; ignore OpenAI Batch state on write.
	s.jobs[job.Id] = &pb.Job{
		Id:        job.Id,
		Name:      job.Name,
		CreatedBy: job.CreatedBy,
	}
	return nil
}

func (s *MemoryStore) GetJob(_ context.Context, id string) (*pb.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, nil
	}
	// Return a fresh struct so callers can mutate it freely.
	return &pb.Job{Id: j.Id, Name: j.Name, CreatedBy: j.CreatedBy}, nil
}

func (s *MemoryStore) ListJobs(_ context.Context, ids []string) (map[string]*pb.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*pb.Job, len(ids))
	for _, id := range ids {
		if j, ok := s.jobs[id]; ok {
			out[id] = &pb.Job{Id: j.Id, Name: j.Name, CreatedBy: j.CreatedBy}
		}
	}
	return out, nil
}

func (s *MemoryStore) DeleteJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
	return nil
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
			providerName := ""
			if m.Metadata != nil {
				providerName = m.Metadata.ProviderName
			}
			if !strings.Contains(strings.ToLower(m.Name), q) &&
				!strings.Contains(strings.ToLower(providerName), q) {
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

// --- Model Deployment Templates ---

// uuidV4 returns an RFC 4122 v4 UUID. Local helper to avoid an extra dependency.
func uuidV4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read on crypto/rand only fails if the OS RNG is misconfigured;
		// fall back to a timestamp-derived string so we never crash the server.
		return fmt.Sprintf("uuid-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *MemoryStore) ListModelDeploymentTemplates(_ context.Context, modelID, statusFilter, name string) ([]*pb.ModelDeploymentTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*pb.ModelDeploymentTemplate, 0)
	for _, t := range s.templates {
		if modelID != "" && t.ModelId != modelID {
			continue
		}
		if statusFilter != "" && !strings.EqualFold(t.Status, statusFilter) {
			continue
		}
		if name != "" && t.Name != name {
			continue
		}
		result = append(result, t)
	}
	return result, nil
}

func (s *MemoryStore) GetModelDeploymentTemplate(_ context.Context, modelID, id string) (*pb.ModelDeploymentTemplate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.templates {
		if t.Id == id && t.ModelId == modelID {
			return t, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "deployment template %q not found under model %q", id, modelID)
}

func (s *MemoryStore) CreateModelDeploymentTemplate(_ context.Context, req *pb.CreateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	version := req.GetVersion()
	if version == "" {
		version = "v1.0.0"
	}
	templateStatus := req.GetStatus()
	if templateStatus == "" {
		templateStatus = "active"
	}

	for _, t := range s.templates {
		if t.ModelId == req.GetModelId() && t.Name == req.GetName() && t.Version == version {
			return nil, status.Errorf(codes.AlreadyExists, "template %q@%q already exists for model %q", req.GetName(), version, req.GetModelId())
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	t := &pb.ModelDeploymentTemplate{
		Id:        uuidV4(),
		Name:      req.GetName(),
		Version:   version,
		Status:    templateStatus,
		ModelId:   req.GetModelId(),
		Spec:      req.GetSpec(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.templates = append(s.templates, t)
	return t, nil
}

func (s *MemoryStore) UpdateModelDeploymentTemplate(_ context.Context, req *pb.UpdateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if req.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.templates {
		if t.Id != req.GetId() || t.ModelId != req.GetModelId() {
			continue
		}
		newName := t.Name
		if req.GetName() != "" {
			newName = req.GetName()
		}
		newVersion := t.Version
		if req.GetVersion() != "" {
			newVersion = req.GetVersion()
		}
		if newName != t.Name || newVersion != t.Version {
			for _, other := range s.templates {
				if other.Id == t.Id {
					continue
				}
				if other.ModelId == t.ModelId && other.Name == newName && other.Version == newVersion {
					return nil, status.Errorf(codes.AlreadyExists, "template %q@%q already exists for model %q", newName, newVersion, t.ModelId)
				}
			}
		}
		t.Name = newName
		t.Version = newVersion
		if req.GetStatus() != "" {
			t.Status = req.GetStatus()
		}
		if req.GetSpec() != nil {
			t.Spec = req.GetSpec()
		}
		t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return t, nil
	}
	return nil, status.Errorf(codes.NotFound, "deployment template %q not found under model %q", req.GetId(), req.GetModelId())
}

func (s *MemoryStore) DeleteModelDeploymentTemplate(_ context.Context, modelID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, t := range s.templates {
		if t.Id == id && t.ModelId == modelID {
			s.templates = append(s.templates[:i], s.templates[i+1:]...)
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "deployment template %q not found under model %q", id, modelID)
}

// compareVersions returns negative / 0 / positive for a vs b using a SemVer-ish
// rule: split on ".", strip a leading "v", compare numerically when both sides
// parse as ints, lexically otherwise. Good enough for "v1.3.0" vs "v0.2.0";
// pre-release tags are not modeled.
func compareVersions(a, b string) int {
	norm := func(s string) []string {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
		return strings.Split(s, ".")
	}
	ap, bp := norm(a), norm(b)
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var av, bv string
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				return ai - bi
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (s *MemoryStore) ResolveModelDeploymentTemplate(_ context.Context, modelID, name, version string) (*pb.ModelDeploymentTemplate, error) {
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if version != "" {
		for _, t := range s.templates {
			if t.ModelId == modelID && t.Name == name && t.Version == version {
				return t, nil
			}
		}
		return nil, status.Errorf(codes.NotFound, "no template %q@%q under model %q", name, version, modelID)
	}

	var (
		latest    *pb.ModelDeploymentTemplate
		anyByName bool
	)
	for _, t := range s.templates {
		if t.ModelId != modelID || t.Name != name {
			continue
		}
		anyByName = true
		if !strings.EqualFold(t.Status, "active") {
			continue
		}
		if latest == nil || compareVersions(t.Version, latest.Version) > 0 {
			latest = t
		}
	}
	if latest != nil {
		return latest, nil
	}
	if anyByName {
		return nil, status.Errorf(codes.FailedPrecondition, "template %q under model %q has no active version", name, modelID)
	}
	return nil, status.Errorf(codes.NotFound, "no template %q under model %q", name, modelID)
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

	fullKey := "aibrix_" + randomString(24)
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
