/*
Copyright 2024 The Aibrix Team.

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

package cert

import (
	"fmt"

	cert "github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// DefaultWebhookNamePrefix is the name prefix that the default install
// (config/default) and the Helm chart give the webhook objects.
const DefaultWebhookNamePrefix = "aibrix-"

// Base names of the webhook objects, before the install's name prefix. They
// match the names in config/webhook and config/internalcert.
const (
	serviceBaseName             = "webhook-service"
	secretBaseName              = "webhook-server-cert"
	validateWebhookConfBaseName = "validating-webhook-configuration"
	mutatingWebhookConfBaseName = "mutating-webhook-configuration"
)

const (
	certDir = "/tmp/k8s-webhook-server/serving-certs"
	caName  = "aibrix-ca"
	caOrg   = "aibrix"
)

// WebhookNames are the objects the cert rotator reads and updates.
type WebhookNames struct {
	Service                        string
	Secret                         string
	ValidatingWebhookConfiguration string
	MutatingWebhookConfiguration   string
}

// WebhookNamesForPrefix returns the webhook object names for an install whose
// kustomize namePrefix is prefix.
func WebhookNamesForPrefix(prefix string) WebhookNames {
	return WebhookNames{
		Service:                        prefix + serviceBaseName,
		Secret:                         prefix + secretBaseName,
		ValidatingWebhookConfiguration: prefix + validateWebhookConfBaseName,
		MutatingWebhookConfiguration:   prefix + mutatingWebhookConfBaseName,
	}
}

//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=mutatingwebhookconfigurations,verbs=get;list;watch;update
//+kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=validatingwebhookconfigurations,verbs=get;list;watch;update

// CertsManager creates certs for webhooks. namePrefix is the install's name
// prefix for the webhook Service, Secret and webhook configurations.
func CertsManager(mgr ctrl.Manager, namespace, namePrefix string, setupFinish chan struct{}) error {
	return cert.AddRotator(mgr, newCertRotator(namespace, namePrefix, setupFinish))
}

// newCertRotator configures the rotator for the webhook objects that an
// install with the given name prefix creates in namespace.
func newCertRotator(namespace, namePrefix string, setupFinish chan struct{}) *cert.CertRotator {
	names := WebhookNamesForPrefix(namePrefix)
	// dnsName is the format of <service name>.<namespace>.svc
	dnsName := fmt.Sprintf("%s.%s.svc", names.Service, namespace)
	return &cert.CertRotator{
		SecretKey: types.NamespacedName{
			Namespace: namespace,
			Name:      names.Secret,
		},
		CertDir:        certDir,
		CAName:         caName,
		CAOrganization: caOrg,
		DNSName:        dnsName,
		IsReady:        setupFinish,
		Webhooks: []cert.WebhookInfo{
			{
				Type: cert.Validating,
				Name: names.ValidatingWebhookConfiguration,
			},
			{
				Type: cert.Mutating,
				Name: names.MutatingWebhookConfiguration,
			},
		},
	}
}
