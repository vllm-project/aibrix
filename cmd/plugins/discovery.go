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
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"sigs.k8s.io/yaml"
)

// etcdFileConfig keeps durations and certificate paths convenient for YAML/JSON
// while the discovery provider accepts a transport-ready configuration.
type etcdFileConfig struct {
	Endpoints   []string `json:"endpoints"`
	Prefix      string   `json:"prefix"`
	DialTimeout string   `json:"dialTimeout"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	TLSCA       string   `json:"tlsCA"`
	TLSCert     string   `json:"tlsCert"`
	TLSKey      string   `json:"tlsKey"`
}

func validateDiscoveryFlags(standalone bool, endpointsPath, etcdPath string) error {
	if endpointsPath != "" && etcdPath != "" {
		return fmt.Errorf("--endpoints-config and --etcd-config are mutually exclusive")
	}
	if etcdPath != "" && !standalone {
		return fmt.Errorf("--etcd-config requires --standalone")
	}
	if standalone && endpointsPath == "" && etcdPath == "" {
		return fmt.Errorf("--endpoints-config or --etcd-config is required in standalone mode")
	}
	return nil
}

func loadEtcdConfig(path string) (discovery.EtcdConfig, error) {
	var config discovery.EtcdConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("read etcd config: %w", err)
	}
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return config, fmt.Errorf("invalid etcd config: expected YAML or JSON")
	}
	var file etcdFileConfig
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		// Parser errors may contain values from a config containing credentials.
		return config, fmt.Errorf("invalid etcd config: expected YAML or JSON with known fields and correct types")
	}
	if len(file.Endpoints) == 0 {
		return config, fmt.Errorf("etcd config requires at least one endpoint")
	}
	var endpointScheme string
	for _, endpoint := range file.Endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
			u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return config, fmt.Errorf("etcd endpoints must be HTTP(S) URLs without credentials, query, fragment or path")
		}
		// The etcd client selects one transport for all endpoints using the first URL.
		if endpointScheme != "" && u.Scheme != endpointScheme {
			return config, fmt.Errorf("etcd endpoints must use the same HTTP or HTTPS scheme")
		}
		endpointScheme = u.Scheme
	}
	config.Endpoints = file.Endpoints
	config.Prefix = file.Prefix
	if config.Prefix == "" {
		config.Prefix = "/aibrix/endpoints/"
	}
	config.DialTimeout = 5 * time.Second
	if file.DialTimeout != "" {
		timeout, err := time.ParseDuration(file.DialTimeout)
		if err != nil || timeout <= 0 {
			return config, fmt.Errorf("etcd dialTimeout must be a positive duration, such as 5s")
		}
		config.DialTimeout = timeout
	}
	config.Username = file.Username
	config.Password = file.Password
	if file.Password != "" && file.Username == "" {
		return config, fmt.Errorf("etcd password requires username")
	}
	config.TLS, err = loadEtcdTLS(file, path)
	return config, err
}

func loadEtcdTLS(file etcdFileConfig, path string) (*tls.Config, error) {
	if (file.TLSCert == "") != (file.TLSKey == "") {
		return nil, fmt.Errorf("etcd tlsCert and tlsKey must be provided together")
	}
	if file.TLSCA == "" && file.TLSCert == "" {
		return nil, nil
	}
	for _, endpoint := range file.Endpoints {
		if !strings.HasPrefix(endpoint, "https://") {
			return nil, fmt.Errorf("etcd TLS configuration requires HTTPS endpoints")
		}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	resolve := func(certPath string) string {
		if filepath.IsAbs(certPath) {
			return certPath
		}
		return filepath.Join(filepath.Dir(path), certPath)
	}
	if file.TLSCA != "" {
		pem, err := os.ReadFile(resolve(file.TLSCA))
		if err != nil {
			return nil, fmt.Errorf("read etcd tlsCA: %w", err)
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("etcd tlsCA contains no valid CA certificates")
		}
	}
	if file.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(resolve(file.TLSCert), resolve(file.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("load etcd client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	return tlsConfig, nil
}
