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

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateDiscoveryFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		standalone bool
		endpoints  string
		etcd       string
		wantError  string
	}{
		{name: "kubernetes"},
		{name: "existing kubernetes flags", endpoints: "ignored.yaml"},
		{name: "static", standalone: true, endpoints: "endpoints.yaml"},
		{name: "etcd", standalone: true, etcd: "etcd.yaml"},
		{name: "missing", standalone: true, wantError: "is required"},
		{name: "both", standalone: true, endpoints: "endpoints.yaml", etcd: "etcd.yaml", wantError: "mutually exclusive"},
		{name: "etcd without standalone", etcd: "etcd.yaml", wantError: "requires --standalone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDiscoveryFlags(tc.standalone, tc.endpoints, tc.etcd)
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
			}
		})
	}
}

func TestLoadEtcdConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"yaml", "endpoints:\n  - http://127.0.0.1:2379\n"},
		{"json", `{"endpoints":["http://127.0.0.1:2379"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadEtcdConfig(writeEtcdTestFile(t, t.TempDir(), "etcd.yaml", tc.content))
			require.NoError(t, err)
			require.Equal(t, []string{"http://127.0.0.1:2379"}, config.Endpoints)
			require.Equal(t, "/aibrix/endpoints/", config.Prefix)
			require.Equal(t, 5*time.Second, config.DialTimeout)
			require.Nil(t, config.TLS)
		})
	}
	path := writeEtcdTestFile(t, t.TempDir(), "etcd.yaml", `
endpoints: ["https://etcd.example:2379"]
prefix: /inference/workers/
dialTimeout: 2s
username: gateway
password: a-secret
`)
	config, err := loadEtcdConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/inference/workers/", config.Prefix)
	require.Equal(t, 2*time.Second, config.DialTimeout)
	require.Equal(t, "gateway", config.Username)
	require.Equal(t, "a-secret", config.Password)
	// HTTPS without an explicit CA uses the client's system trust roots.
	require.Nil(t, config.TLS)
}

func TestLoadEtcdConfigMultipleEndpoints(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			endpoints := []string{scheme + "://etcd-1.example:2379", scheme + "://etcd-2.example:2379"}
			content := "endpoints: [" + endpoints[0] + ", " + endpoints[1] + "]"
			config, err := loadEtcdConfig(writeEtcdTestFile(t, t.TempDir(), "etcd.yaml", content))
			require.NoError(t, err)
			require.Equal(t, endpoints, config.Endpoints)
		})
	}
}

func TestLoadEtcdConfigRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"empty", "", "at least one endpoint"},
		{"null", "null", "at least one endpoint"},
		{"malformed YAML", "endpoints: [", "invalid etcd config"},
		{"unknown field", "endpoints: [http://localhost:2379]\npasswordTypo: secret", "invalid etcd config"},
		{"duplicate field", "endpoints: [http://localhost:2379]\nendpoints: []", "invalid etcd config"},
		{"wrong endpoint type", "endpoints: localhost", "invalid etcd config"},
		{"wrong username type", "endpoints: [http://localhost:2379]\nusername: false", "invalid etcd config"},
		{
			"wrong password type",
			"endpoints: [http://localhost:2379]\nusername: gateway\npassword: 1234",
			"invalid etcd config",
		},
		{"empty endpoint", "endpoints: [\"\"]", "HTTP(S)"},
		{"missing scheme", "endpoints: [localhost:2379]", "HTTP(S)"},
		{
			"mixed schemes HTTP first",
			"endpoints: [http://etcd-1.example:2379, https://etcd-2.example:2379]",
			"same HTTP or HTTPS scheme",
		},
		{
			"mixed schemes HTTPS first",
			"endpoints: [https://etcd-1.example:2379, http://etcd-2.example:2379]",
			"same HTTP or HTTPS scheme",
		},
		{"credentials in URL", "endpoints: [http://user:secret@localhost:2379]", "without credentials"},
		{"endpoint path", "endpoints: [http://localhost:2379/workers]", "HTTP(S)"},
		{"invalid duration", "endpoints: [http://localhost:2379]\ndialTimeout: soon", "positive duration"},
		{"zero duration", "endpoints: [http://localhost:2379]\ndialTimeout: 0s", "positive duration"},
		{"negative duration", "endpoints: [http://localhost:2379]\ndialTimeout: -1s", "positive duration"},
		{"password without user", "endpoints: [http://localhost:2379]\npassword: secret", "requires username"},
		{"certificate without key", "endpoints: [https://localhost:2379]\ntlsCert: cert.pem", "provided together"},
		{"key without certificate", "endpoints: [https://localhost:2379]\ntlsKey: key.pem", "provided together"},
		{"TLS on HTTP", "endpoints: [http://localhost:2379]\ntlsCA: ca.pem", "HTTPS endpoints"},
		{"missing CA file", "endpoints: [https://localhost:2379]\ntlsCA: absent.pem", "read etcd tlsCA"},
		{
			"missing client files",
			"endpoints: [https://localhost:2379]\ntlsCert: absent.pem\ntlsKey: key.pem",
			"load etcd client certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadEtcdConfig(writeEtcdTestFile(t, t.TempDir(), "etcd.yaml", tc.content))
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	_, err := loadEtcdConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorContains(t, err, "read etcd config")
}

func TestLoadEtcdConfigTLS(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "etcd-config-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	writeEtcdTestFile(t, dir, "ca.pem", certPEM)
	writeEtcdTestFile(t, dir, "client.pem", certPEM)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	writeEtcdTestFile(t, dir, "client-key.pem", string(keyPEM))
	path := writeEtcdTestFile(t, dir, "etcd.yaml", `
endpoints: ["https://localhost:2379"]
tlsCA: ca.pem
tlsCert: client.pem
tlsKey: client-key.pem
`)
	config, err := loadEtcdConfig(path)
	require.NoError(t, err)
	require.NotNil(t, config.TLS)
	require.False(t, config.TLS.InsecureSkipVerify)
	require.Equal(t, uint16(tls.VersionTLS12), config.TLS.MinVersion)
	require.Len(t, config.TLS.Certificates, 1)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	_, err = parsed.Verify(x509.VerifyOptions{
		Roots: config.TLS.RootCAs, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	require.NoError(t, err)

	// An absolute CA path works, and an explicit CA alone does not require mTLS.
	absoluteCAConfig := "endpoints: [https://localhost:2379]\ntlsCA: " + filepath.Join(dir, "ca.pem") + "\n"
	path = writeEtcdTestFile(t, dir, "etcd.yaml", absoluteCAConfig)
	config, err = loadEtcdConfig(path)
	require.NoError(t, err)
	require.Empty(t, config.TLS.Certificates)

	writeEtcdTestFile(t, dir, "ca.pem", "not a certificate")
	_, err = loadEtcdConfig(path)
	require.ErrorContains(t, err, "no valid CA certificates")

	// Certificate verification stays enabled when using only a client key pair.
	path = writeEtcdTestFile(t, dir, "etcd.yaml", `
endpoints: ["https://localhost:2379"]
tlsCert: client.pem
tlsKey: client-key.pem
`)
	config, err = loadEtcdConfig(path)
	require.NoError(t, err)
	require.Nil(t, config.TLS.RootCAs)
	require.False(t, config.TLS.InsecureSkipVerify)
}

func writeEtcdTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}
