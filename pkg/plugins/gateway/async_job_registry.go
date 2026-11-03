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
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/bytedance/sonic"
	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// Asynchronous jobs are backend work items (a video render, and in future other
// long-running generations) that outlive the request which created them. The
// engine that accepted the job is the only replica that holds its state and its
// output, so every follow-up request has to reach that exact pod.
//
// The gateway keeps the minimum needed to route those follow-ups: a public job
// id it minted itself, the owner scope allowed to use it, and the pod identity
// that accepted the job. It is deliberately not a job-state authority - it does
// not track status, store results, or poll the backend.
const (
	// asyncJobTypeVideo is the job type of the vLLM-Omni Videos API.
	asyncJobTypeVideo = "video"

	// asyncJobTargetKindPod is the only routing target kind accepted in v1.
	// The field exists so a future kind (a service, a shard group) can be added
	// without a record migration, but an unknown kind is never routable.
	asyncJobTargetKindPod = "pod"

	// defaultAsyncJobTTL bounds how long a pinned pod identity is remembered
	// when the backend does not report an expiry of its own.
	defaultAsyncJobTTL = 7 * 24 * time.Hour

	// asyncJobOwnerShared is the scope of requests that carry no user identity.
	// The gateway's static bearer token names no principal, so those requests
	// share one scope rather than being attributed to the token.
	asyncJobOwnerShared = "scope:shared"

	// asyncJobOwnerUserScopePrefix scopes a job to the request user identity.
	asyncJobOwnerUserScopePrefix = "scope:user:"

	// asyncJobPublicIDPrefix marks an id as minted by AIBrix. Public ids are
	// opaque random values: a backend job id must never leak to a client, since
	// it would let one client address another client's job on a shared pod.
	asyncJobPublicIDPrefix = "aibrixjob-"
	asyncJobPublicIDBytes  = 16

	asyncJobRedisKeyPrefix = "aibrix:gateway:async_job:"

	// Async-job catalog pagination follows the OpenAI Videos API. Keeping the
	// page bounded is also important for ext_proc: a single owner must not turn
	// GET /v1/videos into an unbounded Redis read or response body.
	defaultAsyncJobListLimit = 20
	maxAsyncJobListLimit     = 100
	asyncJobListScanBatch    = 64
	asyncJobListMaxScan      = 512

	// asyncJobStoreMaxAttempts is the total number of attempts, not the number
	// of retries on top of the first one.
	asyncJobStoreMaxAttempts   = 3
	asyncJobStoreBaseBackoff   = 20 * time.Millisecond
	asyncJobStoreRetryDeadline = time.Second
)

var (
	// errAsyncJobNotFound covers both "no such job" and "not yours": the two are
	// deliberately indistinguishable so a public id cannot be probed for
	// existence across owner scopes.
	errAsyncJobNotFound = errors.New("async job not found")

	// errAsyncJobTargetUnavailable means the pinned pod still exists but cannot
	// take the request right now. The record is kept so the client can retry.
	errAsyncJobTargetUnavailable = errors.New("async job routing target is temporarily unavailable")

	// errAsyncJobStoreUnavailable means the store kept failing transiently until
	// the retry budget ran out. Retryable from the client's point of view.
	errAsyncJobStoreUnavailable = errors.New("async job store is temporarily unavailable")

	// errAsyncJobInvalidRecord covers validation and serialization failures,
	// which no amount of retrying will fix.
	errAsyncJobInvalidRecord = errors.New("invalid async job record")

	// transientRedisServerErrorPrefixes are the server replies worth retrying:
	// the node is coming up, the slot is moving, or a failover just changed the
	// write target. Everything else (auth, wrong type, script errors) is a bug
	// or a misconfiguration and retrying only burns the request deadline.
	transientRedisServerErrorPrefixes = []string{
		"LOADING",
		"TRYAGAIN",
		"CLUSTERDOWN",
		"MASTERDOWN",
		"READONLY",
	}
)

// asyncJobInvalidRecordError keeps the classification available to errors.Is
// without making the internal sentinel text part of Error(). This matters when
// validation errors cross an HTTP boundary: adding another %w wrapper may add
// context, but can never expose "invalid async job record" to a client.
type asyncJobInvalidRecordError struct {
	message string
}

func (e asyncJobInvalidRecordError) Error() string {
	return e.message
}

func (e asyncJobInvalidRecordError) Unwrap() error {
	return errAsyncJobInvalidRecord
}

func newAsyncJobInvalidRecordError(format string, args ...any) error {
	return asyncJobInvalidRecordError{message: fmt.Sprintf(format, args...)}
}

// AsyncJobPodTarget identifies the pod that accepted a job. The UID is part of
// the identity on purpose: a pod recreated under the same name is a different
// pod, and the job's output does not exist on its disk.
type AsyncJobPodTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// AsyncJobRoutingTarget describes where follow-up requests for a job must go.
type AsyncJobRoutingTarget struct {
	Kind string            `json:"kind"`
	Pod  AsyncJobPodTarget `json:"pod"`
}

// AsyncJobRecord is everything the gateway remembers about an asynchronous job.
type AsyncJobRecord struct {
	PublicJobID   string                `json:"public_job_id"`
	JobType       string                `json:"job_type"`
	Owner         string                `json:"owner"`
	Model         string                `json:"model"`
	BackendJobID  string                `json:"backend_job_id"`
	RoutingTarget AsyncJobRoutingTarget `json:"routing_target"`
	CreatedAt     time.Time             `json:"created_at"`
	ExpiresAt     time.Time             `json:"expires_at"`
}

func (r AsyncJobRecord) validate() error {
	switch {
	case r.PublicJobID == "":
		return newAsyncJobInvalidRecordError("public job id is required")
	case r.JobType == "":
		return newAsyncJobInvalidRecordError("job type is required")
	case r.Owner == "":
		return newAsyncJobInvalidRecordError("owner is required")
	case r.BackendJobID == "":
		return newAsyncJobInvalidRecordError("backend job id is required")
	case r.RoutingTarget.Kind != asyncJobTargetKindPod:
		return newAsyncJobInvalidRecordError("unsupported routing target kind %q", r.RoutingTarget.Kind)
	case r.RoutingTarget.Pod.Name == "":
		return newAsyncJobInvalidRecordError("routing target pod name is required")
	case r.RoutingTarget.Pod.Namespace == "":
		return newAsyncJobInvalidRecordError("routing target pod namespace is required")
	case r.RoutingTarget.Pod.UID == "":
		return newAsyncJobInvalidRecordError("routing target pod uid is required")
	case r.ExpiresAt.IsZero():
		return newAsyncJobInvalidRecordError("expiry is required")
	}
	return nil
}

func (r AsyncJobRecord) expired(now time.Time) bool {
	return !r.ExpiresAt.After(now)
}

// AsyncJobRegistration is the caller's view of a job that a backend has just
// accepted. The public id and timestamps are the registry's to decide.
type AsyncJobRegistration struct {
	JobType      string
	Owner        string
	Model        string
	BackendJobID string
	Pod          *v1.Pod
	// ExpiresAt is the backend's own expiry when it reported one, and is used
	// exactly as reported. Only an absent expiry gets defaultAsyncJobTTL; an
	// expiry that has already elapsed is refused rather than extended.
	ExpiresAt time.Time
}

// AsyncJobListOptions describes one cursor page of an owner's jobs. After is
// the last public id from the previous page and must still be present in the
// owner's catalog; a deleted or expired cursor is invalid, and the caller must
// restart from the first page. Order is "asc" or "desc" by creation time. Limit
// is validated by the caller or the store before use.
type AsyncJobListOptions struct {
	After string
	Limit int
	Order string
}

// AsyncJobListPage is the registry result used to build OpenAI-style list
// envelopes without exposing backend job ids or routing targets.
type AsyncJobListPage struct {
	Records []AsyncJobRecord
	HasMore bool
}

func normalizeAsyncJobListOptions(options AsyncJobListOptions) (AsyncJobListOptions, error) {
	if options.Limit < 1 || options.Limit > maxAsyncJobListLimit {
		return AsyncJobListOptions{}, newAsyncJobInvalidRecordError("list limit must be between 1 and %d", maxAsyncJobListLimit)
	}
	if options.Order == "" {
		options.Order = "desc"
	}
	if options.Order != "asc" && options.Order != "desc" {
		return AsyncJobListOptions{}, newAsyncJobInvalidRecordError("list order must be asc or desc")
	}
	if options.After != "" && !isValidPublicJobID(options.After) {
		return AsyncJobListOptions{}, newAsyncJobInvalidRecordError("invalid list cursor")
	}
	return options, nil
}

// asyncJobStore is the durable half of the registry. Implementations own
// expiry and the owner+job_type secondary index that backs list.
type asyncJobStore interface {
	put(ctx context.Context, record AsyncJobRecord) error
	get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, error)
	list(ctx context.Context, owner, jobType string) ([]AsyncJobRecord, error)
	delete(ctx context.Context, owner, publicJobID string) error
}

// asyncJobPageStore is deliberately separate from asyncJobStore so existing
// test stores and future minimal stores can still implement the registry. The
// production and in-memory stores implement it to avoid materializing a whole
// owner's catalog for a single page.
type asyncJobPageStore interface {
	listPage(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error)
}

// podResolver is the slice of the informer-backed pod cache the registry needs;
// it never calls the Kubernetes API directly. cache.Cache satisfies it, and its
// error means the pod key is absent from the cache rather than a transient API
// transport failure.
type podResolver interface {
	GetPod(podName string, podNamespace string) (*v1.Pod, error)
}

// asyncJobOwnerFromUser derives the owner scope from the request user identity.
func asyncJobOwnerFromUser(user utils.User) string {
	return asyncJobOwnerFromUserName(user.Name)
}

func asyncJobOwnerFromUserName(userName string) string {
	name := strings.TrimSpace(userName)
	if name == "" {
		return asyncJobOwnerShared
	}
	return asyncJobOwnerUserScopePrefix + name
}

func newAsyncJobPublicID() (string, error) {
	buf := make([]byte, asyncJobPublicIDBytes)
	if _, err := crand.Read(buf); err != nil {
		return "", fmt.Errorf("generate async job public id: %w", err)
	}
	return asyncJobPublicIDPrefix + hex.EncodeToString(buf), nil
}

// AsyncJobRegistry maps opaque public job ids to the backend job id and the pod
// that owns it. Every operation is scoped to an owner, and pod resolution is an
// internal step of Get so no caller can route on a record it does not own.
//
// The gateway depends on this interface rather than a storage implementation.
// Tests use memoryAsyncJobRegistry, while production uses
// redisAsyncJobRegistry so records are visible to every gateway replica.
type AsyncJobRegistry interface {
	Register(ctx context.Context, reg AsyncJobRegistration) (AsyncJobRecord, error)
	Get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, *v1.Pod, error)
	List(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error)
	Delete(ctx context.Context, owner, publicJobID string) error
}

// asyncJobRegistryCore contains behavior shared by the memory and Redis
// registries. Storage selection stays in the concrete implementations so
// callers cannot accidentally construct a production registry with the test
// store.
type asyncJobRegistryCore struct {
	store       asyncJobStore
	pods        podResolver
	now         func() time.Time
	newPublicID func() (string, error)
}

func newAsyncJobRegistryCore(store asyncJobStore, pods podResolver) *asyncJobRegistryCore {
	return &asyncJobRegistryCore{
		store:       store,
		pods:        pods,
		now:         time.Now,
		newPublicID: newAsyncJobPublicID,
	}
}

// memoryAsyncJobRegistry is the in-process implementation used by unit tests
// and standalone development. It must not be used by a multi-replica gateway.
type memoryAsyncJobRegistry struct {
	*asyncJobRegistryCore
}

func newMemoryAsyncJobRegistry(pods podResolver) *memoryAsyncJobRegistry {
	return &memoryAsyncJobRegistry{
		asyncJobRegistryCore: newAsyncJobRegistryCore(newInMemoryAsyncJobStore(), pods),
	}
}

// redisAsyncJobRegistry is the production implementation. Its records are
// shared by all gateway replicas through Redis.
type redisAsyncJobRegistry struct {
	*asyncJobRegistryCore
}

func newRedisAsyncJobRegistry(client redis.Cmdable, pods podResolver) *redisAsyncJobRegistry {
	return &redisAsyncJobRegistry{
		asyncJobRegistryCore: newAsyncJobRegistryCore(newRedisAsyncJobStore(client), pods),
	}
}

var (
	_ AsyncJobRegistry = (*memoryAsyncJobRegistry)(nil)
	_ AsyncJobRegistry = (*redisAsyncJobRegistry)(nil)
)

func (r *memoryAsyncJobRegistry) Register(ctx context.Context, reg AsyncJobRegistration) (AsyncJobRecord, error) {
	return r.register(ctx, reg)
}

func (r *memoryAsyncJobRegistry) Get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, *v1.Pod, error) {
	return r.get(ctx, owner, publicJobID)
}

func (r *memoryAsyncJobRegistry) List(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error) {
	return r.list(ctx, owner, jobType, options)
}

func (r *memoryAsyncJobRegistry) Delete(ctx context.Context, owner, publicJobID string) error {
	return r.delete(ctx, owner, publicJobID)
}

func (r *redisAsyncJobRegistry) Register(ctx context.Context, reg AsyncJobRegistration) (AsyncJobRecord, error) {
	return r.register(ctx, reg)
}

func (r *redisAsyncJobRegistry) Get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, *v1.Pod, error) {
	return r.get(ctx, owner, publicJobID)
}

func (r *redisAsyncJobRegistry) List(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error) {
	return r.list(ctx, owner, jobType, options)
}

func (r *redisAsyncJobRegistry) Delete(ctx context.Context, owner, publicJobID string) error {
	return r.delete(ctx, owner, publicJobID)
}

// Register durably records a job the backend has already accepted, and returns
// the record holding the public id to hand back to the client. The record is in
// the store before this returns: there is no gateway-replica cache in front of
// it, so any replica can serve the follow-up requests.
func (r *asyncJobRegistryCore) register(ctx context.Context, reg AsyncJobRegistration) (AsyncJobRecord, error) {
	if reg.Pod == nil {
		return AsyncJobRecord{}, newAsyncJobInvalidRecordError("routing target pod is required")
	}

	now := r.now()
	expiresAt := reg.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(defaultAsyncJobTTL)
	}
	// A backend expiry is applied as reported, never stretched: a job the backend
	// says is already over has no output left to route to, and giving it the
	// default TTL would pin a pod for a week on behalf of a dead job. Refusing
	// before a public id exists also means no client can be handed one.
	if !expiresAt.After(now) {
		return AsyncJobRecord{}, newAsyncJobInvalidRecordError("expiry %s has already elapsed", expiresAt.UTC().Format(time.RFC3339))
	}

	publicJobID, err := r.newPublicID()
	if err != nil {
		return AsyncJobRecord{}, err
	}

	record := AsyncJobRecord{
		PublicJobID:  publicJobID,
		JobType:      reg.JobType,
		Owner:        reg.Owner,
		Model:        reg.Model,
		BackendJobID: reg.BackendJobID,
		RoutingTarget: AsyncJobRoutingTarget{
			Kind: asyncJobTargetKindPod,
			Pod: AsyncJobPodTarget{
				Namespace: reg.Pod.Namespace,
				Name:      reg.Pod.Name,
				UID:       string(reg.Pod.UID),
			},
		},
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := record.validate(); err != nil {
		return AsyncJobRecord{}, err
	}
	if err := r.store.put(ctx, record); err != nil {
		return AsyncJobRecord{}, err
	}
	return record, nil
}

// Get resolves a public job id for one owner, returning the record and the pod
// that follow-up requests must be pinned to.
//
// A record whose pod can never come back (gone from the informer cache,
// terminating, or replaced by a pod of the same name and a different UID) is
// deleted here: keeping it would only produce a 503 forever. That deletion is
// part of the answer - if it fails, the caller is told to retry rather than told
// the job is gone.
func (r *asyncJobRegistryCore) get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, *v1.Pod, error) {
	record, err := r.store.get(ctx, owner, publicJobID)
	if err != nil {
		return AsyncJobRecord{}, nil, err
	}

	pod, err := r.resolvePin(record)
	if err != nil {
		if errors.Is(err, errAsyncJobNotFound) {
			// The record is garbage, but only once the store agrees. Answering
			// "gone" while it is still durable would tell the client to stop
			// asking, and the next request would pin the same dead pod again, so
			// a cleanup that could not complete is reported as retryable instead.
			if delErr := r.store.delete(ctx, owner, publicJobID); delErr != nil {
				klog.ErrorS(delErr, "failed to delete unroutable async job record",
					"public_job_id", publicJobID, "job_type", record.JobType)
				return record, nil, fmt.Errorf("%w: could not drop the unroutable record of %s: %v",
					errAsyncJobStoreUnavailable, publicJobID, delErr)
			}
		}
		// The record itself is still returned: the caller needs its model to
		// attribute the failure it is about to report, and the record is the only
		// place that knows it.
		return record, nil, err
	}
	return record, pod, nil
}

// List returns a bounded cursor page for one owner's records of one job type.
// It is a registry read only: no pod is resolved and no backend is contacted,
// so a listing never reflects live job status. Stores that predate cursor
// support fall back to the legacy list implementation; production never takes
// that path.
func (r *asyncJobRegistryCore) list(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error) {
	options, err := normalizeAsyncJobListOptions(options)
	if err != nil {
		return AsyncJobListPage{}, err
	}
	if pageStore, ok := r.store.(asyncJobPageStore); ok {
		return pageStore.listPage(ctx, owner, jobType, options)
	}

	records, err := r.store.list(ctx, owner, jobType)
	if err != nil {
		return AsyncJobListPage{}, err
	}
	return paginateAsyncJobRecords(records, options)
}

// Delete forgets a job. It is idempotent, so the cleanup that follows a backend
// delete can be retried.
func (r *asyncJobRegistryCore) delete(ctx context.Context, owner, publicJobID string) error {
	return r.store.delete(ctx, owner, publicJobID)
}

// resolvePin turns a recorded pod identity back into a live pod. It is
// unexported so that resolution cannot be reached without passing Get's
// ownership check first.
func (r *asyncJobRegistryCore) resolvePin(record AsyncJobRecord) (*v1.Pod, error) {
	if record.RoutingTarget.Kind != asyncJobTargetKindPod {
		return nil, fmt.Errorf("%w: unroutable target kind %q", errAsyncJobNotFound, record.RoutingTarget.Kind)
	}

	target := record.RoutingTarget.Pod
	pod, err := r.pods.GetPod(target.Name, target.Namespace)
	if err != nil || pod == nil {
		return nil, fmt.Errorf("%w: pod %s/%s is gone", errAsyncJobNotFound, target.Namespace, target.Name)
	}
	if string(pod.UID) != target.UID {
		return nil, fmt.Errorf("%w: pod %s/%s was replaced", errAsyncJobNotFound, target.Namespace, target.Name)
	}
	if utils.IsPodTerminating(pod) {
		return nil, fmt.Errorf("%w: pod %s/%s is terminating", errAsyncJobNotFound, target.Namespace, target.Name)
	}
	if !utils.IsPodReady(pod) {
		return nil, fmt.Errorf("%w: pod %s/%s is not ready", errAsyncJobTargetUnavailable, target.Namespace, target.Name)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("%w: pod %s/%s has no address", errAsyncJobTargetUnavailable, target.Namespace, target.Name)
	}
	return pod, nil
}

// inMemoryAsyncJobStore backs unit tests and single-replica local runs. It is
// never used by a multi-replica gateway: a job registered on one replica has to
// be resolvable on every other one.
type inMemoryAsyncJobStore struct {
	mu      sync.RWMutex
	records map[string]AsyncJobRecord
	now     func() time.Time
}

func newInMemoryAsyncJobStore() *inMemoryAsyncJobStore {
	return &inMemoryAsyncJobStore{
		records: map[string]AsyncJobRecord{},
		now:     time.Now,
	}
}

func inMemoryAsyncJobKey(owner, publicJobID string) string {
	return owner + "/" + publicJobID
}

func (s *inMemoryAsyncJobStore) put(_ context.Context, record AsyncJobRecord) error {
	if err := record.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[inMemoryAsyncJobKey(record.Owner, record.PublicJobID)] = record
	return nil
}

func (s *inMemoryAsyncJobStore) get(_ context.Context, owner, publicJobID string) (AsyncJobRecord, error) {
	now := s.now()
	s.mu.RLock()
	record, ok := s.records[inMemoryAsyncJobKey(owner, publicJobID)]
	s.mu.RUnlock()
	if !ok {
		return AsyncJobRecord{}, fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
	}
	if record.expired(now) {
		// Evict on read to prevent unbounded memory growth
		s.mu.Lock()
		delete(s.records, inMemoryAsyncJobKey(owner, publicJobID))
		s.mu.Unlock()
		return AsyncJobRecord{}, fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
	}
	return record, nil
}

func (s *inMemoryAsyncJobStore) list(_ context.Context, owner, jobType string) ([]AsyncJobRecord, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	// Evict expired records to bound memory growth
	for key, record := range s.records {
		if record.expired(now) {
			delete(s.records, key)
		}
	}

	records := make([]AsyncJobRecord, 0, len(s.records))
	for _, record := range s.records {
		if record.Owner != owner || record.JobType != jobType {
			continue
		}
		records = append(records, record)
	}

	sortAsyncJobRecords(records)
	return records, nil
}

func (s *inMemoryAsyncJobStore) listPage(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error) {
	records, err := s.list(ctx, owner, jobType)
	if err != nil {
		return AsyncJobListPage{}, err
	}
	return paginateAsyncJobRecords(records, options)
}

func (s *inMemoryAsyncJobStore) delete(_ context.Context, owner, publicJobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, inMemoryAsyncJobKey(owner, publicJobID))
	return nil
}

// The owner is part of the record key rather than a field to compare after the
// read: an unauthorized read then cannot be distinguished from a miss, and a
// wrong-owner delete cannot reach a record it does not own.
//
// The owner is also the Redis Cluster hash tag. Every key touched by one of the
// registry's multi-key Lua scripts therefore belongs to the same hash slot.
func asyncJobOwnerKeyPrefix(owner string) string {
	return asyncJobRedisKeyPrefix + "{" + owner + "}:"
}

func asyncJobRecordKeyPrefix(owner string) string {
	return asyncJobOwnerKeyPrefix(owner) + "record:"
}

func asyncJobRecordKey(owner, publicJobID string) string {
	return asyncJobRecordKeyPrefix(owner) + publicJobID
}

// isValidPublicJobID checks that a public job id is safe to use in Redis keys.
// It rejects ids containing key separators, whitespace, or control characters,
// but does not enforce a specific format to allow test flexibility.
func isValidPublicJobID(publicJobID string) bool {
	if publicJobID == "" || publicJobID == "sync" {
		return false
	}
	for _, r := range publicJobID {
		if r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func asyncJobIndexKey(owner, jobType string) string {
	return asyncJobOwnerKeyPrefix(owner) + "index:" + jobType
}

// asyncJobIndexesKey names the set of index keys one owner currently has jobs
// in. It exists so delete can reach a record's index without knowing its job
// type: the record's own TTL may have fired first, and the registry is generic -
// hard-coding the job types that own an index would leak an index member for
// every job type added later.
func asyncJobIndexesKey(owner string) string {
	return asyncJobOwnerKeyPrefix(owner) + "indexes"
}

// asyncJobRegisterScript writes the record and its index membership in one
// round trip so a reader can never see one without the other. It is idempotent:
// re-running it for the same public id overwrites the record and updates the
// same ZSET member, so a retried registration cannot duplicate a listing.
//
// The index inherits the longest TTL of its members, so it disappears on its
// own once the last job in it has expired. PTTL returns -1 for a key with no
// expiry and -2 for a missing key; both compare below any real TTL, which is
// exactly the "needs an expiry" case.
var asyncJobRegisterScript = redis.NewScript(`
local ttl = tonumber(ARGV[3])
redis.call('SET', KEYS[1], ARGV[2], 'PX', ttl)
redis.call('ZADD', KEYS[2], ARGV[4], ARGV[1])
redis.call('SADD', KEYS[3], KEYS[2])
for i = 2, 3 do
  local keyTTL = tonumber(redis.call('PTTL', KEYS[i]))
  if keyTTL < ttl then
    redis.call('PEXPIRE', KEYS[i], ttl)
  end
end
return 1
`)

// asyncJobDeleteScript drops the record and every index membership together, so
// a delete cannot leave a member pointing at nothing. KEYS[1] is the record and
// KEYS[2] is the owner's set of index keys, which is what makes the cleanup work
// for any job type - including one whose record has already expired, and one the
// gateway has no constant for yet.
//
// The index keys are read out of KEYS[2] rather than passed in, so all of an
// owner's keys have to live on one node; that holds for the single Redis this
// gateway talks to, which is also what the multi-key register script assumes.
// An index that lost its last member is deleted by Redis itself, so an EXISTS of
// 0 is the signal to forget it.
var asyncJobDeleteScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
local indexes = redis.call('SMEMBERS', KEYS[2])
for i = 1, #indexes do
  redis.call('ZREM', indexes[i], ARGV[1])
  if redis.call('EXISTS', indexes[i]) == 0 then
    redis.call('SREM', KEYS[2], indexes[i])
  end
end
return 1
`)

// asyncJobListPageScript performs cursor lookup, stale-member pruning and page
// assembly atomically. Keeping those steps in one script prevents concurrent
// registrations or deletions from shifting a rank between separate commands.
//
// Redis sorted sets have no member-level TTL. Record keys do, so the script
// treats a missing or invalid record as stale, removes its index member, and
// continues scanning until it has limit+1 live records. max_scan bounds server
// work because canceling the client context cannot interrupt a running script.
var asyncJobListPageScript = redis.NewScript(`
local index_key = KEYS[1]
local indexes_key = KEYS[2]
local owner = ARGV[1]
local job_type = ARGV[2]
local after = ARGV[3]
local limit = tonumber(ARGV[4])
local order = ARGV[5]
local max_scan = tonumber(ARGV[6])
local batch_size = tonumber(ARGV[7])
local record_prefix = ARGV[8]

local reverse = order == 'desc'
local start = 0
if after ~= '' then
  local rank
  if reverse then
    rank = redis.call('ZREVRANK', index_key, after)
  else
    rank = redis.call('ZRANK', index_key, after)
  end
  if not rank then
    return {1, 0, 0}
  end
  start = rank + 1
end

local function valid_record(payload, member)
  if not payload then
    return false
  end
  local ok, record = pcall(cjson.decode, payload)
  if not ok or type(record) ~= 'table' then
    return false
  end
  if record.public_job_id ~= member or record.owner ~= owner or record.job_type ~= job_type then
    return false
  end
  if type(record.backend_job_id) ~= 'string' or record.backend_job_id == '' then
    return false
  end
  local target = record.routing_target
  if type(target) ~= 'table' or target.kind ~= 'pod' or type(target.pod) ~= 'table' then
    return false
  end
  if type(target.pod.namespace) ~= 'string' or target.pod.namespace == '' or
     type(target.pod.name) ~= 'string' or target.pod.name == '' or
     type(target.pod.uid) ~= 'string' or target.pod.uid == '' then
    return false
  end
  return type(record.expires_at) == 'string' and record.expires_at ~= ''
end

local target_count = limit + 1
local payloads = {}
local scanned = 0

while #payloads < target_count and scanned < max_scan do
  local take = math.min(batch_size, max_scan - scanned)
  local members
  if reverse then
    members = redis.call('ZREVRANGE', index_key, start, start + take - 1)
  else
    members = redis.call('ZRANGE', index_key, start, start + take - 1)
  end
  if #members == 0 then
    break
  end

  local stale = 0
  for i = 1, #members do
    local member = members[i]
    local payload = redis.call('GET', record_prefix .. member)
    if valid_record(payload, member) then
      table.insert(payloads, payload)
    else
      redis.call('ZREM', index_key, member)
      stale = stale + 1
    end
    if #payloads >= target_count then
      break
    end
  end

  scanned = scanned + #members
  start = start + #members - stale
  if #members < take then
    break
  end
end

if redis.call('ZCARD', index_key) == 0 then
  redis.call('SREM', indexes_key, index_key)
end

if #payloads < target_count and scanned >= max_scan and redis.call('ZCARD', index_key) > start then
  return {2, 0, scanned}
end

local has_more = 0
if #payloads > limit then
  has_more = 1
end
local result = {0, has_more, scanned}
for i = 1, math.min(limit, #payloads) do
  table.insert(result, payloads[i])
end
return result
`)

// redisAsyncJobStore is the production store. Registry operations read and
// write Redis directly - there is no per-replica job cache to go stale, to warm
// up after a restart, or to make one replica disagree with another.
type redisAsyncJobStore struct {
	// redis.Cmdable covers both the plain commands and the scripting subset, so
	// a *redis.Client and a cluster client are equally acceptable here.
	client redis.Cmdable
	now    func() time.Time
	retry  asyncJobRetryPolicy
}

func newRedisAsyncJobStore(client redis.Cmdable) *redisAsyncJobStore {
	return &redisAsyncJobStore{
		client: client,
		now:    time.Now,
		retry:  defaultAsyncJobRetryPolicy(),
	}
}

// newAsyncJobRegistryForClient picks the registry a Server should use. Without
// Redis (tests and standalone local development), records live in this process
// only. Production gateways use the Redis implementation so every replica sees
// the same records.
func newAsyncJobRegistryForClient(client *redis.Client, pods podResolver) AsyncJobRegistry {
	if client == nil {
		return newMemoryAsyncJobRegistry(pods)
	}
	return newRedisAsyncJobRegistry(asyncJobStoreClient(client), pods)
}

// asyncJobStoreClient derives the client the store uses from the gateway's
// shared one, with go-redis' own retry loop switched off.
//
// go-redis retries a failed command three times internally by default, which
// this package cannot see: retryAsyncJobStoreOp would then be counting logical
// operations while the wire carried up to twelve attempts, and the attempt
// budget would mean nothing. The rest of the gateway keeps the shared client's
// defaults - only this store owns a retry policy of its own.
//
// MaxRetries is -1 rather than 0 because go-redis reads 0 as "use the default".
func asyncJobStoreClient(client *redis.Client) *redis.Client {
	options := *client.Options()
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	return redis.NewClient(&options)
}

// asyncJobRegistry returns the Server's registry, building it on first use. The
// lazy path exists for Servers assembled as struct literals (tests, and any
// caller that does not go through NewServerWithOptions).
func (s *Server) asyncJobRegistry() AsyncJobRegistry {
	s.asyncJobsOnce.Do(func() {
		if s.asyncJobs == nil {
			s.asyncJobs = newAsyncJobRegistryForClient(s.redisClient, s.cache)
		}
	})
	return s.asyncJobs
}

func (s *redisAsyncJobStore) put(ctx context.Context, record AsyncJobRecord) error {
	if err := record.validate(); err != nil {
		return err
	}
	ttl := record.ExpiresAt.Sub(s.now())
	if ttl <= 0 {
		return newAsyncJobInvalidRecordError("expiry %s has already elapsed", record.ExpiresAt)
	}
	payload, err := sonic.Marshal(record)
	if err != nil {
		return newAsyncJobInvalidRecordError("marshal record: %v", err)
	}

	keys := []string{
		asyncJobRecordKey(record.Owner, record.PublicJobID),
		asyncJobIndexKey(record.Owner, record.JobType),
		asyncJobIndexesKey(record.Owner),
	}
	return retryAsyncJobStoreOp(ctx, s.retry, "put", func(ctx context.Context) error {
		return asyncJobRegisterScript.Run(ctx, s.client, keys,
			record.PublicJobID, payload, ttl.Milliseconds(), record.CreatedAt.UnixMicro()).Err()
	})
}

func (s *redisAsyncJobStore) get(ctx context.Context, owner, publicJobID string) (AsyncJobRecord, error) {
	if !isValidPublicJobID(publicJobID) {
		return AsyncJobRecord{}, fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
	}
	var record AsyncJobRecord
	err := retryAsyncJobStoreOp(ctx, s.retry, "get", func(ctx context.Context) error {
		payload, err := s.client.Get(ctx, asyncJobRecordKey(owner, publicJobID)).Bytes()
		if errors.Is(err, redis.Nil) {
			return fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
		}
		if err != nil {
			return err
		}
		decoded, err := decodeAsyncJobRecord(payload)
		if err != nil {
			return err
		}
		record = decoded
		return nil
	})
	if err != nil {
		return AsyncJobRecord{}, err
	}
	// Redis expires the key on its own; the logical check covers the window
	// between the expiry passing and the eviction landing.
	if record.expired(s.now()) {
		return AsyncJobRecord{}, fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
	}
	return record, nil
}

// list reads the owner+job_type index rather than scanning the keyspace: a SCAN
// over a shared Redis grows with every other tenant's jobs, and cannot be made
// to answer "this owner's jobs" cheaply.
func (s *redisAsyncJobStore) list(ctx context.Context, owner, jobType string) ([]AsyncJobRecord, error) {
	page, err := s.listPage(ctx, owner, jobType, AsyncJobListOptions{Limit: maxAsyncJobListLimit, Order: "desc"})
	if err != nil {
		return nil, err
	}
	return page.Records, nil
}

// listPage reads one bounded range of the owner+job-type ZSET. Unlike SMEMBERS
// this keeps Redis, the gateway heap, and the ext_proc response bounded even
// when an owner has years of retained jobs.
func (s *redisAsyncJobStore) listPage(ctx context.Context, owner, jobType string, options AsyncJobListOptions) (AsyncJobListPage, error) {
	indexKey := asyncJobIndexKey(owner, jobType)
	var raw []interface{}
	if err := retryAsyncJobStoreOp(ctx, s.retry, "list_page", func(ctx context.Context) error {
		var err error
		raw, err = asyncJobListPageScript.Run(ctx, s.client,
			[]string{indexKey, asyncJobIndexesKey(owner)},
			owner, jobType, options.After, options.Limit, options.Order,
			asyncJobListMaxScan, asyncJobListScanBatch, asyncJobRecordKeyPrefix(owner)).Slice()
		return err
	}); err != nil {
		return AsyncJobListPage{}, err
	}
	if len(raw) < 3 {
		return AsyncJobListPage{}, newAsyncJobInvalidRecordError("malformed list page result")
	}
	status, ok := raw[0].(int64)
	if !ok {
		return AsyncJobListPage{}, newAsyncJobInvalidRecordError("malformed list page status")
	}
	scanned, _ := raw[2].(int64)
	switch status {
	case 1:
		return AsyncJobListPage{}, fmt.Errorf("%w: list cursor", errAsyncJobNotFound)
	case 2:
		return AsyncJobListPage{}, fmt.Errorf("%w: list scan exhausted after %d candidates", errAsyncJobStoreUnavailable, scanned)
	case 0:
	default:
		return AsyncJobListPage{}, newAsyncJobInvalidRecordError("unknown list page status %d", status)
	}

	hasMore, ok := raw[1].(int64)
	if !ok {
		return AsyncJobListPage{}, newAsyncJobInvalidRecordError("malformed list page continuation")
	}
	page := AsyncJobListPage{HasMore: hasMore == 1, Records: make([]AsyncJobRecord, 0, len(raw)-3)}
	for _, item := range raw[3:] {
		payload, ok := item.(string)
		if !ok {
			return AsyncJobListPage{}, newAsyncJobInvalidRecordError("malformed list page record")
		}
		record, err := decodeAsyncJobRecord([]byte(payload))
		if err != nil {
			return AsyncJobListPage{}, err
		}
		if err := record.validate(); err != nil || record.Owner != owner || record.JobType != jobType {
			return AsyncJobListPage{}, newAsyncJobInvalidRecordError("inconsistent list page record")
		}
		page.Records = append(page.Records, record)
	}
	return page, nil
}

func (s *redisAsyncJobStore) delete(ctx context.Context, owner, publicJobID string) error {
	if !isValidPublicJobID(publicJobID) {
		return fmt.Errorf("%w: %s", errAsyncJobNotFound, publicJobID)
	}
	keys := []string{
		asyncJobRecordKey(owner, publicJobID),
		asyncJobIndexesKey(owner),
	}
	return retryAsyncJobStoreOp(ctx, s.retry, "delete", func(ctx context.Context) error {
		return asyncJobDeleteScript.Run(ctx, s.client, keys, publicJobID).Err()
	})
}

func decodeAsyncJobRecord(payload []byte) (AsyncJobRecord, error) {
	var record AsyncJobRecord
	if err := sonic.Unmarshal(payload, &record); err != nil {
		return AsyncJobRecord{}, newAsyncJobInvalidRecordError("unmarshal record: %v", err)
	}
	return record, nil
}

// sortAsyncJobRecords gives listings a stable newest-first order. v1 exposes no
// pagination, so this is presentation only, not a cursor.
func sortAsyncJobRecords(records []AsyncJobRecord) {
	sort.Slice(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.After(records[j].CreatedAt)
		}
		return records[i].PublicJobID < records[j].PublicJobID
	})
}

func paginateAsyncJobRecords(records []AsyncJobRecord, options AsyncJobListOptions) (AsyncJobListPage, error) {
	if options.Order == "asc" {
		for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
			records[i], records[j] = records[j], records[i]
		}
	}
	start := 0
	if options.After != "" {
		start = -1
		for i, record := range records {
			if record.PublicJobID == options.After {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return AsyncJobListPage{}, fmt.Errorf("%w: list cursor", errAsyncJobNotFound)
		}
	}
	if start >= len(records) {
		return AsyncJobListPage{}, nil
	}
	end := start + options.Limit
	page := AsyncJobListPage{}
	if end < len(records) {
		page.HasMore = true
	} else {
		end = len(records)
	}
	page.Records = records[start:end]
	return page, nil
}

// asyncJobRetryPolicy bounds how long a store operation may keep trying. The
// deadline matters more than the attempt count: these calls happen inside an
// ext_proc exchange that Envoy is timing, so a retry loop that outlives the
// filter's response deadline turns a recoverable blip into a dropped request.
type asyncJobRetryPolicy struct {
	maxAttempts int
	baseBackoff time.Duration
	deadline    time.Duration
}

func defaultAsyncJobRetryPolicy() asyncJobRetryPolicy {
	return asyncJobRetryPolicy{
		maxAttempts: asyncJobStoreMaxAttempts,
		baseBackoff: asyncJobStoreBaseBackoff,
		deadline:    asyncJobStoreRetryDeadline,
	}
}

// retryAsyncJobStoreOp retries only failures classified as transient. A
// validation, serialization, auth or script failure is returned as it is, so it
// surfaces as the permanent error it is instead of eating the retry budget.
func retryAsyncJobStoreOp(ctx context.Context, policy asyncJobRetryPolicy, op string, fn func(context.Context) error) error {
	started := time.Now()

	// Every attempt and every backoff runs under one deadline, so the bound is
	// the whole operation rather than the sleeps between attempts: a Redis call
	// that hangs would otherwise outlive the ext_proc exchange no matter how few
	// attempts are left. Deriving it from ctx keeps whichever is shorter - the
	// policy's budget or what the caller's deadline still has.
	opCtx, cancel := context.WithTimeout(ctx, policy.deadline)
	defer cancel()

	var lastErr error
	for attempt := 1; ; attempt++ {
		lastErr = fn(opCtx)
		if lastErr == nil {
			return nil
		}
		// A miss and a rejected record are answers, not failures: they stand even
		// when the budget happens to have run out at the same moment.
		if errors.Is(lastErr, errAsyncJobNotFound) || errors.Is(lastErr, errAsyncJobInvalidRecord) {
			return lastErr
		}
		// Out of time. Whether the store said so or the deadline fired mid-call,
		// the operation never got an answer, which is retryable for the client.
		if opCtx.Err() != nil {
			break
		}
		if !isTransientRedisError(lastErr) {
			return lastErr
		}
		if attempt >= policy.maxAttempts {
			break
		}
		backoff := asyncJobRetryBackoff(policy.baseBackoff, attempt)
		if time.Since(started)+backoff >= policy.deadline {
			break
		}
		select {
		case <-opCtx.Done():
			return fmt.Errorf("%w: %s: %v", errAsyncJobStoreUnavailable, op, lastErr)
		case <-time.After(backoff):
		}
	}
	klog.V(4).InfoS("async job store operation exhausted its retry budget",
		"op", op, "elapsed", time.Since(started), "error", lastErr)
	return fmt.Errorf("%w: %s: %v", errAsyncJobStoreUnavailable, op, lastErr)
}

// asyncJobRetryBackoff grows exponentially with jitter so that a Redis failover
// does not have every in-flight request retrying in lockstep.
func asyncJobRetryBackoff(base time.Duration, attempt int) time.Duration {
	backoff := base << (attempt - 1)
	return backoff/2 + time.Duration(mrand.Int64N(int64(backoff)))/2
}

// isTransientRedisError classifies a store failure as worth retrying. It errs
// towards "permanent": a wrong answer here either wastes the request deadline
// or reports a recoverable blip as a hard failure.
func isTransientRedisError(err error) bool {
	if err == nil {
		return false
	}
	// A nil reply is an answer, not a failure, and the registry's own errors are
	// decisions that will not change on a second try.
	if errors.Is(err, redis.Nil) ||
		errors.Is(err, errAsyncJobNotFound) ||
		errors.Is(err, errAsyncJobInvalidRecord) {
		return false
	}
	// The caller gave up or ran out of time; retrying cannot help it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var serverErr redis.Error
	if errors.As(err, &serverErr) {
		msg := strings.TrimPrefix(serverErr.Error(), "ERR ")
		for _, prefix := range transientRedisServerErrorPrefixes {
			if strings.HasPrefix(msg, prefix) {
				return true
			}
		}
		return false
	}

	// Everything below is the connection dropping under us: a closed pool, a
	// failover, or a node restart.
	if errors.Is(err, redis.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, target := range []error{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.EPIPE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
