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

package deployers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const dynamoPlatformHelmReleaseName = "dynamo-platform"
const DynamoBenchmarkNamespace = "brixbench-dynamo"
const dynamoClearFinalizersPatch = `{"metadata":{"finalizers":[]}}`
const dynamoFrontendComponentLabel = "nvidia.com/dynamo-component"
const dynamoFrontendComponentName = "Frontend"
const dynamoReadinessTimeout = 10 * time.Minute
const dynamoReadinessPollInterval = 5 * time.Second
const dynamoHelmRepoRetryAttempts = 3
const dynamoHelmRepoRetryDelay = 2 * time.Second
const dynamoHelmStateDirName = "dynamo-helm"
const dynamoDefaultPlatformValuesFileName = "dynamo-platform-values.yaml"
const dynamoModelProbeTimeout = 10 * time.Minute

// DynamoDeployer deploys Dynamo platform releases and user-provided
// DynamoGraphDeployment manifests.
type DynamoDeployer struct {
	namespace         string
	logDir            string
	projectRoot       string
	version           string
	engineManifest    string
	platformValues    string
	graphName         string
	components        []string
	componentReplicas map[string]int
	effectiveNS       string
	modelName         string
	releaseSource     DynamoReleaseSource
	runner            commandRunner
	release           *DynamoRelease
}

var _ Deployer = (*DynamoDeployer)(nil)

// NewDynamoDeployer creates a release-based Dynamo deployer.
func NewDynamoDeployer() *DynamoDeployer {
	return &DynamoDeployer{
		releaseSource: NewGitDynamoReleaseSource(),
		runner:        execCommandRunner{},
	}
}

// CleanupStaleDynamoNamespace clears Dynamo finalizers before the generic
// namespace reset path waits on namespace deletion.
func CleanupStaleDynamoNamespace(ctx context.Context, namespace string, engineManifest string, projectRoot string, logDir string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	deployer := &DynamoDeployer{
		namespace:      namespace,
		effectiveNS:    namespace,
		engineManifest: strings.TrimSpace(engineManifest),
		projectRoot:    strings.TrimSpace(projectRoot),
		logDir:         strings.TrimSpace(logDir),
		runner:         execCommandRunner{},
	}
	exists, err := deployer.dynamoNamespaceExists(ctx, namespace)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return deployer.Teardown(ctx)
}

func (d *DynamoDeployer) Initialize(ctx context.Context, config Config) error {
	if d.releaseSource == nil {
		d.releaseSource = NewGitDynamoReleaseSource()
	}
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}

	d.namespace = DynamoBenchmarkNamespace
	d.logDir = strings.TrimSpace(config.LogDir)
	d.projectRoot = strings.TrimSpace(config.ProjectRoot)
	d.engineManifest = strings.TrimSpace(config.EnginePath)
	d.platformValues = strings.TrimSpace(config.PlatformValuesFile)
	if config.TestCase != nil {
		d.version = strings.TrimSpace(config.TestCase.Version)
		if d.engineManifest == "" {
			d.engineManifest = strings.TrimSpace(config.TestCase.Engine.Manifest)
		}
		if d.platformValues == "" {
			d.platformValues = strings.TrimSpace(config.TestCase.Platform.ValuesFile)
		}
	}

	if d.version == "" {
		return fmt.Errorf("Dynamo deployer requires version")
	}
	if d.namespace == "" {
		return fmt.Errorf("Dynamo deployer requires namespace")
	}
	if d.projectRoot == "" {
		return fmt.Errorf("Dynamo deployer requires project root")
	}
	if d.engineManifest == "" {
		return fmt.Errorf("Dynamo deployer requires engine manifest")
	}
	if d.platformValues != "" && !pathExists(d.platformValues) {
		return fmt.Errorf("Dynamo platform values file %s was not found", d.platformValues)
	}
	if err := d.loadDynamoGraphMetadata(); err != nil {
		return err
	}
	return nil
}

func (d *DynamoDeployer) DeployControlPlane(ctx context.Context) error {
	release, err := d.releaseSource.PrepareRelease(ctx, d.projectRoot, d.version)
	if err != nil {
		return fmt.Errorf("failed to prepare Dynamo release %s: %w", d.version, err)
	}
	if release == nil {
		return fmt.Errorf("failed to prepare Dynamo release %s: release is nil", d.version)
	}
	d.release = release

	if err := d.ensureDynamoHelmRepositories(ctx, release.ChartPath); err != nil {
		return err
	}
	if err := d.runDynamoHelmCommandWithRetry(ctx, "helm-dependency-build-dynamo-platform", dynamoHelmRepoRetryAttempts, "dependency", "build", "--skip-refresh", release.ChartPath); err != nil {
		return err
	}
	installArgs := []string{
		"upgrade", "--install", dynamoPlatformHelmReleaseName, release.ChartPath,
		"-n", d.namespace,
		"--create-namespace",
		"--set", "dynamo-operator.namespaceRestriction.enabled=true",
	}
	if d.platformValues == "" {
		platformValues, err := d.writeDynamoDefaultPlatformValues()
		if err != nil {
			return err
		}
		d.platformValues = platformValues
	}
	if d.platformValues != "" {
		installArgs = append(installArgs, "-f", d.platformValues)
	}
	installArgs = append(
		installArgs,
		"--no-hooks",
		"--wait",
		"--timeout", "10m",
	)
	return d.runDynamoHelmCommand(ctx, "helm-install-dynamo-platform", installArgs...)
}

func (d *DynamoDeployer) DeployGateway(ctx context.Context) error {
	return nil
}

func (d *DynamoDeployer) DeployEngine(ctx context.Context) error {
	if strings.TrimSpace(d.engineManifest) == "" {
		return fmt.Errorf("Dynamo deployer requires engine manifest")
	}
	if err := d.ensureDynamoGraphMetadata(); err != nil {
		return err
	}
	if err := d.runDynamoCommand(ctx, "apply-dynamo-graph-deployment", "kubectl", "apply", "-n", d.effectiveNS, "-f", d.engineManifest); err != nil {
		return err
	}
	return nil
}

func (d *DynamoDeployer) WaitForReady(ctx context.Context) error {
	if err := d.ensureDynamoGraphMetadata(); err != nil {
		return err
	}
	if _, _, err := d.waitForDynamoFrontendService(ctx, dynamoReadinessTimeout, dynamoReadinessPollInterval); err != nil {
		return fmt.Errorf("Dynamo Frontend service is not ready: %w", err)
	}
	if err := d.waitForDynamoComponentReady(ctx, dynamoFrontendComponentName); err != nil {
		return err
	}
	workers := d.dynamoWorkerComponents()
	if len(workers) == 0 {
		return fmt.Errorf("DynamoGraphDeployment %s has no non-Frontend components to wait for", d.graphName)
	}
	for _, component := range workers {
		if err := d.waitForDynamoComponentReady(ctx, component); err != nil {
			return err
		}
	}
	if err := d.waitForDynamoModelReady(ctx); err != nil {
		return err
	}
	return nil
}

func (d *DynamoDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	if err := d.ensureDynamoGraphMetadata(); err != nil {
		return "", err
	}
	serviceName, serviceIP, port, err := d.resolveDynamoFrontendService(ctx)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(serviceIP)
	if host == "" || strings.EqualFold(host, "None") {
		host = fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, d.effectiveNS)
	}
	return fmt.Sprintf("http://%s:%d", host, port), nil
}

func (d *DynamoDeployer) Teardown(ctx context.Context) error {
	namespace := d.effectiveNS
	if namespace == "" {
		namespace = d.namespace
	}
	var criticalErrs []error
	addCriticalErr := func(err error) {
		if err != nil {
			criticalErrs = append(criticalErrs, err)
		}
	}
	addCriticalErrUnlessNotFound := func(err error) {
		if err != nil && !isDynamoCleanupNotFoundError(err) {
			criticalErrs = append(criticalErrs, err)
		}
	}
	hasEngineManifest := strings.TrimSpace(d.engineManifest) != "" && pathExists(d.engineManifest)
	if hasEngineManifest {
		args := []string{"patch"}
		if strings.TrimSpace(namespace) != "" {
			args = append(args, "-n", namespace)
		}
		args = append(args, "-f", d.engineManifest, "--type=merge", "-p", dynamoClearFinalizersPatch)
		addCriticalErrUnlessNotFound(d.runDynamoCleanupCommand(ctx, "patch-dynamo-graph-deployment-finalizers", "kubectl", args...))
	}
	componentDeployments := []string{}
	if strings.TrimSpace(namespace) != "" {
		var err error
		componentDeployments, err = d.patchDynamoComponentDeploymentFinalizers(ctx, namespace)
		addCriticalErr(err)
	}
	if hasEngineManifest {
		args := []string{"delete"}
		if strings.TrimSpace(namespace) != "" {
			args = append(args, "-n", namespace)
		}
		args = append(args, "-f", d.engineManifest, "--ignore-not-found", "--wait=false")
		addCriticalErr(d.runDynamoCleanupCommand(ctx, "delete-dynamo-graph-deployment", "kubectl", args...))
		waitArgs := []string{"wait", "--for=delete"}
		if strings.TrimSpace(namespace) != "" {
			waitArgs = append(waitArgs, "-n", namespace)
		}
		waitArgs = append(waitArgs, "-f", d.engineManifest, "--timeout=2m")
		addCriticalErrUnlessNotFound(d.runDynamoCleanupCommand(ctx, "wait-delete-dynamo-graph-deployment", "kubectl", waitArgs...))
	}
	if strings.TrimSpace(namespace) != "" {
		addCriticalErr(d.deleteDynamoComponentDeployments(ctx, namespace, componentDeployments))
		addCriticalErr(d.runDynamoHelmCleanupCommand(ctx, "uninstall-dynamo-platform", "uninstall", dynamoPlatformHelmReleaseName, "-n", namespace, "--ignore-not-found", "--wait", "--timeout", "5m"))
		_ = d.runDynamoCleanupCommand(ctx, "delete-dynamo-pvcs", "kubectl", "delete", "pvc", "--all", "-n", namespace, "--ignore-not-found")
		addCriticalErr(d.runDynamoCleanupCommand(ctx, "delete-dynamo-namespace", "kubectl", "delete", "namespace", namespace, "--ignore-not-found"))
		addCriticalErr(d.runDynamoCleanupCommand(ctx, "wait-delete-dynamo-namespace", "kubectl", "wait", "--for=delete", "namespace/"+namespace, "--timeout=10m"))
	}
	return errors.Join(criticalErrs...)
}

func (d *DynamoDeployer) dynamoNamespaceExists(ctx context.Context, namespace string) (bool, error) {
	output, err := d.captureDynamoCommand(ctx, "inspect-stale-dynamo-namespace", "bash", "-lc", fmt.Sprintf("kubectl get namespace %s -o name 2>/dev/null || true", shellQuote(namespace)))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

func (d *DynamoDeployer) writeDynamoDefaultPlatformValues() (string, error) {
	return d.writeDynamoRuntimeManifest("default-dynamo-platform-values", renderDynamoDefaultPlatformValues(d.version))
}

func (d *DynamoDeployer) writeDynamoRuntimeManifest(name string, content string) (string, error) {
	dir := strings.TrimSpace(d.logDir)
	if dir == "" {
		dir = filepath.Join(d.projectRoot, ".tmp", "dynamo-runtime", d.namespace)
	} else {
		dir = filepath.Join(dir, "dynamo-runtime")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create Dynamo runtime manifest directory %s: %w", dir, err)
	}
	filename := sanitizeDynamoCommandStage(name) + ".yaml"
	if name == "default-dynamo-platform-values" {
		filename = dynamoDefaultPlatformValuesFileName
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write Dynamo runtime manifest %s: %w", path, err)
	}
	return path, nil
}

func renderDynamoDefaultPlatformValues(operatorTag string) string {
	operatorTag = strings.TrimPrefix(strings.TrimSpace(operatorTag), "v")
	return fmt.Sprintf(`dynamo-operator:
  enabled: true
  istioVirtualServiceEnabled: true
  upgradeCRD: true
  controllerManager:
    manager:
      image:
        tag: %q
        pullPolicy: IfNotPresent
  webhook:
    failurePolicy: Ignore
    certificateSecret:
      name: webhook-server-cert
      external: false
    certManager:
      enabled: false
  discoveryBackend: kubernetes
global:
  etcd:
    install: false
  nats:
    install: true
nats:
  config:
    jetstream:
      enabled: true
      fileStore:
        enabled: true
        dir: /data
        pvc:
          enabled: false
        size: 8Gi
      memoryStore:
        enabled: true
        maxSize: 8Gi
    merge:
      max_payload: 15728640
    monitor:
      enabled: true
      port: 8222
  natsBox:
    enabled: false
`, operatorTag)
}

func (d *DynamoDeployer) patchDynamoComponentDeploymentFinalizers(ctx context.Context, namespace string) ([]string, error) {
	output, err := d.captureDynamoCommand(ctx, "list-dynamo-component-deployments", "kubectl", "get", "dynamocomponentdeployments.nvidia.com", "-n", namespace, "-o", "name")
	if err != nil {
		fmt.Printf("Warning: Dynamo cleanup step list-dynamo-component-deployments failed: %v\n", err)
		if isDynamoCleanupMissingResourceTypeError(err) {
			return nil, nil
		}
		return nil, err
	}
	names := []string{}
	var errs []error
	for _, name := range strings.Fields(output) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		names = append(names, name)
		if err := d.runDynamoCleanupCommand(ctx, "patch-dynamo-component-deployment-finalizers-"+name, "kubectl", "patch", name, "-n", namespace, "--type=merge", "-p", dynamoClearFinalizersPatch); err != nil && !isDynamoCleanupNotFoundError(err) {
			errs = append(errs, err)
		}
	}
	return names, errors.Join(errs...)
}

func (d *DynamoDeployer) deleteDynamoComponentDeployments(ctx context.Context, namespace string, names []string) error {
	var errs []error
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := d.runDynamoCleanupCommand(ctx, "delete-dynamo-component-deployment-"+name, "kubectl", "delete", name, "-n", namespace, "--ignore-not-found", "--wait=false"); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if err := d.runDynamoCleanupCommand(ctx, "wait-delete-dynamo-component-deployment-"+name, "kubectl", "wait", "--for=delete", name, "-n", namespace, "--timeout=2m"); err != nil && !isDynamoCleanupNotFoundError(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isDynamoCleanupNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "Error from server (NotFound)") || strings.Contains(message, " not found")
}

func isDynamoCleanupMissingResourceTypeError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "the server doesn't have a resource type") ||
		strings.Contains(message, "the server could not find the requested resource") ||
		strings.Contains(message, "no matches for kind")
}

type dynamoGraphManifestMetadata struct {
	Name              string
	Namespace         string
	Components        []string
	ComponentReplicas map[string]int
	RawContent        string
}

func (d *DynamoDeployer) ensureDynamoGraphMetadata() error {
	if d.graphName != "" && d.effectiveNS != "" {
		return nil
	}
	return d.loadDynamoGraphMetadata()
}

func (d *DynamoDeployer) loadDynamoGraphMetadata() error {
	metadata, err := readDynamoGraphManifestMetadata(d.engineManifest)
	if err != nil {
		return err
	}
	if metadata.Name == "" {
		return fmt.Errorf("DynamoGraphDeployment metadata.name is required in %s", d.engineManifest)
	}
	if d.namespace == "" {
		return fmt.Errorf("Dynamo deployer requires namespace")
	}

	effectiveNS := metadata.Namespace
	if effectiveNS == "" {
		effectiveNS = d.namespace
	} else if effectiveNS != d.namespace {
		return fmt.Errorf("DynamoGraphDeployment namespace %q must match benchmark namespace %q", effectiveNS, d.namespace)
	}

	d.graphName = metadata.Name
	d.components = metadata.Components
	d.componentReplicas = metadata.ComponentReplicas
	d.effectiveNS = effectiveNS
	d.modelName = inferDynamoModelNameFromManifest(metadata.RawContent)
	return nil
}

func readDynamoGraphManifestMetadata(path string) (*dynamoGraphManifestMetadata, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read Dynamo engine manifest %s: %w", path, err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec struct {
				Services map[string]struct {
					Replicas int `yaml:"replicas"`
				} `yaml:"services"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("failed to parse Dynamo engine manifest %s: %w", path, err)
		}
		if doc.Kind != "DynamoGraphDeployment" {
			continue
		}
		components := make([]string, 0, len(doc.Spec.Services))
		componentReplicas := make(map[string]int, len(doc.Spec.Services))
		for name, service := range doc.Spec.Services {
			name = strings.TrimSpace(name)
			if name != "" {
				components = append(components, name)
				replicas := service.Replicas
				if replicas < 1 {
					replicas = 1
				}
				componentReplicas[name] = replicas
			}
		}
		sort.Strings(components)
		return &dynamoGraphManifestMetadata{
			Name:              strings.TrimSpace(doc.Metadata.Name),
			Namespace:         strings.TrimSpace(doc.Metadata.Namespace),
			Components:        components,
			ComponentReplicas: componentReplicas,
			RawContent:        string(content),
		}, nil
	}
	return nil, fmt.Errorf("Dynamo engine manifest %s must contain a DynamoGraphDeployment", path)
}

func inferDynamoModelNameFromManifest(content string) string {
	for _, pattern := range []string{
		`export\s+SERVED_MODEL_NAME=["']?([A-Za-z0-9._-]+)["']?`,
		`--served-model-name\s+([A-Za-z0-9._-]+)`,
		`export\s+MODEL=["']?([A-Za-z0-9._-]+)["']?`,
	} {
		if value := firstRegexGroup(content, pattern); value != "" && !strings.HasPrefix(value, "$") {
			return value
		}
	}
	return ""
}

func (d *DynamoDeployer) waitForDynamoModelReady(ctx context.Context) error {
	modelName := strings.TrimSpace(d.modelName)
	if modelName == "" {
		return nil
	}
	frontendPod, err := d.resolveDynamoFrontendPodName(ctx)
	if err != nil {
		return err
	}
	if err := d.waitForDynamoModelRegistration(ctx, frontendPod, modelName); err != nil {
		return err
	}
	return nil
}

func (d *DynamoDeployer) resolveDynamoFrontendPodName(ctx context.Context) (string, error) {
	output, err := d.captureDynamoCommand(ctx, "get-dynamo-frontend-pod-name", "kubectl", "get", "pod", "-n", d.effectiveNS, "-l", dynamoFrontendComponentLabel+"="+dynamoFrontendComponentName, "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	podName := strings.TrimSpace(strings.Trim(output, "'"))
	if podName == "" {
		return "", fmt.Errorf("Dynamo Frontend pod was not found in namespace %s", d.effectiveNS)
	}
	return podName, nil
}

func (d *DynamoDeployer) waitForDynamoModelRegistration(ctx context.Context, frontendPod string, modelName string) error {
	deadline := time.Now().Add(dynamoModelProbeTimeout)
	var lastResponse string
	for {
		response, err := d.captureDynamoCommand(ctx, "probe-dynamo-models", "kubectl", "exec", frontendPod, "-n", d.effectiveNS, "--", "curl", "-sS", "http://localhost:8000/v1/models", "--max-time", "30")
		if err == nil && strings.Contains(response, modelName) {
			return nil
		}
		if err != nil {
			lastResponse = err.Error()
		} else {
			lastResponse = strings.TrimSpace(response)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Dynamo model %s was not registered within %s; last response: %s", modelName, dynamoModelProbeTimeout, truncateDynamoProbeResponse(lastResponse))
		}
		if err := sleepOrContextDone(ctx, 10*time.Second); err != nil {
			return fmt.Errorf("Dynamo model %s registration probe interrupted: %w", modelName, err)
		}
	}
}

func truncateDynamoProbeResponse(response string) string {
	response = strings.TrimSpace(response)
	if len(response) <= 200 {
		return response
	}
	return response[:200]
}

func (d *DynamoDeployer) resolveDynamoFrontendService(ctx context.Context) (string, string, int, error) {
	output, err := d.captureDynamoCommand(ctx, "get-dynamo-frontend-service-by-label", "kubectl", "get", "svc", "-n", d.effectiveNS, "-l", dynamoFrontendComponentLabel+"="+dynamoFrontendComponentName, "-o", "json")
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to list Dynamo Frontend services in %s: %w", d.effectiveNS, err)
	}

	services, err := parseDynamoServiceList(output)
	if err != nil {
		return "", "", 0, err
	}
	if len(services) > 1 {
		names := make([]string, 0, len(services))
		for _, service := range services {
			names = append(names, service.Name)
		}
		sort.Strings(names)
		return "", "", 0, fmt.Errorf("multiple Dynamo Frontend services found in %s: %s", d.effectiveNS, strings.Join(names, ", "))
	}
	if len(services) == 1 {
		port, err := selectDynamoFrontendServicePort(services[0])
		if err != nil {
			return "", "", 0, err
		}
		return services[0].Name, services[0].ClusterIP, port, nil
	}

	fallbackName := d.graphName + "-frontend"
	output, err = d.captureDynamoCommand(ctx, "get-dynamo-frontend-service-by-name", "kubectl", "get", "svc", fallbackName, "-n", d.effectiveNS, "-o", "json")
	if err != nil {
		return "", "", 0, fmt.Errorf("no Dynamo Frontend service found in %s by label or fallback name %s: %w", d.effectiveNS, fallbackName, err)
	}
	service, err := parseDynamoService(output)
	if err != nil {
		return "", "", 0, err
	}
	port, err := selectDynamoFrontendServicePort(service)
	if err != nil {
		return "", "", 0, err
	}
	return service.Name, service.ClusterIP, port, nil
}

type dynamoService struct {
	Name      string
	ClusterIP string
	Ports     []int
}

func parseDynamoServiceList(output string) ([]dynamoService, error) {
	var serviceList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ClusterIP string `json:"clusterIP"`
				Ports     []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(output), &serviceList); err != nil {
		return nil, fmt.Errorf("failed to parse Dynamo Frontend service list: %w", err)
	}
	services := make([]dynamoService, 0, len(serviceList.Items))
	for _, item := range serviceList.Items {
		if strings.TrimSpace(item.Metadata.Name) == "" {
			continue
		}
		ports := make([]int, 0, len(item.Spec.Ports))
		for _, port := range item.Spec.Ports {
			ports = append(ports, port.Port)
		}
		services = append(services, dynamoService{Name: item.Metadata.Name, ClusterIP: item.Spec.ClusterIP, Ports: ports})
	}
	return services, nil
}

func parseDynamoService(output string) (dynamoService, error) {
	var service struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			ClusterIP string `json:"clusterIP"`
			Ports     []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(output), &service); err != nil {
		return dynamoService{}, fmt.Errorf("failed to parse Dynamo Frontend service: %w", err)
	}
	if strings.TrimSpace(service.Metadata.Name) == "" {
		return dynamoService{}, fmt.Errorf("Dynamo Frontend service has no metadata.name")
	}
	ports := make([]int, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		ports = append(ports, port.Port)
	}
	return dynamoService{Name: service.Metadata.Name, ClusterIP: service.Spec.ClusterIP, Ports: ports}, nil
}

func selectDynamoFrontendServicePort(service dynamoService) (int, error) {
	if len(service.Ports) == 0 {
		return 0, fmt.Errorf("Dynamo Frontend service %s has no ports", service.Name)
	}
	for _, port := range service.Ports {
		if port == 8000 {
			return port, nil
		}
	}
	if len(service.Ports) == 1 {
		return service.Ports[0], nil
	}
	return 0, fmt.Errorf("Dynamo Frontend service %s has multiple ports and none is 8000", service.Name)
}

func (d *DynamoDeployer) waitForDynamoFrontendService(ctx context.Context, timeout time.Duration, interval time.Duration) (string, int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		serviceName, _, port, err := d.resolveDynamoFrontendService(ctx)
		if err == nil {
			return serviceName, port, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", 0, fmt.Errorf("timed out waiting for Dynamo Frontend service in %s: %w", d.effectiveNS, lastErr)
		}
		if err := sleepOrContextDone(ctx, interval); err != nil {
			return "", 0, fmt.Errorf("timed out waiting for Dynamo Frontend service in %s: %w", d.effectiveNS, err)
		}
	}
}

func (d *DynamoDeployer) waitForDynamoComponentReady(ctx context.Context, component string) error {
	expectedReplicas := d.expectedDynamoComponentReplicas(component)
	if err := d.waitForDynamoComponentPods(ctx, component, expectedReplicas, dynamoReadinessTimeout, dynamoReadinessPollInterval); err != nil {
		return err
	}
	return d.runDynamoCommand(ctx, "wait-dynamo-"+component+"-pods-ready", "kubectl", "wait", "--for=condition=Ready", "pod", "-n", d.effectiveNS, "-l", dynamoFrontendComponentLabel+"="+component, "--timeout=10m")
}

func (d *DynamoDeployer) expectedDynamoComponentReplicas(component string) int {
	if d.componentReplicas != nil {
		if replicas := d.componentReplicas[component]; replicas > 0 {
			return replicas
		}
	}
	return 1
}

func (d *DynamoDeployer) waitForDynamoComponentPods(ctx context.Context, component string, expectedReplicas int, timeout time.Duration, interval time.Duration) error {
	if expectedReplicas < 1 {
		expectedReplicas = 1
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		output, err := d.captureDynamoCommand(ctx, "get-dynamo-"+component+"-pods", "kubectl", "get", "pod", "-n", d.effectiveNS, "-l", dynamoFrontendComponentLabel+"="+component, "-o", "json")
		if err == nil {
			podCount, parseErr := parseDynamoPodListItemCount(output)
			if parseErr != nil {
				return parseErr
			}
			if podCount >= expectedReplicas {
				return nil
			}
			lastErr = fmt.Errorf("found %d pod(s), need %d", podCount, expectedReplicas)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Dynamo component %s pods in %s: %w", component, d.effectiveNS, lastErr)
		}
		if err := sleepOrContextDone(ctx, interval); err != nil {
			return fmt.Errorf("timed out waiting for Dynamo component %s pods in %s: %w", component, d.effectiveNS, err)
		}
	}
}

func parseDynamoPodListItemCount(output string) (int, error) {
	var podList struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(output), &podList); err != nil {
		return 0, fmt.Errorf("failed to parse Dynamo pod list: %w", err)
	}
	return len(podList.Items), nil
}

func sleepOrContextDone(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *DynamoDeployer) dynamoWorkerComponents() []string {
	var workers []string
	for _, component := range d.components {
		if component == "" || component == dynamoFrontendComponentName {
			continue
		}
		workers = append(workers, component)
	}
	return workers
}

type dynamoHelmDependency struct {
	Name       string `yaml:"name"`
	Repository string `yaml:"repository"`
}

type dynamoHelmLock struct {
	Dependencies []dynamoHelmDependency `yaml:"dependencies"`
}

type dynamoHelmRepo struct {
	Alias string
	URL   string
}

type dynamoHelmRepositoryConfig struct {
	Repositories []struct {
		Name string `yaml:"name"`
		URL  string `yaml:"url"`
	} `yaml:"repositories"`
}

type dynamoHelmState struct {
	configDir          string
	cacheDir           string
	dataDir            string
	repositoryCacheDir string
}

func (d *DynamoDeployer) ensureDynamoHelmRepositories(ctx context.Context, chartPath string) error {
	repos, err := readDynamoHelmDependencyRepos(chartPath)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return nil
	}

	for _, repo := range repos {
		exists, err := d.dynamoHelmRepositoryExists(repo)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := d.runDynamoHelmCommandWithRetry(
			ctx,
			"helm-repo-add-"+repo.Alias,
			dynamoHelmRepoRetryAttempts,
			"repo",
			"add",
			repo.Alias,
			repo.URL,
			"--force-update",
		); err != nil {
			return err
		}
	}
	return nil
}

func (d *DynamoDeployer) dynamoHelmRepositoryExists(repo dynamoHelmRepo) (bool, error) {
	configPath, cachePath, err := d.dynamoHelmRepositoryPaths(repo)
	if err != nil {
		return false, err
	}
	if !pathExists(cachePath) {
		return false, nil
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read Dynamo Helm repository config %s: %w", configPath, err)
	}

	var config dynamoHelmRepositoryConfig
	if err := yaml.Unmarshal(content, &config); err != nil {
		return false, fmt.Errorf("failed to parse Dynamo Helm repository config %s: %w", configPath, err)
	}
	for _, configuredRepo := range config.Repositories {
		if configuredRepo.Name == repo.Alias && configuredRepo.URL == repo.URL {
			return true, nil
		}
	}
	return false, nil
}

func (d *DynamoDeployer) dynamoHelmRepositoryPaths(repo dynamoHelmRepo) (string, string, error) {
	state, err := d.dynamoHelmState()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(state.configDir, "repositories.yaml"), filepath.Join(state.repositoryCacheDir, repo.Alias+"-index.yaml"), nil
}

func readDynamoHelmDependencyRepos(chartPath string) ([]dynamoHelmRepo, error) {
	lockPath := filepath.Join(chartPath, "Chart.lock")
	content, err := os.ReadFile(lockPath)
	manifestPath := lockPath
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to read Dynamo chart lock %s: %w", lockPath, err)
		}
		manifestPath = filepath.Join(chartPath, "Chart.yaml")
		content, err = os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read Dynamo chart manifest %s: %w", manifestPath, err)
		}
	}

	var chart dynamoHelmLock
	if err := yaml.Unmarshal(content, &chart); err != nil {
		return nil, fmt.Errorf("failed to parse Dynamo chart dependencies %s: %w", manifestPath, err)
	}

	repoByURL := make(map[string]string)
	for _, dependency := range chart.Dependencies {
		repoURL := strings.TrimSpace(dependency.Repository)
		if repoURL == "" || strings.HasPrefix(repoURL, "file://") || strings.HasPrefix(repoURL, "oci://") {
			continue
		}
		repoByURL[repoURL] = "dynamo-" + sanitizeDynamoCommandStage(repoURL)
	}

	repoURLs := make([]string, 0, len(repoByURL))
	for repoURL := range repoByURL {
		repoURLs = append(repoURLs, repoURL)
	}
	sort.Strings(repoURLs)

	repos := make([]dynamoHelmRepo, 0, len(repoURLs))
	for _, repoURL := range repoURLs {
		repos = append(repos, dynamoHelmRepo{
			Alias: repoByURL[repoURL],
			URL:   repoURL,
		})
	}
	return repos, nil
}

func (d *DynamoDeployer) runDynamoHelmCommandWithRetry(ctx context.Context, stage string, attempts int, args ...string) error {
	if attempts <= 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return d.runDynamoHelmCommand(ctx, stage, args...)
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = d.runDynamoHelmCommand(ctx, stage, args...)
		if lastErr == nil {
			return nil
		}
		if !isRetryableDynamoHelmError(lastErr) || attempt == attempts {
			return lastErr
		}
		if err := sleepOrContextDone(ctx, dynamoHelmRepoRetryDelay); err != nil {
			return fmt.Errorf("%s retry interrupted: %w", stage, err)
		}
	}
	return lastErr
}

func isRetryableDynamoHelmError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	retryableMarkers := []string{
		"eof",
		"tls handshake timeout",
		"i/o timeout",
		"context deadline exceeded",
		"client.timeout",
		"context cancellation while reading body",
		"connection reset by peer",
		"server sent goaway",
		"temporarily unavailable",
	}
	for _, marker := range retryableMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (d *DynamoDeployer) runDynamoCommand(ctx context.Context, stage string, name string, args ...string) error {
	startedAt := time.Now()
	output, err := d.runner.Run(ctx, name, args...)
	finishedAt := time.Now()

	if logErr := d.writeDynamoCommandLog(stage, name, args, startedAt, finishedAt, commandExitCode(err), output); logErr != nil && err == nil {
		return logErr
	}
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return fmt.Errorf("%s failed: %w: %s", stage, err, output)
		}
		return fmt.Errorf("%s failed: %w", stage, err)
	}
	return nil
}

func (d *DynamoDeployer) runDynamoHelmCommand(ctx context.Context, stage string, args ...string) error {
	name, commandArgs, err := d.dynamoHelmCommand(args...)
	if err != nil {
		return err
	}
	return d.runDynamoCommand(ctx, stage, name, commandArgs...)
}

func (d *DynamoDeployer) dynamoHelmCommand(args ...string) (string, []string, error) {
	envArgs, err := d.dynamoHelmEnvArgs()
	if err != nil {
		return "", nil, err
	}
	commandArgs := append(envArgs, "helm")
	commandArgs = append(commandArgs, args...)
	return "env", commandArgs, nil
}

func (d *DynamoDeployer) dynamoHelmEnvArgs() ([]string, error) {
	state, err := d.prepareDynamoHelmState()
	if err != nil {
		return nil, err
	}

	return []string{
		"HELM_CONFIG_HOME=" + state.configDir,
		"HELM_CACHE_HOME=" + state.cacheDir,
		"HELM_DATA_HOME=" + state.dataDir,
		"HELM_REPOSITORY_CONFIG=" + filepath.Join(state.configDir, "repositories.yaml"),
		"HELM_REPOSITORY_CACHE=" + state.repositoryCacheDir,
	}, nil
}

func (d *DynamoDeployer) prepareDynamoHelmState() (*dynamoHelmState, error) {
	state, err := d.dynamoHelmState()
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{state.configDir, state.cacheDir, state.dataDir, state.repositoryCacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create Dynamo Helm state directory %s: %w", dir, err)
		}
	}
	return state, nil
}

func (d *DynamoDeployer) dynamoHelmState() (*dynamoHelmState, error) {
	projectRoot := strings.TrimSpace(d.projectRoot)
	if projectRoot == "" {
		return nil, fmt.Errorf("Dynamo deployer requires project root")
	}

	stateDir := filepath.Join(projectRoot, ".tmp", dynamoHelmStateDirName)
	configDir := filepath.Join(stateDir, "config")
	cacheDir := filepath.Join(stateDir, "cache")
	dataDir := filepath.Join(stateDir, "data")
	repositoryCacheDir := filepath.Join(cacheDir, "repository")
	return &dynamoHelmState{
		configDir:          configDir,
		cacheDir:           cacheDir,
		dataDir:            dataDir,
		repositoryCacheDir: repositoryCacheDir,
	}, nil
}

func (d *DynamoDeployer) captureDynamoCommand(ctx context.Context, stage string, name string, args ...string) (string, error) {
	startedAt := time.Now()
	output, err := d.runner.Run(ctx, name, args...)
	finishedAt := time.Now()

	if logErr := d.writeDynamoCommandLog(stage, name, args, startedAt, finishedAt, commandExitCode(err), output); logErr != nil && err == nil {
		return "", logErr
	}
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return output, fmt.Errorf("%s failed: %w: %s", stage, err, output)
		}
		return output, fmt.Errorf("%s failed: %w", stage, err)
	}
	return output, nil
}

func (d *DynamoDeployer) runDynamoCleanupCommand(ctx context.Context, stage string, name string, args ...string) error {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}
	if strings.TrimSpace(name) == "" {
		return nil
	}
	if err := d.runDynamoCommand(ctx, stage, name, args...); err != nil {
		fmt.Printf("Warning: Dynamo cleanup step %s failed: %v\n", stage, err)
		return err
	}
	return nil
}

func (d *DynamoDeployer) runDynamoHelmCleanupCommand(ctx context.Context, stage string, args ...string) error {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}
	if err := d.runDynamoHelmCommand(ctx, stage, args...); err != nil {
		fmt.Printf("Warning: Dynamo cleanup step %s failed: %v\n", stage, err)
		return err
	}
	return nil
}

func (d *DynamoDeployer) writeDynamoCommandLog(stage string, name string, args []string, startedAt time.Time, finishedAt time.Time, exitCode int, output string) error {
	if strings.TrimSpace(d.logDir) == "" {
		return nil
	}
	logDir := filepath.Join(d.logDir, "commands")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("failed to create Dynamo command log directory: %w", err)
	}

	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", sanitizeDynamoCommandStage(stage)))
	content := strings.Builder{}
	content.WriteString("command: ")
	content.WriteString(formatExecCommand(name, args...))
	content.WriteString("\n")
	content.WriteString("startedAt: ")
	content.WriteString(startedAt.Format(time.RFC3339Nano))
	content.WriteString("\n")
	content.WriteString("finishedAt: ")
	content.WriteString(finishedAt.Format(time.RFC3339Nano))
	content.WriteString("\n")
	content.WriteString(fmt.Sprintf("exitCode: %d\n", exitCode))
	content.WriteString("\noutput:\n")
	content.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		content.WriteString("\n")
	}
	return os.WriteFile(logPath, []byte(content.String()), 0o644)
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitizeDynamoCommandStage(stage string) string {
	stage = strings.ToLower(strings.TrimSpace(stage))
	if stage == "" {
		return "command"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range stage {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
