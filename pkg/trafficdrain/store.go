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

// Package trafficdrain coordinates admission and scale-in on one Redis primary.
// Missing state, lost replies and incomplete requests never imply an idle Pod.
package trafficdrain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	Accepting        int64 = 1
	Draining         int64 = 2
	Sealed           int64 = 3
	operationTimeout       = time.Second
)

var (
	ErrUnknown   = errors.New("traffic drain state is unknown")
	ErrClosed    = errors.New("traffic drain admission is closed")
	ErrConflict  = errors.New("traffic drain attempt changed or is not idle")
	ErrExpired   = errors.New("traffic drain attempt expired")
	ErrRestarted = errors.New("traffic drain Redis incarnation changed")
)

type Snapshot struct {
	Phase      int64
	Active     int64
	Generation int64
	DeadlineMS int64
	RetryAtMS  int64
}

// Store owns a separate pool. Every connection verifies the same Redis run_id;
// an epoch also carries that identity so a new process cannot trust restored state
// from a previous Redis incarnation. Automatic primary failover is not supported.
type Store struct {
	client *redis.Client
	boot   atomic.Pointer[string]
}

func New(source *redis.Client) *Store {
	if source == nil {
		return &Store{}
	}
	return newWithOptions(*source.Options())
}

func newWithOptions(opts redis.Options) *Store {
	s := &Store{}
	prior := opts.OnConnect
	opts.MaxRetries = -1
	opts.ContextTimeoutEnabled = true
	opts.ReadTimeout, opts.WriteTimeout = operationTimeout, operationTimeout
	opts.OnConnect = func(ctx context.Context, conn *redis.Conn) error {
		if prior != nil {
			if err := prior(ctx, conn); err != nil {
				return err
			}
		}
		info, err := conn.Info(ctx, "server").Result()
		if err != nil {
			return err
		}
		var id string
		for _, line := range strings.Split(info, "\n") {
			if strings.HasPrefix(line, "run_id:") {
				id = strings.TrimSpace(strings.TrimPrefix(line, "run_id:"))
				break
			}
		}
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 20 {
			return ErrUnknown
		}
		s.boot.CompareAndSwap(nil, &id)
		if *s.boot.Load() != id {
			return ErrRestarted
		}
		return nil
	}
	s.client = redis.NewClient(&opts)
	return s
}

func (s *Store) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func NewToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func key(uid string) string { return "aibrix:traffic-drain:{" + uid + "}" }

func (s *Store) identity(ctx context.Context) (string, error) {
	if s == nil || s.client == nil {
		return "", ErrUnknown
	}
	if s.boot.Load() == nil {
		if err := s.client.Ping(ctx).Err(); err != nil {
			return "", err
		}
	}
	if id := s.boot.Load(); id != nil {
		return *id, nil
	}
	return "", ErrUnknown
}

const initializeScript = `
if redis.call('EXISTS', KEYS[1]) ~= 0 then
  return redis.call('HGET', KEYS[1], 'epoch') or ''
end
redis.call('HSET', KEYS[1], 'epoch', ARGV[1], 'phase', 1, 'active', 0,
  'generation', 0, 'deadline', 0, 'retry', 0)
return ARGV[1]
`

// Initialize may only be used before the Pod's epoch annotation is published.
// The Gateway refuses admission until that annotation is present. A published
// epoch whose hash disappeared must never be initialized again.
func (s *Store) Initialize(ctx context.Context, uid string) (string, error) {
	if uid == "" {
		return "", ErrUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	boot, err := s.identity(ctx)
	if err != nil {
		return "", err
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	epoch, err := s.client.Eval(ctx, initializeScript, []string{key(uid)}, boot+":"+token).Text()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(epoch, boot+":") {
		return "", ErrUnknown
	}
	return epoch, nil
}

// Metadata and request tokens share ONE hash: eviction cannot lose only the
// request set and leave behind an apparently valid zero. There is no request TTL
// or heartbeat exclusion. An uncertain request pins the attempt until timeout.
const transitionScript = `
local v = redis.call('HMGET', KEYS[1], 'epoch', 'phase', 'active', 'generation', 'deadline', 'retry')
if v[1] ~= ARGV[1] then return {-1,0,0,0,0,0} end
local p,a,g,d,r = tonumber(v[2]),tonumber(v[3]),tonumber(v[4]),tonumber(v[5]),tonumber(v[6])
if not p or not a or not g or not d or not r or a < 0 or a ~= math.floor(a) then return {-1,0,0,0,0,0} end
if (p ~= 1 and p ~= 2 and p ~= 3) or (p == 3 and a ~= 0) or redis.call('HLEN', KEYS[1]) ~= a+6 then return {-1,0,0,0,0,0} end
local function result(code) return {code,p,a,g,d,r} end
local op = ARGV[2]
local tm = redis.call('TIME')
local now = tonumber(tm[1])*1000 + math.floor(tonumber(tm[2])/1000)
if op == 'read' then
  if p == 2 and now >= d then return result(2) end
  return result(1)
end
if op == 'admit' then
  local token = 'request:'..ARGV[3]
  if redis.call('HEXISTS', KEYS[1], token) == 1 then return result(1) end
  if p ~= 1 then return result(0) end
  redis.call('HSET', KEYS[1], token, 1)
  a = redis.call('HINCRBY', KEYS[1], 'active', 1)
  return result(1)
end
if op == 'release' then
  local token = 'request:'..ARGV[3]
  if redis.call('HEXISTS', KEYS[1], token) == 0 then return result(1) end
  if a == 0 then return result(-1) end
  redis.call('HDEL', KEYS[1], token)
  a = redis.call('HINCRBY', KEYS[1], 'active', -1)
  return result(1)
end
if op == 'start' then
  if p == 2 and now >= d then return result(2) end
  if p == 2 or p == 3 then return result(1) end
  if p ~= 1 or now < r then return result(0) end
  p,g,d = 2,g+1,now+tonumber(ARGV[3])
  redis.call('HSET', KEYS[1], 'phase', p, 'generation', g, 'deadline', d)
  return result(1)
end
if op == 'cancel' then
  if p == 3 or g ~= tonumber(ARGV[3]) then return result(0) end
  if p == 2 then
    p,d,r = 1,0,now+tonumber(ARGV[4])
    redis.call('HSET', KEYS[1], 'phase', p, 'deadline', d, 'retry', r)
  end
  return result(1)
end
if op == 'seal' then
  if g ~= tonumber(ARGV[3]) then return result(0) end
  if p == 3 then return result(1) end
  if p ~= 2 then return result(0) end
  if now >= d then return result(2) end
  if a ~= 0 then return result(0) end
  p = 3
  redis.call('HSET', KEYS[1], 'phase', p)
  return result(1)
end
if op == 'forget' then
  if p ~= 3 or a ~= 0 then return result(0) end
  redis.call('DEL', KEYS[1])
  return result(1)
end
return result(-1)
`

func (s *Store) call(ctx context.Context, uid, epoch, op string, args ...interface{}) (Snapshot, error) {
	var snapshot Snapshot
	if uid == "" || epoch == "" {
		return snapshot, ErrUnknown
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	boot, err := s.identity(ctx)
	if err != nil {
		return snapshot, err
	}
	if !strings.HasPrefix(epoch, boot+":") {
		return snapshot, ErrRestarted
	}
	values := append([]interface{}{epoch, op}, args...)
	result, err := s.client.Eval(ctx, transitionScript, []string{key(uid)}, values...).Slice()
	if err != nil {
		return snapshot, err
	}
	if len(result) != 6 {
		return snapshot, ErrUnknown
	}
	var numbers [6]int64
	for i, value := range result {
		n, ok := value.(int64)
		if !ok {
			return snapshot, ErrUnknown
		}
		numbers[i] = n
	}
	snapshot = Snapshot{numbers[1], numbers[2], numbers[3], numbers[4], numbers[5]}
	switch numbers[0] {
	case 1:
		return snapshot, nil
	case 0:
		return snapshot, ErrConflict
	case 2:
		return snapshot, ErrExpired
	default:
		return snapshot, ErrUnknown
	}
}

func (s *Store) Read(ctx context.Context, uid, epoch string) (Snapshot, error) {
	return s.call(ctx, uid, epoch, "read")
}
func (s *Store) Start(ctx context.Context, uid, epoch string, maxWait time.Duration) (Snapshot, error) {
	if maxWait <= 0 {
		return Snapshot{}, ErrUnknown
	}
	return s.call(ctx, uid, epoch, "start", maxWait.Milliseconds())
}
func (s *Store) Cancel(ctx context.Context, uid, epoch string, generation int64, cooldown time.Duration) (Snapshot, error) {
	if cooldown < 0 {
		return Snapshot{}, ErrUnknown
	}
	return s.call(ctx, uid, epoch, "cancel", generation, cooldown.Milliseconds())
}
func (s *Store) Seal(ctx context.Context, uid, epoch string, generation int64) (Snapshot, error) {
	return s.call(ctx, uid, epoch, "seal", generation)
}
func (s *Store) Forget(ctx context.Context, uid, epoch string) error {
	_, err := s.call(ctx, uid, epoch, "forget")
	return err
}
func (s *Store) Admit(ctx context.Context, uid, epoch, token string) error {
	if token == "" || len(token) > 128 {
		return ErrUnknown
	}
	_, err := s.call(ctx, uid, epoch, "admit", token)
	if errors.Is(err, ErrConflict) {
		return ErrClosed
	}
	return err
}
func (s *Store) Release(ctx context.Context, uid, epoch, token string) error {
	if token == "" || len(token) > 128 {
		return ErrUnknown
	}
	_, err := s.call(ctx, uid, epoch, "release", token)
	return err
}

func (s Snapshot) String() string {
	return fmt.Sprintf("phase=%d active=%d generation=%d", s.Phase, s.Active, s.Generation)
}
