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

package discovery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEtcdProviderPrefixBoundary(t *testing.T) {
	for _, tc := range []struct {
		prefix, want string
		invalid      bool
	}{
		{"", "/aibrix/endpoints/", false},
		{"/workers", "/workers/", false},
		{"/workers/", "/workers/", false},
		{"/", "", true}, {" ", "", true}, {" /workers", "", true},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			p, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{"localhost:2379"}, Prefix: tc.prefix})
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, p.config.Prefix)
		})
	}
}

func TestEtcdProviderRejectsPlaintextCredentials(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		tls      *tls.Config
		valid    bool
	}{
		{"http://localhost:2379", nil, false},
		{"http://localhost:2379", &tls.Config{}, false},
		{"localhost:2379", nil, false},
		{"localhost:2379", &tls.Config{}, true},
		{"https://localhost:2379", nil, true},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			_, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{tc.endpoint}, TLS: tc.tls, Username: "gateway", Password: "secret"})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "encrypted")
			}
		})
	}
	_, err := NewEtcdProvider(EtcdConfig{Endpoints: []string{"https://localhost:2379"}, Password: "secret"})
	require.ErrorContains(t, err, "requires username")
}

func TestEtcdCompactionResnapshotStartsImmediately(t *testing.T) {
	client := newFakeEtcdClient()
	provider := fakeEtcdProvider(t, client)
	_, _, stream := startFakeEtcdWatch(t, provider, client, 10)
	stream <- clientv3.WatchResponse{Canceled: true, CompactRevision: 50}
	select {
	case request := <-client.gets:
		request.reply <- fakeEtcdGetResult{response: etcdSnapshot(51)}
	case <-time.After(etcdRetryDelay / 2):
		t.Fatal("first compaction recovery waited for the reconnect backoff")
	}
}

func TestEtcdFailureDiagnosticsDoNotExposePayloads(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{nil, "stream_closed"},
		{status.Error(codes.Unauthenticated, "secret-password"), "authentication"},
		{status.Error(codes.PermissionDenied, "/private/key"), "permission"},
		{status.Error(codes.Unavailable, "tls: secret certificate at private.example"), "tls"},
		{status.Error(codes.Unavailable, "dial private.example:2379"), "transport"},
		{fmt.Errorf("secret: %w", context.DeadlineExceeded), "timeout"},
		{context.Canceled, "canceled"},
		{errors.New("arbitrary secret payload"), "other"},
	} {
		require.Equal(t, tc.reason, etcdFailureReason(tc.err))
	}
}
