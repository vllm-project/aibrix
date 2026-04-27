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
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed migrations/001_initial.sql
var initialMigrationSQL string

// MySQLStore implements Store backed by a MySQL database.
type MySQLStore struct {
	db     *sql.DB
	aesKey []byte
}

// NewMySQLStore opens a MySQL connection, verifies it with a ping, and returns a MySQLStore.
// encryptionKey is a hex-encoded 32-byte key used for AES-256-GCM on stored secrets.
func NewMySQLStore(dsn, encryptionKey string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping mysql: %w", err)
	}
	key, err := hex.DecodeString(encryptionKey)
	if err != nil || len(key) != 32 {
		_ = db.Close()
		return nil, fmt.Errorf("encryption key must be a 64-char hex string (32 bytes)")
	}
	return &MySQLStore{db: db, aesKey: key}, nil
}

// RunMigrations executes the embedded initial SQL migration.
func (s *MySQLStore) RunMigrations() error {
	if _, err := s.db.Exec(initialMigrationSQL); err != nil {
		return fmt.Errorf("failed to execute migration: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *MySQLStore) Close() error {
	return s.db.Close()
}

// mysqlRandomString generates a random alphanumeric string of length n.
// It panics if crypto/rand fails, since continuing without randomness would
// produce predictable identifiers.
func mysqlRandomString(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		b[i] = chars[idx.Int64()]
	}
	return string(b)
}

// encryptSecret encrypts plaintext with AES-256-GCM using the store's key.
// The returned string is base64(nonce || ciphertext || tag).
func (s *MySQLStore) encryptSecret(plaintext string) (string, error) {
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

// hashAPIKey returns a hex-encoded SHA-256 hash of the full API key.
func hashAPIKey(fullKey string) string {
	sum := sha256.Sum256([]byte(fullKey))
	return hex.EncodeToString(sum[:])
}

// --- Deployments ---

func (s *MySQLStore) ListDeployments(ctx context.Context, search string) ([]*pb.Deployment, error) {
	var rows *sql.Rows
	var err error

	if search == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name, deployment_id, base_model, base_model_id,
			        replicas, gpus_per_replica, gpu_type, region, created_by, status
			 FROM deployments ORDER BY created_at DESC`)
	} else {
		like := "%" + search + "%"
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name, deployment_id, base_model, base_model_id,
			        replicas, gpus_per_replica, gpu_type, region, created_by, status
			 FROM deployments
			 WHERE name LIKE ? OR base_model LIKE ? OR created_by LIKE ?
			 ORDER BY created_at DESC`, like, like, like)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list deployments: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*pb.Deployment
	for rows.Next() {
		d := &pb.Deployment{}
		if err := rows.Scan(&d.Id, &d.Name, &d.DeploymentId, &d.BaseModel,
			&d.BaseModelId, &d.Replicas, &d.GpusPerReplica, &d.GpuType,
			&d.Region, &d.CreatedBy, &d.Status); err != nil {
			return nil, status.Errorf(codes.Internal, "scan deployment: %v", err)
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate deployments: %v", err)
	}
	return result, nil
}

func (s *MySQLStore) GetDeployment(ctx context.Context, id string) (*pb.Deployment, error) {
	d := &pb.Deployment{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, deployment_id, base_model, base_model_id,
		        replicas, gpus_per_replica, gpu_type, region, created_by, status
		 FROM deployments WHERE id = ?`, id).
		Scan(&d.Id, &d.Name, &d.DeploymentId, &d.BaseModel,
			&d.BaseModelId, &d.Replicas, &d.GpusPerReplica, &d.GpuType,
			&d.Region, &d.CreatedBy, &d.Status)
	if err == sql.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "deployment %q not found", id)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get deployment: %v", err)
	}
	return d, nil
}

func (s *MySQLStore) CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	id := mysqlRandomString(36)
	deploymentID := mysqlRandomString(8)
	baseModelID := strings.ToLower(strings.ReplaceAll(req.BaseModel, " ", "-"))
	replicas := fmt.Sprintf("%d", req.MinReplicas)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deployments (id, name, deployment_id, base_model, base_model_id,
		                          replicas, gpus_per_replica, gpu_type, region, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.Name, deploymentID, req.BaseModel, baseModelID,
		replicas, req.AcceleratorCount, req.AcceleratorType, req.Region, "Deploying")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create deployment: %v", err)
	}

	return &pb.Deployment{
		Id:             id,
		Name:           req.Name,
		DeploymentId:   deploymentID,
		BaseModel:      req.BaseModel,
		BaseModelId:    baseModelID,
		Replicas:       replicas,
		GpusPerReplica: req.AcceleratorCount,
		GpuType:        req.AcceleratorType,
		Region:         req.Region,
		Status:         "Deploying",
	}, nil
}

func (s *MySQLStore) DeleteDeployment(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM deployments WHERE id = ?`, id)
	if err != nil {
		return status.Errorf(codes.Internal, "delete deployment: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Errorf(codes.NotFound, "deployment %q not found", id)
	}
	return nil
}

// --- Jobs (Console-side fields only) ---

func (s *MySQLStore) UpsertJob(ctx context.Context, job *pb.Job) error {
	if job == nil || job.Id == "" {
		return status.Error(codes.InvalidArgument, "job id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, name, created_by) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE name = VALUES(name), created_by = VALUES(created_by)`,
		job.Id, job.Name, job.CreatedBy)
	if err != nil {
		return status.Errorf(codes.Internal, "upsert job: %v", err)
	}
	return nil
}

func (s *MySQLStore) GetJob(ctx context.Context, id string) (*pb.Job, error) {
	j := &pb.Job{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_by FROM jobs WHERE id = ?`, id).
		Scan(&j.Id, &j.Name, &j.CreatedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get job: %v", err)
	}
	return j, nil
}

func (s *MySQLStore) ListJobs(ctx context.Context, ids []string) (map[string]*pb.Job, error) {
	out := make(map[string]*pb.Job, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_by FROM jobs WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list jobs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		j := &pb.Job{}
		if err := rows.Scan(&j.Id, &j.Name, &j.CreatedBy); err != nil {
			return nil, status.Errorf(codes.Internal, "scan job: %v", err)
		}
		out[j.Id] = j
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate jobs: %v", err)
	}
	return out, nil
}

func (s *MySQLStore) DeleteJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return status.Errorf(codes.Internal, "delete job: %v", err)
	}
	return nil
}

// --- Models ---

func (s *MySQLStore) ListModels(ctx context.Context, search, category string) ([]*pb.Model, error) {
	// `provider` column is retained for the legacy LIKE search path even though
	// the proto field has been removed; metadata.provider_name is the canonical
	// source. A future migration can drop the column entirely.
	query := `SELECT id, name, icon_bg, icon_text, icon_text_color,
	                  categories, is_new, pricing, context_length, description,
	                  metadata, specification, tags
	           FROM models WHERE 1=1`
	var args []interface{}

	if category != "" {
		query += " AND JSON_CONTAINS(categories, ?)"
		catJSON, _ := json.Marshal(category)
		args = append(args, string(catJSON))
	}
	if search != "" {
		like := "%" + search + "%"
		query += " AND (name LIKE ? OR provider LIKE ?)"
		args = append(args, like, like)
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list models: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*pb.Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "scan model: %v", err)
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate models: %v", err)
	}
	return result, nil
}

func (s *MySQLStore) GetModel(ctx context.Context, id string) (*pb.Model, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, icon_bg, icon_text, icon_text_color,
		        categories, is_new, pricing, context_length, description,
		        metadata, specification, tags
		 FROM models WHERE id = ?`, id)

	m, err := scanModelRow(row)
	if err == sql.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "model %q not found", id)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get model: %v", err)
	}
	return m, nil
}

// scanModel scans a model from sql.Rows.
func scanModel(rows *sql.Rows) (*pb.Model, error) {
	m := &pb.Model{}
	var (
		categoriesJSON  []byte
		pricingJSON     []byte
		metadataJSON    []byte
		specJSON        []byte
		tagsJSON        []byte
		descriptionNull sql.NullString
	)
	if err := rows.Scan(&m.Id, &m.Name, &m.IconBg, &m.IconText,
		&m.IconTextColor, &categoriesJSON, &m.IsNew, &pricingJSON,
		&m.ContextLength, &descriptionNull, &metadataJSON, &specJSON, &tagsJSON); err != nil {
		return nil, err
	}
	if descriptionNull.Valid {
		m.Description = descriptionNull.String
	}
	return unmarshalModelJSON(m, categoriesJSON, pricingJSON, metadataJSON, specJSON, tagsJSON)
}

// scanModelRow scans a model from a single sql.Row.
func scanModelRow(row *sql.Row) (*pb.Model, error) {
	m := &pb.Model{}
	var (
		categoriesJSON  []byte
		pricingJSON     []byte
		metadataJSON    []byte
		specJSON        []byte
		tagsJSON        []byte
		descriptionNull sql.NullString
	)
	if err := row.Scan(&m.Id, &m.Name, &m.IconBg, &m.IconText,
		&m.IconTextColor, &categoriesJSON, &m.IsNew, &pricingJSON,
		&m.ContextLength, &descriptionNull, &metadataJSON, &specJSON, &tagsJSON); err != nil {
		return nil, err
	}
	if descriptionNull.Valid {
		m.Description = descriptionNull.String
	}
	return unmarshalModelJSON(m, categoriesJSON, pricingJSON, metadataJSON, specJSON, tagsJSON)
}

// --- Model Deployment Templates (MySQL) ---
//
// TODO: persistence not yet implemented for MySQL. Templates currently live
// only in the memory store. When this lands, add a `model_deployment_templates`
// table and JSON-encode the spec column.

func (s *MySQLStore) ListModelDeploymentTemplates(_ context.Context, _ string, _ string, _ string) ([]*pb.ModelDeploymentTemplate, error) {
	return nil, status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

func (s *MySQLStore) GetModelDeploymentTemplate(_ context.Context, _ string, _ string) (*pb.ModelDeploymentTemplate, error) {
	return nil, status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

func (s *MySQLStore) CreateModelDeploymentTemplate(_ context.Context, _ *pb.CreateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return nil, status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

func (s *MySQLStore) UpdateModelDeploymentTemplate(_ context.Context, _ *pb.UpdateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error) {
	return nil, status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

func (s *MySQLStore) DeleteModelDeploymentTemplate(_ context.Context, _ string, _ string) error {
	return status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

func (s *MySQLStore) ResolveModelDeploymentTemplate(_ context.Context, _ string, _ string, _ string) (*pb.ModelDeploymentTemplate, error) {
	return nil, status.Error(codes.Unimplemented, "model deployment templates not yet supported by mysql store")
}

// unmarshalModelJSON unmarshals the JSON columns into the model's sub-messages.
func unmarshalModelJSON(m *pb.Model, categoriesJSON, pricingJSON, metadataJSON, specJSON, tagsJSON []byte) (*pb.Model, error) {
	if len(categoriesJSON) > 0 {
		if err := json.Unmarshal(categoriesJSON, &m.Categories); err != nil {
			return nil, fmt.Errorf("unmarshal model categories: %w", err)
		}
	}
	if len(pricingJSON) > 0 {
		m.Pricing = &pb.ModelPricing{}
		if err := json.Unmarshal(pricingJSON, m.Pricing); err != nil {
			return nil, fmt.Errorf("unmarshal model pricing: %w", err)
		}
	}
	if len(metadataJSON) > 0 {
		m.Metadata = &pb.ModelMetadata{}
		if err := json.Unmarshal(metadataJSON, m.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal model metadata: %w", err)
		}
	}
	if len(specJSON) > 0 {
		m.Specification = &pb.ModelSpecification{}
		if err := json.Unmarshal(specJSON, m.Specification); err != nil {
			return nil, fmt.Errorf("unmarshal model specification: %w", err)
		}
	}
	if len(tagsJSON) > 0 {
		if err := json.Unmarshal(tagsJSON, &m.Tags); err != nil {
			return nil, fmt.Errorf("unmarshal model tags: %w", err)
		}
	}
	return m, nil
}

// --- API Keys ---

func (s *MySQLStore) ListAPIKeys(ctx context.Context) ([]*pb.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, key_prefix, created_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list api keys: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*pb.APIKey
	for rows.Next() {
		k := &pb.APIKey{}
		var createdAt time.Time
		if err := rows.Scan(&k.Id, &k.Name, &k.SecretKey, &createdAt); err != nil {
			return nil, status.Errorf(codes.Internal, "scan api key: %v", err)
		}
		k.CreatedAt = createdAt.Format("Jan 02, 2006 3:04 PM")
		result = append(result, k)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate api keys: %v", err)
	}
	return result, nil
}

func (s *MySQLStore) CreateAPIKey(ctx context.Context, name string) (*pb.APIKey, string, error) {
	id := "key_" + mysqlRandomString(16)
	fullKey := "aibrix_" + mysqlRandomString(24)
	masked := fullKey[:10] + "..."
	hashed := hashAPIKey(fullKey)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, key_hash, key_prefix) VALUES (?, ?, ?, ?)`,
		id, name, hashed, masked)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "create api key: %v", err)
	}

	key := &pb.APIKey{
		Id:        id,
		Name:      name,
		SecretKey: masked,
		CreatedAt: time.Now().Format("Jan 02, 2006 3:04 PM"),
	}
	return key, fullKey, nil
}

func (s *MySQLStore) DeleteAPIKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return status.Errorf(codes.Internal, "delete api key: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Errorf(codes.NotFound, "api key %q not found", id)
	}
	return nil
}

// --- Secrets ---

func (s *MySQLStore) ListSecrets(ctx context.Context, search string) ([]*pb.Secret, error) {
	var rows *sql.Rows
	var err error

	if search == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name FROM secrets ORDER BY created_at DESC`)
	} else {
		like := "%" + search + "%"
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name FROM secrets WHERE name LIKE ? ORDER BY created_at DESC`, like)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list secrets: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*pb.Secret
	for rows.Next() {
		sec := &pb.Secret{}
		if err := rows.Scan(&sec.Id, &sec.Name); err != nil {
			return nil, status.Errorf(codes.Internal, "scan secret: %v", err)
		}
		result = append(result, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate secrets: %v", err)
	}
	return result, nil
}

func (s *MySQLStore) CreateSecret(ctx context.Context, name, value string) (*pb.Secret, error) {
	id := mysqlRandomString(36)
	encrypted, err := s.encryptSecret(value)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encrypt secret: %v", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets (id, name, encrypted_value) VALUES (?, ?, ?)`,
		id, name, encrypted)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create secret: %v", err)
	}
	return &pb.Secret{Id: id, Name: name}, nil
}

func (s *MySQLStore) DeleteSecret(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE id = ?`, id)
	if err != nil {
		return status.Errorf(codes.Internal, "delete secret: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Errorf(codes.NotFound, "secret %q not found", id)
	}
	return nil
}

// --- Quotas ---

func (s *MySQLStore) ListQuotas(ctx context.Context, search string) ([]*pb.Quota, error) {
	var rows *sql.Rows
	var err error

	if search == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name, quota_id, current_usage, usage_percentage, quota
			 FROM quotas ORDER BY name`)
	} else {
		like := "%" + search + "%"
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, name, quota_id, current_usage, usage_percentage, quota
			 FROM quotas WHERE name LIKE ? OR quota_id LIKE ?
			 ORDER BY name`, like, like)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list quotas: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var result []*pb.Quota
	for rows.Next() {
		q := &pb.Quota{}
		if err := rows.Scan(&q.Id, &q.Name, &q.QuotaId,
			&q.CurrentUsage, &q.UsagePercentage, &q.Quota); err != nil {
			return nil, status.Errorf(codes.Internal, "scan quota: %v", err)
		}
		result = append(result, q)
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate quotas: %v", err)
	}
	return result, nil
}

// --- Seed Data ---

// LoadDemoData inserts the default demo data if the database is empty (deployment count == 0).
func (s *MySQLStore) LoadDemoData() error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&count); err != nil {
		return fmt.Errorf("check deployment count: %w", err)
	}
	if count > 0 {
		return nil
	}

	if err := s.loadDemoDeployments(); err != nil {
		return err
	}
	if err := s.loadDemoJobs(); err != nil {
		return err
	}
	if err := s.loadDemoModels(); err != nil {
		return err
	}
	if err := s.loadDemoAPIKeys(); err != nil {
		return err
	}
	if err := s.loadDemoSecrets(); err != nil {
		return err
	}
	if err := s.loadDemoQuotas(); err != nil {
		return err
	}
	return nil
}

func (s *MySQLStore) loadDemoDeployments() error {
	_, err := s.db.Exec(
		`INSERT INTO deployments (id, name, deployment_id, base_model, base_model_id,
		                          replicas, gpus_per_replica, gpu_type, region, created_by, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"deploy-1", "gsm-8k-20260118", "euxdnr5z",
		"DeepSeek R1 Distill Llama 8B", "deepseek-r1-distil-llama-8b",
		"1[01]", 1, "NVIDIA H100 80GB", "US Iowa 1",
		"demo@aibrix.ai", "Ready")
	return err
}

func (s *MySQLStore) loadDemoJobs() error {
	// The store only persists Console-owned fields (id, name, created_by).
	// OpenAI Batch state for these demo IDs is synthesized by the JobHandler
	// dev fallback when the metadata service is unreachable.
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, name, created_by) VALUES (?, ?, ?), (?, ?, ?)`,
		"batch_demo_27a6ee2c", "gsm-8k-20260118", "demo@aibrix.ai",
		"batch_demo_a0b13ef5", "gsm-8k-20260118-v2", "demo@aibrix.ai")
	return err
}

func (s *MySQLStore) loadDemoModels() error {
	type demoModel struct {
		id            string
		name          string
		provider      string
		iconBg        string
		iconText      string
		iconTextColor string
		categories    []string
		isNew         bool
		pricing       *pb.ModelPricing
		contextLength string
		description   string
		metadata      *pb.ModelMetadata
		specification *pb.ModelSpecification
		tags          []string
	}

	models := []demoModel{
		{
			id: "model-minimax-m2.5", name: "MiniMax-M2.5", provider: "MiniMax",
			iconBg: "bg-red-100", iconText: "M", iconTextColor: "text-red-600",
			categories: []string{"LLM"}, isNew: true,
			pricing:       &pb.ModelPricing{UncachedInput: "$0.30/M", CachedInput: "$0.03/M", Output: "$1.20/M"},
			contextLength: "192k Context",
			description:   "MiniMax-M2.5 is a powerful language model designed for complex reasoning and code generation tasks.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 10, 2026", ProviderName: "MiniMax", HuggingFace: "minimax/MiniMax-M2.5"},
			specification: &pb.ModelSpecification{Parameters: "250B"},
			tags:          []string{"Serverless"},
		},
		{
			id: "model-glm-5", name: "GLM-5", provider: "Z.ai",
			iconBg: "bg-green-100", iconText: "Z", iconTextColor: "text-green-700",
			categories: []string{"LLM"}, isNew: true,
			pricing:       &pb.ModelPricing{UncachedInput: "$1.00/M", CachedInput: "$0.20/M", Output: "$3.20/M"},
			contextLength: "198k Context",
			description:   "GLM-5 is Z.ai's SOTA model targeting complex systems engineering and long-horizon agentic tasks.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 12, 2026", ProviderName: "Z.ai", HuggingFace: "zai-org/GLM-5"},
			specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "700B"},
			tags:          []string{"Serverless"},
		},
		{
			id: "model-kimi-k2.5", name: "Kimi K2.5", provider: "Moonshot AI",
			iconBg: "bg-gray-100", iconText: "K", iconTextColor: "text-gray-800",
			categories: []string{"LLM", "Vision"}, isNew: true,
			pricing:       &pb.ModelPricing{UncachedInput: "$0.60/M", CachedInput: "$0.10/M", Output: "$3.00/M"},
			contextLength: "256k Context",
			description:   "Kimi K2.5 is a multimodal language model from Moonshot AI that supports both text and vision inputs.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 8, 2026", ProviderName: "Moonshot AI", HuggingFace: "moonshot/kimi-k2.5"},
			specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "400B"},
			tags:          []string{"Serverless", "Tunable"},
		},
		{
			id: "model-deepseek-v3.2", name: "Deepseek v3.2", provider: "DeepSeek",
			iconBg: "bg-purple-100", iconText: "D", iconTextColor: "text-purple-700",
			categories:    []string{"LLM"},
			pricing:       &pb.ModelPricing{UncachedInput: "$0.56/M"},
			contextLength: "128k Context",
			description:   "Deepseek v3.2 is the latest iteration of the DeepSeek language model series.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Jan 25, 2026", ProviderName: "DeepSeek", HuggingFace: "deepseek-ai/deepseek-v3.2"},
			specification: &pb.ModelSpecification{MixtureOfExperts: true, Parameters: "671B"},
			tags:          []string{"Serverless"},
		},
		{
			id: "model-llama-3.3-70b", name: "Llama 3.3 70B Instruct", provider: "Meta",
			iconBg: "bg-blue-100", iconText: "L", iconTextColor: "text-blue-700",
			categories:    []string{"LLM"},
			pricing:       &pb.ModelPricing{UncachedInput: "$0.20/M", Output: "$0.20/M"},
			contextLength: "128k Context",
			description:   "Meta's Llama 3.3 70B Instruct model is optimized for instruction following and multi-turn dialogue.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Dec 10, 2025", ProviderName: "Meta", HuggingFace: "meta-llama/Llama-3.3-70B-Instruct"},
			specification: &pb.ModelSpecification{Parameters: "70B"},
			tags:          []string{"Serverless", "Tunable", "Function Calling"},
		},
		{
			id: "model-whisper-v3-large", name: "Whisper V3 Large", provider: "OpenAI",
			iconBg: "bg-gray-100", iconText: "W", iconTextColor: "text-gray-700",
			categories:    []string{"Audio"},
			pricing:       &pb.ModelPricing{PerMinute: "$0.0015/minute"},
			description:   "OpenAI's Whisper V3 Large is a general-purpose speech recognition model.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Oct 20, 2025", ProviderName: "OpenAI", HuggingFace: "openai/whisper-large-v3"},
			specification: &pb.ModelSpecification{Calibrated: true, Parameters: "1.5B"},
			tags:          []string{"Serverless"},
		},
		{
			id: "model-flux-kontext-pro", name: "FLUX.1 Kontext Pro", provider: "Black Forest Labs",
			iconBg: "bg-gray-200", iconText: "\u25b3", iconTextColor: "text-gray-700",
			categories:    []string{"Image"},
			pricing:       &pb.ModelPricing{PerImage: "$0.04/ea"},
			description:   "FLUX.1 Kontext Pro is a professional-grade image generation model.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Feb 1, 2026", ProviderName: "Black Forest Labs"},
			specification: &pb.ModelSpecification{Parameters: "12B"},
			tags:          []string{"Serverless"},
		},
		{
			id: "model-nomic-embed-text", name: "Nomic Embed Text v1.5", provider: "Nomic AI",
			iconBg: "bg-teal-100", iconText: "N", iconTextColor: "text-teal-700",
			categories:    []string{"Embedding"},
			pricing:       &pb.ModelPricing{UncachedInput: "$0.008/M"},
			contextLength: "8k Context",
			description:   "Nomic Embed Text v1.5 is a lightweight, high-performance text embedding model.",
			metadata:      &pb.ModelMetadata{State: "Ready", CreatedOn: "Sep 1, 2025", ProviderName: "Nomic AI", HuggingFace: "nomic-ai/nomic-embed-text-v1.5"},
			specification: &pb.ModelSpecification{Parameters: "137M"},
			tags:          []string{"Serverless"},
		},
	}

	for _, m := range models {
		categoriesJSON, _ := json.Marshal(m.categories)
		pricingJSON, _ := json.Marshal(m.pricing)
		metadataJSON, _ := json.Marshal(m.metadata)
		specJSON, _ := json.Marshal(m.specification)
		tagsJSON, _ := json.Marshal(m.tags)

		_, err := s.db.Exec(
			`INSERT INTO models (id, name, provider, icon_bg, icon_text, icon_text_color,
			                     categories, is_new, pricing, context_length, description,
			                     metadata, specification, tags)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.id, m.name, m.provider, m.iconBg, m.iconText, m.iconTextColor,
			string(categoriesJSON), m.isNew, string(pricingJSON), m.contextLength,
			m.description, string(metadataJSON), string(specJSON), string(tagsJSON))
		if err != nil {
			return fmt.Errorf("demo model %s: %w", m.id, err)
		}
	}
	return nil
}

func (s *MySQLStore) loadDemoAPIKeys() error {
	_, err := s.db.Exec(
		`INSERT INTO api_keys (id, name, key_hash, key_prefix) VALUES (?, ?, ?, ?)`,
		"key_5VFKZKA2qxmqU5aJ", "AIBRIX_API_KEY",
		hashAPIKey("aibrix_WSo2xxxxxxxxxxxxxxxxxx"), "aibrix_WSo2...")
	return err
}

func (s *MySQLStore) loadDemoSecrets() error {
	encrypted, err := s.encryptSecret("secret-value")
	if err != nil {
		return fmt.Errorf("demo secrets: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO secrets (id, name, encrypted_value) VALUES (?, ?, ?)`,
		"1", "GENERAL_SECRET_KEY", encrypted)
	return err
}

func (s *MySQLStore) loadDemoQuotas() error {
	_, err := s.db.Exec(
		`INSERT INTO quotas (id, name, quota_id, current_usage, usage_percentage, quota)
		 VALUES (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?),
		        (?, ?, ?, ?, ?, ?)`,
		"1", "Deployed Model Count", "deployed-model-count", 0, 0.0, 100,
		"2", "Eval Protocol Free Daily Credits", "eval-protocol-free-daily-credits", 0, 0.0, 0,
		"3", "GLOBAL - A100 Count", "global--a100-count", 0, 0.0, 16,
		"4", "GLOBAL - B200 Count", "global--b200-count", 0, 0.0, 16,
		"5", "GLOBAL - H100 Count", "global--h100-count", 1, 6.25, 16,
		"6", "GLOBAL - H200 Count", "global--h200-count", 0, 0.0, 16,
		"7", "Job Submission Count", "job-submission-count", 0, 0.0, 8)
	return err
}
