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

package provider

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vllm-project/aibrix/apps/console/api/config"
	deploymentstatus "github.com/vllm-project/aibrix/apps/console/api/deployment/status"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// KubernetesProviderKind is the canonical provider kind for Kubernetes deployments.
	KubernetesProviderKind = "kubernetes"
	// LegacyKubernetesProviderKind is accepted for existing records and templates
	// created before the provider kind was standardized.
	LegacyKubernetesProviderKind         = "k8s-deployment"
	defaultKubernetesNamespace           = "default"
	defaultContainerPort           int32 = 8000
	defaultServicePort             int32 = 8000
	defaultCPURequest                    = "1"
	defaultHPATargetCPUUtilization int32 = 80
	defaultReplicaCount            int32 = 1
	defaultReadyTimeoutSeconds     int32 = 600
	kubernetesCleanupTimeout             = 30 * time.Second
)

// KubernetesClientProvider resolves a Kubernetes client and its workload
// namespace. Alternate environments can implement this interface without
// changing deployment lifecycle or resource rendering.
type KubernetesClientProvider interface {
	Client() (kubernetes.Interface, string, error)
}

type kubeconfigClientProvider struct {
	cfg             config.KubernetesProviderConfig
	mu              sync.Mutex
	cachedClientset kubernetes.Interface
	cachedNamespace string
}

type kubernetesDeploymentProvider struct {
	clients  KubernetesClientProvider
	workload config.KubernetesWorkloadConfig
}

var (
	_ DeploymentProvider       = (*kubernetesDeploymentProvider)(nil)
	_ KubernetesClientProvider = (*kubeconfigClientProvider)(nil)
)

func NewKubernetesClientProvider(cfg config.KubernetesProviderConfig) KubernetesClientProvider {
	return &kubeconfigClientProvider{cfg: cfg}
}

func NewKubernetesDeploymentProvider(
	clients KubernetesClientProvider,
	workload config.KubernetesWorkloadConfig,
) DeploymentProvider {
	return &kubernetesDeploymentProvider{
		clients:  clients,
		workload: workload,
	}
}

func (d *kubernetesDeploymentProvider) Kind() string {
	return KubernetesProviderKind
}

func (d *kubernetesDeploymentProvider) Aliases() []string {
	return []string{LegacyKubernetesProviderKind}
}

func (d *kubernetesDeploymentProvider) Validate(_ context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "create deployment request is required")
	}
	if template == nil {
		return status.Error(codes.InvalidArgument, "deployment template is required")
	}
	if template.GetSpec() == nil {
		return status.Error(codes.InvalidArgument, "deployment template spec is required")
	}
	if req.GetName() == "" {
		return status.Error(codes.InvalidArgument, "deployment name is required")
	}

	spec := template.GetSpec()
	engine := spec.GetEngine()
	if engine.GetImage() == "" {
		return status.Error(codes.InvalidArgument, "template engine.image is required for kubernetes")
	}
	switch strings.ToLower(engine.GetType()) {
	case "vllm", "sglang", "mock":
	default:
		return status.Errorf(codes.FailedPrecondition, "kubernetes implementation does not support engine %q", engine.GetType())
	}
	if spec.GetModelSource().GetUri() == "" {
		return status.Error(codes.InvalidArgument, "template model_source.uri is required for kubernetes")
	}
	if accelerator := spec.GetAccelerator(); accelerator == nil || accelerator.GetType() == "" || accelerator.GetCount() < 1 {
		return status.Error(codes.InvalidArgument, "template accelerator type and positive count are required")
	}
	if mode := spec.GetDeploymentMode(); mode != "" && mode != "dedicated" {
		return status.Errorf(codes.FailedPrecondition, "kubernetes implementation does not support deployment mode %q", mode)
	}
	if err := validateDeploymentSizing(spec, req); err != nil {
		return err
	}
	if err := d.validateConfig(); err != nil {
		return err
	}

	return nil
}

func (d *kubernetesDeploymentProvider) validateConfig() error {
	cfg := d.workload
	switch cfg.ServiceType {
	case "", string(corev1.ServiceTypeClusterIP), string(corev1.ServiceTypeNodePort), string(corev1.ServiceTypeLoadBalancer):
	default:
		return status.Errorf(codes.InvalidArgument, "unsupported kubernetes service type %q", cfg.ServiceType)
	}
	if cfg.ContainerPort < 0 || cfg.ContainerPort > 65535 {
		return status.Error(codes.InvalidArgument, "kubernetes container port must be between 1 and 65535")
	}
	if cfg.ServicePort < 0 || cfg.ServicePort > 65535 {
		return status.Error(codes.InvalidArgument, "kubernetes service port must be between 1 and 65535")
	}
	if cfg.CPURequest != "" {
		cpuRequest, err := resource.ParseQuantity(cfg.CPURequest)
		if err != nil || cpuRequest.Sign() <= 0 {
			return status.Errorf(codes.InvalidArgument, "invalid kubernetes CPU request %q", cfg.CPURequest)
		}
	}
	if cfg.HPATargetCPUUtilization < 0 || cfg.HPATargetCPUUtilization > 100 {
		return status.Error(codes.InvalidArgument, "kubernetes HPA target CPU utilization must be between 1 and 100")
	}
	return nil
}

func (d *kubernetesDeploymentProvider) Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	spec := proto.Clone(template.GetSpec()).(*pb.ModelDeploymentTemplateSpec)
	accelerator := spec.GetAccelerator()
	overrides := req.GetOverrides()
	if overrides != nil && len(overrides.GetEngineArgs()) > 0 {
		if spec.EngineArgs == nil {
			spec.EngineArgs = map[string]string{}
		}
		for key, value := range overrides.GetEngineArgs() {
			spec.EngineArgs[key] = value
		}
	}
	if overrides != nil && overrides.GetEnableMultiLora() {
		if spec.EngineArgs == nil {
			spec.EngineArgs = map[string]string{}
		}
		spec.EngineArgs["enable_lora"] = "true"
	}

	minReplicas := defaultReplicaCount
	maxReplicas := defaultReplicaCount
	enableAutoScaling := false
	if overrides != nil {
		if overrides.GetMinReplicas() > 0 {
			minReplicas = overrides.GetMinReplicas()
		}
		if overrides.GetMaxReplicas() > 0 {
			maxReplicas = overrides.GetMaxReplicas()
		}
		enableAutoScaling = overrides.GetEnableAutoScaling()
	}
	if maxReplicas < minReplicas {
		maxReplicas = minReplicas
	}

	replicas := fmt.Sprintf("%d", minReplicas)
	if enableAutoScaling && maxReplicas > minReplicas {
		replicas = fmt.Sprintf("%d[%d]", minReplicas, maxReplicas)
	}

	region := req.GetRegion()
	if overrides != nil && overrides.GetRegion() != "" {
		region = overrides.GetRegion()
	}

	clientset, namespace, err := d.clients.Client()
	if err != nil {
		return nil, err
	}

	resourceName := generateResourceName(req.GetName())
	labels := map[string]string{
		"app.kubernetes.io/name":     "aibrix-console-deployment",
		"app.kubernetes.io/instance": resourceName,
		"aibrix.io/template-id":      template.GetId(),
		"aibrix.io/provider":         d.Kind(),
		"aibrix.io/model-id":         template.GetModelId(),
	}

	replicasValue := minReplicas
	deployment := buildDeployment(resourceName, namespace, labels, d.workload, spec, replicasValue)
	if _, err := clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "kubernetes namespace %q does not exist", namespace)
		}
		return nil, status.Errorf(codes.Internal, "create deployment %q: %v", resourceName, err)
	}
	deploymentCreated := true

	serviceName := resourceName + "-svc"
	service := buildService(serviceName, namespace, labels, d.workload)
	if _, err := clientset.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		createErr := status.Errorf(codes.Internal, "create service %q: %v", serviceName, err)
		cleanupErr := cleanupCreatedKubernetesResourcesWithTimeout(clientset, namespace, resourceName, deploymentCreated, false, false)
		return nil, withRollbackError(createErr, cleanupErr)
	}
	serviceCreated := true

	if enableAutoScaling && maxReplicas > minReplicas {
		hpa := buildHPA(resourceName, namespace, d.workload, minReplicas, maxReplicas)
		if _, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{}); err != nil {
			createErr := status.Errorf(codes.Internal, "create hpa %q: %v", resourceName, err)
			cleanupErr := cleanupCreatedKubernetesResourcesWithTimeout(clientset, namespace, resourceName, deploymentCreated, serviceCreated, false)
			return nil, withRollbackError(createErr, cleanupErr)
		}
	}

	return &pb.Deployment{
		Id:                 uuid.NewString(),
		Name:               req.GetName(),
		DeploymentId:       resourceName,
		Replicas:           replicas,
		GpusPerReplica:     accelerator.GetCount(),
		GpuType:            accelerator.GetType(),
		Region:             region,
		CreatedBy:          "console-template",
		Status:             deploymentstatus.StatusDeploying,
		TemplateId:         template.GetId(),
		TemplateVersion:    template.GetVersion(),
		ImplementationKind: d.Kind(),
	}, nil
}

func (d *kubernetesDeploymentProvider) Observe(ctx context.Context, deployment *pb.Deployment) (*ObservedStatus, error) {
	if deployment == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clients.Client()
	if err != nil {
		return nil, err
	}

	kubernetesDeployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ObservedStatus{
				Status:     deploymentstatus.StatusDeleted,
				Reason:     "NotFound",
				Message:    "provider deployment resource was not found",
				Replicas:   deployment.GetReplicas(),
				ObservedAt: time.Now().Unix(),
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "get deployment %q: %v", resourceName, err)
	}

	hpa, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "get hpa %q: %v", resourceName, err)
	}
	if apierrors.IsNotFound(err) {
		hpa = nil
	}

	serviceFound := true
	if _, err := clientset.CoreV1().Services(namespace).Get(ctx, resourceName+"-svc", metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			serviceFound = false
		} else {
			return nil, status.Errorf(codes.Internal, "get service %q: %v", resourceName+"-svc", err)
		}
	}

	statusValue, reason, message := resolveKubernetesDeploymentStatus(kubernetesDeployment, serviceFound)
	return &ObservedStatus{
		Status:     statusValue,
		Reason:     reason,
		Message:    message,
		Replicas:   formatReplicaSpec(kubernetesDeployment, hpa),
		ObservedAt: time.Now().Unix(),
	}, nil
}

func (d *kubernetesDeploymentProvider) Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	if deployment == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clients.Client()
	if err != nil {
		return nil, err
	}

	minReplicas, maxReplicas, autoScaling, err := parseReplicaSpec(deployment.GetReplicas())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse replicas %q: %v", deployment.GetReplicas(), err)
	}

	kubernetesDeployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get deployment %q: %v", resourceName, err)
	}
	kubernetesDeployment.Spec.Replicas = &minReplicas
	if _, updateErr := clientset.AppsV1().Deployments(namespace).Update(ctx, kubernetesDeployment, metav1.UpdateOptions{}); updateErr != nil {
		return nil, status.Errorf(codes.Internal, "update deployment %q: %v", resourceName, updateErr)
	}

	hpaClient := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace)
	existingHPA, err := hpaClient.Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "get hpa %q: %v", resourceName, err)
	}

	if autoScaling && maxReplicas > minReplicas {
		desired := buildHPA(resourceName, namespace, d.workload, minReplicas, maxReplicas)
		if apierrors.IsNotFound(err) {
			if _, createErr := hpaClient.Create(ctx, desired, metav1.CreateOptions{}); createErr != nil {
				return nil, status.Errorf(codes.Internal, "create hpa %q: %v", resourceName, createErr)
			}
		} else {
			existingHPA.Spec.MinReplicas = desired.Spec.MinReplicas
			existingHPA.Spec.MaxReplicas = desired.Spec.MaxReplicas
			existingHPA.Spec.Metrics = desired.Spec.Metrics
			if _, updateErr := hpaClient.Update(ctx, existingHPA, metav1.UpdateOptions{}); updateErr != nil {
				return nil, status.Errorf(codes.Internal, "update hpa %q: %v", resourceName, updateErr)
			}
		}
	} else if err == nil {
		if deleteErr := hpaClient.Delete(ctx, resourceName, metav1.DeleteOptions{}); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			return nil, status.Errorf(codes.Internal, "delete hpa %q: %v", resourceName, deleteErr)
		}
	}

	observed, err := d.Observe(ctx, deployment)
	if err != nil {
		return nil, err
	}
	return ApplyObservedStatus(deployment, observed), nil
}

func (d *kubernetesDeploymentProvider) Delete(ctx context.Context, deployment *pb.Deployment) error {
	if deployment == nil {
		return status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clients.Client()
	if err != nil {
		return err
	}

	if err := cleanupCreatedKubernetesResources(ctx, clientset, namespace, resourceName, true, true, true); err != nil {
		return status.Errorf(codes.Internal, "delete provider resources %q: %v", resourceName, err)
	}
	return nil
}

func cleanupCreatedKubernetesResourcesWithTimeout(clientset kubernetes.Interface, namespace, resourceName string, deploymentCreated, serviceCreated, hpaCreated bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubernetesCleanupTimeout)
	defer cancel()
	return cleanupCreatedKubernetesResources(ctx, clientset, namespace, resourceName, deploymentCreated, serviceCreated, hpaCreated)
}

func cleanupCreatedKubernetesResources(ctx context.Context, clientset kubernetes.Interface, namespace, resourceName string, deploymentCreated, serviceCreated, hpaCreated bool) error {
	var cleanupErrs []string
	deletePolicy := metav1.DeletePropagationBackground

	if hpaCreated {
		if err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, resourceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("delete hpa %q: %v", resourceName, err))
		}
	}
	if serviceCreated {
		serviceName := resourceName + "-svc"
		if err := clientset.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("delete service %q: %v", serviceName, err))
		}
	}
	if deploymentCreated {
		if err := clientset.AppsV1().Deployments(namespace).Delete(ctx, resourceName, metav1.DeleteOptions{PropagationPolicy: &deletePolicy}); err != nil && !apierrors.IsNotFound(err) {
			cleanupErrs = append(cleanupErrs, fmt.Sprintf("delete deployment %q: %v", resourceName, err))
		}
	}
	if len(cleanupErrs) > 0 {
		return fmt.Errorf("%s", strings.Join(cleanupErrs, "; "))
	}
	return nil
}

func withRollbackError(primaryErr, cleanupErr error) error {
	if cleanupErr == nil {
		return primaryErr
	}
	return status.Errorf(codes.Internal, "%v; rollback failed: %v", primaryErr, cleanupErr)
}

func (p *kubeconfigClientProvider) Client() (kubernetes.Interface, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cachedClientset != nil {
		return p.cachedClientset, p.cachedNamespace, nil
	}

	namespace := defaultKubernetesNamespace
	if p.cfg.Namespace != "" {
		namespace = p.cfg.Namespace
	}

	var restConfig *rest.Config
	var err error
	kubeconfig := p.cfg.Kubeconfig
	currentContext := p.cfg.Context

	if kubeconfig != "" || currentContext != "" {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			loadingRules.ExplicitPath = kubeconfig
		}
		overrides := &clientcmd.ConfigOverrides{
			CurrentContext: currentContext,
			Context:        clientcmdapiContext(namespace),
		}
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
		restConfig, err = clientConfig.ClientConfig()
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "build kubeconfig client: %v", err)
		}
		if resolvedNamespace, _, nsErr := clientConfig.Namespace(); nsErr == nil && resolvedNamespace != "" {
			namespace = resolvedNamespace
		}
	} else {
		restConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "kubernetes config is not set; configure KUBERNETES_KUBECONFIG (or legacy K8S_KUBECONFIG) or run in-cluster: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "create kubernetes client: %v", err)
	}
	p.cachedClientset = clientset
	p.cachedNamespace = namespace
	return clientset, namespace, nil
}

func validateDeploymentSizing(spec *pb.ModelDeploymentTemplateSpec, req *pb.CreateDeploymentRequest) error {
	if req.GetMinReplicas() < 0 {
		return status.Error(codes.InvalidArgument, "min_replicas must be non-negative")
	}
	if req.GetMaxReplicas() < 0 {
		return status.Error(codes.InvalidArgument, "max_replicas must be non-negative")
	}
	if req.GetAcceleratorCount() < 0 {
		return status.Error(codes.InvalidArgument, "accelerator_count must be non-negative")
	}
	if overrides := req.GetOverrides(); overrides != nil {
		if overrides.GetMinReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "overrides.min_replicas must be non-negative")
		}
		if overrides.GetMaxReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "overrides.max_replicas must be non-negative")
		}
		if overrides.GetMinReplicas() > 0 &&
			overrides.GetMaxReplicas() > 0 &&
			overrides.GetMaxReplicas() < overrides.GetMinReplicas() {
			return status.Error(codes.InvalidArgument, "overrides.max_replicas cannot be less than overrides.min_replicas")
		}
		if overrides.GetEnableAutoScaling() {
			minReplicas := overrides.GetMinReplicas()
			maxReplicas := overrides.GetMaxReplicas()
			if minReplicas <= 0 {
				minReplicas = 1
			}
			if maxReplicas <= minReplicas {
				return status.Error(codes.InvalidArgument, "autoscaling requires overrides.max_replicas greater than overrides.min_replicas")
			}
		}
	}
	if accelerator := spec.GetAccelerator(); accelerator != nil && accelerator.GetCount() < 0 {
		return status.Error(codes.InvalidArgument, "template accelerator.count must be non-negative")
	}
	return nil
}

func buildDeployment(name, namespace string, labels map[string]string, cfg config.KubernetesWorkloadConfig, spec *pb.ModelDeploymentTemplateSpec, replicas int32) *appsv1.Deployment {
	containerPort := defaultContainerPort
	if cfg.ContainerPort > 0 {
		containerPort = cfg.ContainerPort
	}
	engine := spec.GetEngine()
	modelSource := spec.GetModelSource()
	container := corev1.Container{
		Name:  "engine",
		Image: engine.GetImage(),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: containerPort,
		}},
		Args: buildContainerArgs(spec),
		Env:  buildContainerEnv(spec),
	}
	cpuRequest := resource.MustParse(defaultCPURequest)
	if cfg.CPURequest != "" {
		cpuRequest = resource.MustParse(cfg.CPURequest)
	}
	container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: cpuRequest}
	if strings.EqualFold(engine.GetType(), "mock") {
		container.Command = []string{"/bin/sh", "-c"}
		container.Args = []string{strings.Join(container.Args, " ")}
	}
	if healthPath := engine.GetHealthEndpoint(); healthPath != "" {
		livenessDelay := engine.GetReadyTimeoutSeconds()
		if livenessDelay <= 0 {
			livenessDelay = defaultReadyTimeoutSeconds
		}
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt(int(containerPort)),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		}
		container.LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt(int(containerPort)),
				},
			},
			InitialDelaySeconds: livenessDelay,
			PeriodSeconds:       20,
		}
	}

	if accelerator := spec.GetAccelerator(); accelerator != nil && accelerator.GetCount() > 0 && !strings.EqualFold(accelerator.GetType(), "cpu") {
		gpuQty := resource.MustParse(fmt.Sprintf("%d", accelerator.GetCount()))
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		container.Resources.Limits["nvidia.com/gpu"] = gpuQty
		container.Resources.Requests["nvidia.com/gpu"] = gpuQty
	}

	if modelSource != nil && modelSource.GetAuthSecretRef() != "" && modelSource.GetType() == "huggingface" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: "HF_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: modelSource.GetAuthSecretRef()},
					Key:                  "token",
				},
			},
		})
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance": name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/instance": name,
						"app.kubernetes.io/name":     "aibrix-console-deployment",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
				},
			},
		},
	}
}

func buildService(name, namespace string, labels map[string]string, cfg config.KubernetesWorkloadConfig) *corev1.Service {
	servicePort := defaultServicePort
	containerPort := defaultContainerPort
	serviceType := corev1.ServiceTypeClusterIP
	if cfg.ServicePort > 0 {
		servicePort = cfg.ServicePort
	}
	if cfg.ContainerPort > 0 {
		containerPort = cfg.ContainerPort
	}
	switch cfg.ServiceType {
	case string(corev1.ServiceTypeNodePort):
		serviceType = corev1.ServiceTypeNodePort
	case string(corev1.ServiceTypeLoadBalancer):
		serviceType = corev1.ServiceTypeLoadBalancer
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type: serviceType,
			Selector: map[string]string{
				"app.kubernetes.io/instance": strings.TrimSuffix(name, "-svc"),
			},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       servicePort,
				TargetPort: intstr.FromInt(int(containerPort)),
			}},
		},
	}
}

func buildHPA(name, namespace string, cfg config.KubernetesWorkloadConfig, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	targetCPU := defaultHPATargetCPUUtilization
	if cfg.HPATargetCPUUtilization > 0 {
		targetCPU = cfg.HPATargetCPUUtilization
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &targetCPU,
					},
				},
			}},
		},
	}
}

func resolveKubernetesDeploymentStatus(deployment *appsv1.Deployment, serviceFound bool) (string, string, string) {
	if deployment == nil {
		return deploymentstatus.StatusFailed, "DeploymentMissing", "provider deployment resource is missing"
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Reason == "ProgressDeadlineExceeded" {
			return deploymentstatus.StatusFailed, condition.Reason, condition.Message
		}
	}

	desired := defaultReplicaCount
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
		desired = *deployment.Spec.Replicas
	}
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return deploymentstatus.StatusDeploying, "WaitingForObservedGeneration", "deployment controller has not observed the latest generation"
	}
	if deployment.Status.AvailableReplicas >= desired && deployment.Status.ReadyReplicas >= desired {
		if serviceFound {
			return deploymentstatus.StatusReady, "Available", "deployment has the desired number of ready replicas"
		}
		return deploymentstatus.StatusDegraded, "ServiceMissing", "deployment pods are ready but service is missing"
	}
	if deployment.Status.ReadyReplicas > 0 || deployment.Status.AvailableReplicas > 0 || deployment.Status.UpdatedReplicas > 0 {
		return deploymentstatus.StatusScaling, "ReplicaProgressing", "deployment has partial replica progress"
	}
	return deploymentstatus.StatusDeploying, "WaitingForReplicas", "deployment is waiting for ready replicas"
}

func parseReplicaSpec(value string) (minReplicas, maxReplicas int32, autoScaling bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultReplicaCount, defaultReplicaCount, false, nil
	}
	if minValue, maxSection, found := strings.Cut(value, "["); found && strings.HasSuffix(value, "]") {
		minValue = strings.TrimSpace(minValue)
		maxValue := strings.TrimSpace(strings.TrimSuffix(maxSection, "]"))
		minInt, convErr := strconv.Atoi(minValue)
		if convErr != nil {
			return 0, 0, false, convErr
		}
		maxInt, convErr := strconv.Atoi(maxValue)
		if convErr != nil {
			return 0, 0, false, convErr
		}
		if minInt <= 0 {
			minInt = int(defaultReplicaCount)
		}
		if maxInt < minInt {
			maxInt = minInt
		}
		return int32(minInt), int32(maxInt), true, nil
	}
	replicaCount, err := strconv.Atoi(value)
	if err != nil {
		return 0, 0, false, err
	}
	if replicaCount <= 0 {
		replicaCount = int(defaultReplicaCount)
	}
	return int32(replicaCount), int32(replicaCount), false, nil
}

func formatReplicaSpec(deployment *appsv1.Deployment, hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	desired := defaultReplicaCount
	if deployment != nil && deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
		desired = *deployment.Spec.Replicas
	}
	if hpa != nil {
		minReplicas := desired
		if hpa.Spec.MinReplicas != nil && *hpa.Spec.MinReplicas > 0 {
			minReplicas = *hpa.Spec.MinReplicas
		}
		return fmt.Sprintf("%d[%d]", minReplicas, hpa.Spec.MaxReplicas)
	}
	return fmt.Sprintf("%d", desired)
}

func buildContainerArgs(spec *pb.ModelDeploymentTemplateSpec) []string {
	engine := spec.GetEngine()
	modelSource := spec.GetModelSource()
	if strings.EqualFold(engine.GetType(), "mock") {
		if len(engine.GetServeArgs()) > 0 {
			return append([]string(nil), engine.GetServeArgs()...)
		}
		return []string{"WORKER_VICTIM=1 python app.py || true"}
	}

	args := make([]string, 0)
	isSGLang := strings.EqualFold(engine.GetType(), "sglang")
	modelFlag := "--model"
	if isSGLang {
		modelFlag = "--model-path"
	}
	if modelSource != nil && modelSource.GetUri() != "" && !containsFlag(engine.GetServeArgs(), modelFlag) {
		args = append(args, modelFlag, modelSource.GetUri())
	}
	if !isSGLang && modelSource != nil && modelSource.GetRevision() != "" && !containsFlag(engine.GetServeArgs(), "--revision") {
		args = append(args, "--revision", modelSource.GetRevision())
	}
	tokenizerFlag := "--tokenizer"
	if isSGLang {
		tokenizerFlag = "--tokenizer-path"
	}
	if modelSource != nil && modelSource.GetTokenizerPath() != "" && !containsFlag(engine.GetServeArgs(), tokenizerFlag) {
		args = append(args, tokenizerFlag, modelSource.GetTokenizerPath())
	}
	if !isSGLang && modelSource != nil && modelSource.GetChatTemplatePath() != "" && !containsFlag(engine.GetServeArgs(), "--chat-template") {
		args = append(args, "--chat-template", modelSource.GetChatTemplatePath())
	}

	parallelism := spec.GetParallelism()
	if isSGLang {
		args = appendParallelismArg(args, "--tp", parallelism.GetTp())
		args = appendParallelismArg(args, "--pp", parallelism.GetPp())
		args = appendParallelismArg(args, "--dp", parallelism.GetDp())
	} else {
		args = appendParallelismArg(args, "--tensor-parallel-size", parallelism.GetTp())
		args = appendParallelismArg(args, "--pipeline-parallel-size", parallelism.GetPp())
		args = appendParallelismArg(args, "--data-parallel-size", parallelism.GetDp())
	}

	keys := make([]string, 0, len(spec.GetEngineArgs()))
	for key := range spec.GetEngineArgs() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := strings.TrimSpace(spec.GetEngineArgs()[key])
		flag := engineArgFlag(engine.GetType(), key)
		switch strings.ToLower(value) {
		case "true":
			args = append(args, flag)
			continue
		case "false":
			continue
		}
		if value == "" {
			args = append(args, flag)
		} else {
			args = append(args, flag, value)
		}
	}

	if quantization := spec.GetQuantization(); quantization != nil {
		if quantization.GetWeight() != "" {
			args = append(args, "--quantization", quantization.GetWeight())
		}
		if quantization.GetKvCache() != "" {
			args = append(args, "--kv-cache-dtype", quantization.GetKvCache())
		}
	}
	args = append(args, engine.GetServeArgs()...)
	return args
}

func engineArgFlag(engineType, key string) string {
	normalized := strings.ReplaceAll(strings.TrimLeft(key, "-"), "-", "_")
	if strings.EqualFold(engineType, "sglang") {
		switch normalized {
		case "max_model_len":
			normalized = "context_length"
		case "max_num_seqs":
			normalized = "max_running_requests"
		case "gpu_memory_utilization":
			normalized = "mem_fraction_static"
		}
	}
	return "--" + strings.ReplaceAll(normalized, "_", "-")
}

func appendParallelismArg(args []string, flag string, value int32) []string {
	if value > 1 {
		return append(args, flag, strconv.Itoa(int(value)))
	}
	return args
}

func buildContainerEnv(spec *pb.ModelDeploymentTemplateSpec) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "AIBRIX_ENGINE_TYPE", Value: spec.GetEngine().GetType()},
	}
	if modelSource := spec.GetModelSource(); modelSource != nil {
		env = append(env,
			corev1.EnvVar{Name: "AIBRIX_MODEL_SOURCE_TYPE", Value: modelSource.GetType()},
			corev1.EnvVar{Name: "AIBRIX_MODEL_SOURCE_URI", Value: modelSource.GetUri()},
		)
	}
	return env
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func generateResourceName(name string) string {
	base := sanitizeName(name)
	if base == "" {
		base = "deployment"
	}
	suffix := strings.ToLower(uuid.NewString()[:8])
	maxBaseLength := 63 - len("aibrix--") - len(suffix)
	if len(base) > maxBaseLength {
		base = strings.Trim(base[:maxBaseLength], "-")
	}
	return fmt.Sprintf("aibrix-%s-%s", base, suffix)
}

func sanitizeName(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	lastDash := false
	for _, r := range value {
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

func clientcmdapiContext(namespace string) clientcmdapi.Context {
	return clientcmdapi.Context{Namespace: namespace}
}
