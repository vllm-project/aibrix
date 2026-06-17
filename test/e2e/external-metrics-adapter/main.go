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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	externalmetricsv1beta1 "k8s.io/metrics/pkg/apis/external_metrics/v1beta1"
)

const (
	group      = "external.metrics.k8s.io"
	version    = "v1beta1"
	listKind   = "ExternalMetricValueList"
	defaultKey = "source"
)

func main() {
	metricName := env("METRIC_NAME", "aibrix_test_queue_depth")
	metricValue := env("METRIC_VALUE", "100")
	labelKey := env("METRIC_LABEL_KEY", defaultKey)
	labelValue := env("METRIC_LABEL_VALUE", "aibrix-e2e")

	cert, err := selfSignedCert()
	if err != nil {
		log.Fatalf("failed to create self-signed cert: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", writeOK)
	mux.HandleFunc("/readyz", writeOK)
	apiHandler := func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSuffix(r.URL.Path, "/") {
		case "/apis":
			writeJSON(w, metav1.APIGroupList{
				Groups: []metav1.APIGroup{apiGroup()},
			})
		case "/apis/" + group:
			writeJSON(w, apiGroup())
		case "/apis/" + group + "/" + version:
			writeJSON(w, metav1.APIResourceList{
				TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
				GroupVersion: schema.GroupVersion{Group: group, Version: version}.String(),
				APIResources: []metav1.APIResource{
					{
						Name:       metricName,
						Namespaced: true,
						Kind:       "ExternalMetricValue",
						Verbs:      []string{"get", "list"},
					},
				},
			})
		default:
			serveMetric(w, r, metricName, metricValue, labelKey, labelValue)
		}
	}
	mux.HandleFunc("/apis", apiHandler)
	mux.HandleFunc("/apis/", apiHandler)

	server := &http.Server{
		Addr:      ":8443",
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	log.Printf("serving fake external metrics API for %q on :8443", metricName)
	log.Fatal(server.ListenAndServeTLS("", ""))
}

func serveMetric(w http.ResponseWriter, r *http.Request, metricName, metricValue, labelKey, labelValue string) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 6 ||
		parts[0] != "apis" ||
		parts[1] != group ||
		parts[2] != version ||
		parts[3] != "namespaces" ||
		parts[5] != metricName {
		writeStatus(w, http.StatusNotFound, "NotFound", "metric path not found")
		return
	}

	value, err := strconv.ParseFloat(metricValue, 64)
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, "InvalidMetricValue", err.Error())
		return
	}

	writeJSON(w, externalmetricsv1beta1.ExternalMetricValueList{
		TypeMeta: metav1.TypeMeta{
			Kind:       listKind,
			APIVersion: schema.GroupVersion{Group: group, Version: version}.String(),
		},
		Items: []externalmetricsv1beta1.ExternalMetricValue{
			{
				MetricName:   metricName,
				MetricLabels: map[string]string{labelKey: labelValue},
				Timestamp:    metav1.Now(),
				Value:        resource.MustParse(strconv.FormatFloat(value, 'f', -1, 64)),
			},
		},
	})
}

func apiGroup() metav1.APIGroup {
	gv := metav1.GroupVersionForDiscovery{
		GroupVersion: schema.GroupVersion{Group: group, Version: version}.String(),
		Version:      version,
	}
	return metav1.APIGroup{
		TypeMeta:         metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
		Name:             group,
		Versions:         []metav1.GroupVersionForDiscovery{gv},
		PreferredVersion: gv,
	}
}

func writeOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, obj any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		log.Printf("failed to write json response: %v", err)
	}
}

func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReason(reason),
		Message:  message,
		Code:     int32(code),
	})
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "aibrix-external-metrics-adapter.aibrix-system.svc",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames: []string{
			"aibrix-external-metrics-adapter",
			"aibrix-external-metrics-adapter.aibrix-system",
			"aibrix-external-metrics-adapter.aibrix-system.svc",
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}
