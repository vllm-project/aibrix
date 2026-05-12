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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	DefaultStoreListLimit = 50
)

// NewMySQLStore creates mysql-backed gorm store with auto-migrations.
func NewMySQLStore(dsn, encryptionKey string) (*GORMStore, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to open mysql: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to access mysql db: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("failed to ping mysql: %w", err)
	}
	s, err := newGORMStore(db, encryptionKey)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := s.RunMigrations(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("failed to run mysql migrations: %w", err)
	}
	return s, nil
}

// NewSQLiteStore creates sqlite-backed gorm store with auto-migrations.
// Pass ":memory:" or any "...mode=memory..." DSN to get an in-memory database;
// such DSNs are pinned to a single connection so all queries see the same
// in-process database.
func NewSQLiteStore(dsn, encryptionKey string) (*GORMStore, error) {
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to access sqlite db: %w", err)
	}
	if isSQLiteInMemoryDSN(dsn) {
		// In-memory SQLite databases are per-connection; multiple connections
		// would each see a different empty DB. Pin to one connection.
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	}
	s, err := newGORMStore(db, encryptionKey)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := s.RunMigrations(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("failed to run sqlite migrations: %w", err)
	}
	return s, nil
}

// NewMemoryStore returns an in-memory SQLite store. Convenience wrapper used
// by the memory:// URI scheme and by tests; equivalent to
// NewSQLiteStore(":memory:", ...). Production deployments should use a
// sqlite: file URL or mysql:// instead.
func NewMemoryStore() *GORMStore {
	s, err := NewSQLiteStore(":memory:", strings.Repeat("0", 64))
	if err != nil {
		panic(fmt.Sprintf("failed to initialize in-memory sqlite store: %v", err))
	}
	return s
}

func isSQLiteInMemoryDSN(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// GORMStore implements Store for mysql/sqlite with shared logic.
type GORMStore struct {
	db     *gorm.DB
	aesKey []byte
}

func newGORMStore(db *gorm.DB, encryptionKey string) (*GORMStore, error) {
	key, err := hex.DecodeString(encryptionKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be a 64-char hex string (32 bytes)")
	}
	return &GORMStore{db: db, aesKey: key}, nil
}

func (s *GORMStore) RunMigrations() error {
	return models.AutoMigrate(s.db)
}

func (s *GORMStore) Close() error {
	if s.db == nil {
		return nil
	}

	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *GORMStore) encryptSecret(plaintext string) (string, error) {
	block, err := aes.NewCipher(s.aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func hashAPIKey(fullKey string) string {
	sum := sha256.Sum256([]byte(fullKey))
	return hex.EncodeToString(sum[:])
}

func (s *GORMStore) ListDeployments(ctx context.Context, search string) ([]*pb.Deployment, error) {
	q := s.db.WithContext(ctx).Model(&models.Deployment{})
	if search != "" {
		like := "%" + search + "%"
		q = q.Where("name LIKE ? OR base_model LIKE ? OR created_by LIKE ?", like, like, like)
	}
	var rows []models.Deployment
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list deployments: %v", err)
	}
	out := make([]*pb.Deployment, 0, len(rows))
	for i := range rows {
		d, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "convert deployment: %v", err)
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *GORMStore) GetDeployment(ctx context.Context, id string) (*pb.Deployment, error) {
	var row models.Deployment
	if err := s.db.WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "deployment %q not found", id)
		}
		return nil, status.Errorf(codes.Internal, "get deployment: %v", err)
	}
	d, err := row.ToPB()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert deployment: %v", err)
	}
	return d, nil
}

func (s *GORMStore) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	id := uuid.NewString()
	deploymentID := uuid.NewString()[:8]
	d := models.Deployment{ID: id, Name: req.Name, DeploymentID: deploymentID, BaseModel: req.BaseModel, BaseModelID: strings.ToLower(strings.ReplaceAll(req.BaseModel, " ", "-")), Replicas: fmt.Sprintf("%d", req.MinReplicas), GpusPerReplica: req.AcceleratorCount, GpuType: req.AcceleratorType, Region: req.Region, Status: "Deploying"}
	if err := s.db.WithContext(ctx).Create(&d).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "create deployment: %v", err)
	}
	dep, err := d.ToPB()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert deployment: %v", err)
	}
	return dep, nil
}

func (s *GORMStore) DeleteDeployment(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&models.Deployment{}, "id = ?", id)
	if res.Error != nil {
		return status.Errorf(codes.Internal, "delete deployment: %v", res.Error)
	}
	if res.RowsAffected == 0 {
		return status.Errorf(codes.NotFound, "deployment %q not found", id)
	}
	return nil
}

func (s *GORMStore) UpsertJob(ctx context.Context, job *pb.Job) error {
	if job == nil || job.Id == "" {
		return status.Error(codes.InvalidArgument, "job id is required")
	}
	rec := models.Job{ID: job.Id, Name: job.Name, CreatedBy: job.CreatedBy}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "created_by", "updated_at"})}).Create(&rec).Error; err != nil {
		return status.Errorf(codes.Internal, "upsert job: %v", err)
	}
	return nil
}

func (s *GORMStore) GetJob(ctx context.Context, id string) (*pb.Job, error) {
	var rec models.Job
	if err := s.db.WithContext(ctx).First(&rec, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, status.Errorf(codes.Internal, "get job: %v", err)
	}
	job, err := rec.ToPB()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert job: %v", err)
	}
	return job, nil
}

func (s *GORMStore) ListJobs(ctx context.Context, ids []string) (map[string]*pb.Job, error) {
	out := make(map[string]*pb.Job, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.Job
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list jobs: %v", err)
	}
	for i := range rows {
		job, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "convert job %s: %v", rows[i].ID, err)
		}
		out[rows[i].ID] = job
	}
	return out, nil
}

func (s *GORMStore) DeleteJob(ctx context.Context, id string) error {
	if err := s.db.WithContext(ctx).Delete(&models.Job{}, "id = ?", id).Error; err != nil {
		return status.Errorf(codes.Internal, "delete job: %v", err)
	}
	return nil
}

func (s *GORMStore) ListModels(ctx context.Context, search, category string) ([]*pb.Model, error) {
	q := s.db.WithContext(ctx).Model(&models.Model{})
	if search != "" {
		like := "%" + search + "%"
		q = q.Where("name LIKE ? OR provider LIKE ?", like, like)
	}
	if category != "" {
		// Categories stored as JSON array like ["LLM", "Vision"]
		// Use LIKE pattern matching which works for both MySQL and SQLite
		q = q.Where("categories LIKE ?", `%"`+category+`"%`)
	}
	var rows []models.Model
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list models: %v", err)
	}
	out := make([]*pb.Model, 0, len(rows))
	for i := range rows {
		m, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "scan model: %v", err)
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *GORMStore) GetModel(ctx context.Context, id string) (*pb.Model, error) {
	var rec models.Model
	if err := s.db.WithContext(ctx).First(&rec, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "model %q not found", id)
		}
		return nil, status.Errorf(codes.Internal, "get model: %v", err)
	}
	m, err := rec.ToPB()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get model: %v", err)
	}
	return m, nil
}

func (s *GORMStore) ListModelDeploymentTemplates(ctx context.Context, modelID, statusFilter, name string) ([]*pb.ModelDeploymentTemplate, error) {
	q := s.db.WithContext(ctx).Model(&models.ModelDeploymentTemplate{})
	if modelID != "" {
		q = q.Where("model_id = ?", modelID)
	}
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	if name != "" {
		q = q.Where("name = ?", name)
	}
	var rows []models.ModelDeploymentTemplate
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list deployment templates: %v", err)
	}
	out := make([]*pb.ModelDeploymentTemplate, 0, len(rows))
	for i := range rows {
		tpl, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "decode template spec: %v", err)
		}
		out = append(out, tpl)
	}
	return out, nil
}

func (s *GORMStore) GetModelDeploymentTemplate(ctx context.Context, modelID, id string) (*pb.ModelDeploymentTemplate, error) {
	var rec models.ModelDeploymentTemplate
	if err := s.db.WithContext(ctx).First(&rec, "id = ? AND model_id = ?", id, modelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "deployment template %q not found under model %q", id, modelID)
		}
		return nil, status.Errorf(codes.Internal, "get deployment template: %v", err)
	}
	return rec.ToPB()
}

func (s *GORMStore) CreateModelDeploymentTemplate(ctx context.Context, req *pb.CreateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	if req.GetSpec() == nil {
		return nil, status.Error(codes.InvalidArgument, "spec is required")
	}
	version := req.GetVersion()
	if version == "" {
		version = "v1.0.0"
	}
	tplStatus := req.GetStatus()
	if tplStatus == "" {
		tplStatus = "active"
	}
	specJSON, err := json.Marshal(req.GetSpec())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode template spec: %v", err)
	}
	rec := models.ModelDeploymentTemplate{ID: uuid.NewString(), Name: req.GetName(), Version: version, Status: tplStatus, ModelID: req.GetModelId(), Spec: datatypes.JSON(specJSON)}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		if isDuplicatedKeyError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "template %q@%q already exists for model %q", req.GetName(), version, req.GetModelId())
		}
		return nil, status.Errorf(codes.Internal, "create deployment template: %v", err)
	}
	return rec.ToPB()
}

func (s *GORMStore) UpdateModelDeploymentTemplate(ctx context.Context, req *pb.UpdateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if req.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	var rec models.ModelDeploymentTemplate
	if err := s.db.WithContext(ctx).First(&rec, "id = ? AND model_id = ?", req.GetId(), req.GetModelId()).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "deployment template %q not found under model %q", req.GetId(), req.GetModelId())
		}
		return nil, status.Errorf(codes.Internal, "update deployment template: %v", err)
	}
	newName, newVersion := rec.Name, rec.Version
	if req.GetName() != "" {
		newName = req.GetName()
	}
	if req.GetVersion() != "" {
		newVersion = req.GetVersion()
	}
	updates := map[string]interface{}{"name": newName, "version": newVersion}
	if req.GetStatus() != "" {
		updates["status"] = req.GetStatus()
	}
	if req.GetSpec() != nil {
		specJSON, err := json.Marshal(req.GetSpec())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode template spec: %v", err)
		}
		updates["spec"] = datatypes.JSON(specJSON)
	}
	if err := s.db.WithContext(ctx).Model(&models.ModelDeploymentTemplate{}).Where("id = ?", rec.ID).Updates(updates).Error; err != nil {
		if isDuplicatedKeyError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "template %q@%q already exists for model %q", newName, newVersion, rec.ModelID)
		}
		return nil, status.Errorf(codes.Internal, "update deployment template: %v", err)
	}
	return s.GetModelDeploymentTemplate(ctx, req.GetModelId(), req.GetId())
}

func (s *GORMStore) DeleteModelDeploymentTemplate(ctx context.Context, modelID, id string) error {
	res := s.db.WithContext(ctx).Delete(&models.ModelDeploymentTemplate{}, "id = ? AND model_id = ?", id, modelID)
	if res.Error != nil {
		return status.Errorf(codes.Internal, "delete deployment template: %v", res.Error)
	}
	if res.RowsAffected == 0 {
		return status.Errorf(codes.NotFound, "deployment template %q not found under model %q", id, modelID)
	}
	return nil
}

// isDuplicatedKeyError checks if the error is a duplicate key violation.
// It handles both GORM's ErrDuplicatedKey and SQLite's "UNIQUE constraint failed" error.
func isDuplicatedKeyError(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	// SQLite returns "UNIQUE constraint failed" as a string error
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return true
	}
	return false
}

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

func (s *GORMStore) ResolveModelDeploymentTemplate(ctx context.Context, modelID, name, version string) (*pb.ModelDeploymentTemplate, error) {
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if version != "" {
		var rec models.ModelDeploymentTemplate
		if err := s.db.WithContext(ctx).First(&rec, "model_id = ? AND name = ? AND version = ?", modelID, name, version).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, status.Errorf(codes.NotFound, "no template %q@%q under model %q", name, version, modelID)
			}
			return nil, status.Errorf(codes.Internal, "resolve deployment template: %v", err)
		}
		return s.GetModelDeploymentTemplate(ctx, modelID, rec.ID)
	}
	var rows []models.ModelDeploymentTemplate
	if err := s.db.WithContext(ctx).Where("model_id = ? AND name = ?", modelID, name).Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "resolve deployment template: %v", err)
	}
	if len(rows) == 0 {
		return nil, status.Errorf(codes.NotFound, "no template %q under model %q", name, modelID)
	}
	var latest *models.ModelDeploymentTemplate
	for i := range rows {
		if !strings.EqualFold(rows[i].Status, "active") {
			continue
		}
		if latest == nil || compareVersions(rows[i].Version, latest.Version) > 0 {
			latest = &rows[i]
		}
	}
	if latest == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "template %q under model %q has no active version", name, modelID)
	}
	return s.GetModelDeploymentTemplate(ctx, modelID, latest.ID)
}

func (s *GORMStore) ListAPIKeys(ctx context.Context) ([]*pb.APIKey, error) {
	var rows []models.APIKey
	if err := s.db.WithContext(ctx).Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list api keys: %v", err)
	}
	out := make([]*pb.APIKey, 0, len(rows))
	for i := range rows {
		k, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "convert api key: %v", err)
		}
		out = append(out, k)
	}
	return out, nil
}

func (s *GORMStore) CreateAPIKey(ctx context.Context, name string) (*pb.APIKey, string, error) {
	randomID := uuid.NewString()[:8]
	randomSecret := uuid.NewString()[:16]
	id := "key_" + randomID
	fullKey := "aibrix_" + randomSecret
	masked := fullKey[:10] + "..."
	rec := models.APIKey{ID: id, Name: name, KeyHash: hashAPIKey(fullKey), KeyPrefix: masked}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return nil, "", status.Errorf(codes.Internal, "create api key: %v", err)
	}
	k, err := rec.ToPB()
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "convert api key: %v", err)
	}
	return k, fullKey, nil
}

func (s *GORMStore) DeleteAPIKey(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&models.APIKey{}, "id = ?", id)
	if res.Error != nil {
		return status.Errorf(codes.Internal, "delete api key: %v", res.Error)
	}
	if res.RowsAffected == 0 {
		return status.Errorf(codes.NotFound, "api key %q not found", id)
	}
	return nil
}

func (s *GORMStore) ListSecrets(ctx context.Context, search string) ([]*pb.Secret, error) {
	q := s.db.WithContext(ctx).Model(&models.Secret{})
	if search != "" {
		like := "%" + search + "%"
		q = q.Where("name LIKE ?", like)
	}
	var rows []models.Secret
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list secrets: %v", err)
	}
	out := make([]*pb.Secret, 0, len(rows))
	for i := range rows {
		sec, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "convert secret: %v", err)
		}
		out = append(out, sec)
	}
	return out, nil
}

func (s *GORMStore) CreateSecret(ctx context.Context, name, value string) (*pb.Secret, error) {
	encrypted, err := s.encryptSecret(value)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encrypt secret: %v", err)
	}
	secretID := uuid.NewString()
	rec := models.Secret{ID: secretID, Name: name, EncryptedValue: encrypted}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "create secret: %v", err)
	}
	sec, err := rec.ToPB()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "convert secret: %v", err)
	}
	return sec, nil
}

func (s *GORMStore) DeleteSecret(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Delete(&models.Secret{}, "id = ?", id)
	if res.Error != nil {
		return status.Errorf(codes.Internal, "delete secret: %v", res.Error)
	}
	if res.RowsAffected == 0 {
		return status.Errorf(codes.NotFound, "secret %q not found", id)
	}
	return nil
}

func (s *GORMStore) ListQuotas(ctx context.Context, search string) ([]*pb.Quota, error) {
	q := s.db.WithContext(ctx).Model(&models.Quota{})
	if search != "" {
		like := "%" + search + "%"
		q = q.Where("name LIKE ? OR quota_id LIKE ?", like, like)
	}
	var rows []models.Quota
	if err := q.Order("name").Find(&rows).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list quotas: %v", err)
	}
	out := make([]*pb.Quota, 0, len(rows))
	for i := range rows {
		q, err := rows[i].ToPB()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "convert quota: %v", err)
		}
		out = append(out, q)
	}
	return out, nil
}

func (s *GORMStore) GetProvision(ctx context.Context, provisionId string) (*types.ProvisionResult, error) {
	var rec models.ProvisionResult
	if err := s.db.WithContext(ctx).First(&rec, "provision_id = ? AND deleted = ?", provisionId, false).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "provision %q not found", provisionId)
		}
		return nil, fmt.Errorf("failed to get provision %q: %w", provisionId, err)
	}
	res := &types.ProvisionResult{}
	if err := json.Unmarshal(rec.Payload, res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal provision payload: %w", err)
	}
	res.Status = types.ProvisionStatus(rec.Status)
	res.UpdatedAt = rec.UpdatedAt
	return res, nil
}

func (s *GORMStore) GetProvisionByIdempotencyKey(ctx context.Context, idempotencyKey string) (*types.ProvisionResult, error) {
	var rec models.ProvisionResult
	if err := s.db.WithContext(ctx).First(&rec, "idempotency_key = ? AND deleted = ?", idempotencyKey, false).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(codes.NotFound, "provision with idempotency key %q not found", idempotencyKey)
		}
		return nil, fmt.Errorf("failed to get provision by idempotency key %q: %w", idempotencyKey, err)
	}
	res := &types.ProvisionResult{}
	if err := json.Unmarshal(rec.Payload, res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal provision payload: %w", err)
	}
	res.Status = types.ProvisionStatus(rec.Status)
	res.UpdatedAt = rec.UpdatedAt
	return res, nil
}

func (s *GORMStore) UpsertProvision(ctx context.Context, result *types.ProvisionResult) error {
	if result == nil {
		return fmt.Errorf("provision result is required")
	}
	if result.IdempotencyKey == "" {
		return fmt.Errorf("idempotency key is required")
	}
	record, err := result.ToProvisionRecord()
	if err != nil {
		return fmt.Errorf("failed to convert provision result to record: %w", err)
	}
	rec := models.ProvisionResult{
		IdempotencyKey: result.IdempotencyKey,
		ProvisionID:    record.ProvisionID,
		Region:         record.Region,
		Status:         record.Status,
		Payload:        datatypes.JSON(record.Payload),
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		Deleted:        false,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "idempotency_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"provision_id", "region", "status", "payload", "updated_at", "deleted"}),
	}).Create(&rec).Error; err != nil {
		return fmt.Errorf("failed to upsert provision result: %w", err)
	}
	return nil
}

func (s *GORMStore) UpdateProvisionStatus(ctx context.Context, provisionId string, pstatus types.ProvisionStatus) error {
	if err := s.db.WithContext(ctx).Model(&models.ProvisionResult{}).Where("provision_id = ? AND deleted = ?", provisionId, false).Updates(map[string]interface{}{"status": string(pstatus), "updated_at": time.Now()}).Error; err != nil {
		return fmt.Errorf("failed to update provision result status: %w", err)
	}
	return nil
}

func (s *GORMStore) DeleteProvision(ctx context.Context, provisionId string) error {
	if err := s.db.WithContext(ctx).Model(&models.ProvisionResult{}).Where("provision_id = ?", provisionId).Updates(map[string]interface{}{"deleted": true, "updated_at": time.Now()}).Error; err != nil {
		return fmt.Errorf("failed to delete provision result: %w", err)
	}
	return nil
}

func (s *GORMStore) ExistsProvision(ctx context.Context, provisionId string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&models.ProvisionResult{}).Where("provision_id = ? AND deleted = ?", provisionId, false).Count(&count).Error; err != nil {
		return false, fmt.Errorf("failed to check provision result: %w", err)
	}
	return count > 0, nil
}

func (s *GORMStore) ListProvisions(ctx context.Context, options *types.ListOptions) ([]*types.ProvisionResult, error) {
	q := s.db.WithContext(ctx).Model(&models.ProvisionResult{}).Where("deleted = ?", false)
	if options != nil {
		if options.Status != nil {
			q = q.Where("status = ?", string(*options.Status))
		}
		if options.ProvisionIDs != nil && len(*options.ProvisionIDs) > 0 {
			q = q.Where("provision_id IN ?", *options.ProvisionIDs)
		}
		if options.Regions != nil && len(*options.Regions) > 0 {
			regionStrs := make([]string, 0, len(*options.Regions))
			for _, region := range *options.Regions {
				regionBytes, err := json.Marshal(region)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal region: %w", err)
				}
				regionStrs = append(regionStrs, string(regionBytes))
			}
			q = q.Where("region IN ?", regionStrs)
		}
	}
	limit := DefaultStoreListLimit
	if options != nil && options.Limit > 0 {
		limit = options.Limit
	}
	offset := 0
	if options != nil {
		offset = options.Offset
	}
	var rows []models.ProvisionResult
	if err := q.Order("created_at DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("failed to list provision results: %w", err)
	}
	out := make([]*types.ProvisionResult, 0, len(rows))
	for _, r := range rows {
		res := &types.ProvisionResult{}
		if err := json.Unmarshal(r.Payload, res); err != nil {
			return nil, fmt.Errorf("failed to unmarshal provision payload: %w", err)
		}
		res.Status = types.ProvisionStatus(r.Status)
		res.Region = r.Region
		res.UpdatedAt = r.UpdatedAt
		out = append(out, res)
	}
	return out, nil
}
