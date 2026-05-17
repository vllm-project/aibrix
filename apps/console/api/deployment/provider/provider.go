package provider

import (
	"context"
	"fmt"
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
	// DefaultProviderKind is the canonical provider kind for new Kubernetes deployments.
	DefaultProviderKind = "kubernetes"
	// LegacyKubernetesProviderKind is accepted for existing records and templates
	// created before the provider kind was standardized.
	LegacyKubernetesProviderKind = "k8s-deployment"
	kubernetesCleanupTimeout     = 30 * time.Second
)

type DeploymentProvider interface {
	Kind() string
	Validate(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error
	Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error)
	Observe(ctx context.Context, deployment *pb.Deployment) (*ObservedStatus, error)
	Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error)
	Delete(ctx context.Context, deployment *pb.Deployment) error
}

type ObservedStatus struct {
	Status     string
	Reason     string
	Message    string
	Replicas   string
	ObservedAt int64
}

type Registry struct {
	providers map[string]DeploymentProvider
}

func NewRegistry(providers ...DeploymentProvider) *Registry {
	r := &Registry{providers: map[string]DeploymentProvider{}}
	for _, p := range providers {
		r.providers[p.Kind()] = p
	}
	return r
}

func (r *Registry) Get(kind string) (DeploymentProvider, error) {
	kind = normalizeProviderKind(kind)
	p, ok := r.providers[kind]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported deployment provider kind %q", kind)
	}
	return p, nil
}

func normalizeProviderKind(kind string) string {
	switch kind {
	case "", LegacyKubernetesProviderKind:
		return DefaultProviderKind
	default:
		return kind
	}
}

func isCompatibleProviderKind(kind, providerKind string) bool {
	return normalizeProviderKind(kind) == providerKind
}

type KubernetesDeploymentProvider struct {
	cfg             *config.Config
	mu              sync.Mutex
	cachedClientset kubernetes.Interface
	cachedNamespace string
}

func NewKubernetesDeploymentProvider(cfg *config.Config) *KubernetesDeploymentProvider {
	return &KubernetesDeploymentProvider{cfg: cfg}
}

func (d *KubernetesDeploymentProvider) Kind() string {
	return DefaultProviderKind
}

func (d *KubernetesDeploymentProvider) Validate(_ context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error {
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

	compatibility := template.GetSpec().GetCompatibility()
	if compatibility != nil && len(compatibility.GetProviderKinds()) > 0 {
		allowed := false
		for _, kind := range compatibility.GetProviderKinds() {
			if isCompatibleProviderKind(kind, d.Kind()) {
				allowed = true
				break
			}
		}
		if !allowed {
			return status.Errorf(codes.FailedPrecondition, "template %q does not support provider %q", template.GetName(), d.Kind())
		}
	}

	if topology := template.GetSpec().GetTopology(); topology != nil && topology.GetKind() == "pd-disaggregated" {
		return status.Errorf(codes.FailedPrecondition, "provider %q does not support topology %q", d.Kind(), topology.GetKind())
	}
	if template.GetSpec().GetEngine().GetImage() == "" {
		return status.Error(codes.InvalidArgument, "template engine.image is required for kubernetes")
	}
	if err := validateDeploymentSizing(template.GetSpec(), req); err != nil {
		return err
	}

	return nil
}

func (d *KubernetesDeploymentProvider) Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
	spec := template.GetSpec()
	accelerator := spec.GetAccelerator()
	overrides := req.GetOverrides()
	scalingDefaults := spec.GetScalingDefaults()

	minReplicas := int32(1)
	maxReplicas := int32(1)
	enableAutoScaling := false
	if scalingDefaults != nil {
		if scalingDefaults.GetMinReplicas() > 0 {
			minReplicas = scalingDefaults.GetMinReplicas()
		}
		if scalingDefaults.GetMaxReplicas() > 0 {
			maxReplicas = scalingDefaults.GetMaxReplicas()
		}
		enableAutoScaling = scalingDefaults.GetEnableAutoScaling()
	}
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

	baseModel := template.GetName()
	baseModelID := template.GetModelId()
	if modelSource := spec.GetModelSource(); modelSource != nil && modelSource.GetUri() != "" {
		baseModel = modelSource.GetUri()
	}

	region := req.GetRegion()
	if overrides != nil && overrides.GetRegion() != "" {
		region = overrides.GetRegion()
	}

	clientset, namespace, err := d.clientset()
	if err != nil {
		return nil, err
	}
	if err := ensureNamespace(ctx, clientset, namespace); err != nil {
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
	deployment := buildDeployment(resourceName, namespace, labels, d.cfg, spec, replicasValue)
	if _, err := clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		return nil, status.Errorf(codes.Internal, "create deployment %q: %v", resourceName, err)
	}
	deploymentCreated := true

	serviceName := resourceName + "-svc"
	service := buildService(serviceName, namespace, labels, d.cfg)
	if _, err := clientset.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		createErr := status.Errorf(codes.Internal, "create service %q: %v", serviceName, err)
		cleanupErr := cleanupCreatedKubernetesResourcesWithTimeout(clientset, namespace, resourceName, deploymentCreated, false, false)
		return nil, withRollbackError(createErr, cleanupErr)
	}
	serviceCreated := true

	if enableAutoScaling && maxReplicas > minReplicas {
		hpa := buildHPA(resourceName, namespace, d.cfg, minReplicas, maxReplicas)
		if _, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{}); err != nil {
			createErr := status.Errorf(codes.Internal, "create hpa %q: %v", resourceName, err)
			cleanupErr := cleanupCreatedKubernetesResourcesWithTimeout(clientset, namespace, resourceName, deploymentCreated, serviceCreated, false)
			return nil, withRollbackError(createErr, cleanupErr)
		}
	}

	return &pb.Deployment{
		Id:              uuid.NewString(),
		Name:            req.GetName(),
		DeploymentId:    resourceName,
		BaseModel:       baseModel,
		BaseModelId:     baseModelID,
		Replicas:        replicas,
		GpusPerReplica:  accelerator.GetCount(),
		GpuType:         accelerator.GetType(),
		Region:          region,
		CreatedBy:       "console-template",
		Status:          deploymentstatus.StatusDeploying,
		TemplateId:      template.GetId(),
		TemplateVersion: template.GetVersion(),
		ProviderKind:    d.Kind(),
	}, nil
}

func (d *KubernetesDeploymentProvider) Observe(ctx context.Context, deployment *pb.Deployment) (*ObservedStatus, error) {
	if deployment == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clientset()
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

func (d *KubernetesDeploymentProvider) Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
	if deployment == nil {
		return nil, status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clientset()
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
		desired := buildHPA(resourceName, namespace, d.cfg, minReplicas, maxReplicas)
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

func (d *KubernetesDeploymentProvider) Delete(ctx context.Context, deployment *pb.Deployment) error {
	if deployment == nil {
		return status.Error(codes.InvalidArgument, "deployment is required")
	}
	resourceName := deployment.GetDeploymentId()
	if resourceName == "" {
		return status.Error(codes.InvalidArgument, "deployment_id is required")
	}

	clientset, namespace, err := d.clientset()
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

func (d *KubernetesDeploymentProvider) clientset() (kubernetes.Interface, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cachedClientset != nil {
		return d.cachedClientset, d.cachedNamespace, nil
	}

	namespace := "default"
	if d.cfg != nil && d.cfg.KubernetesProvider.Namespace != "" {
		namespace = d.cfg.KubernetesProvider.Namespace
	}

	var restConfig *rest.Config
	var err error
	kubeconfig := ""
	currentContext := ""
	if d.cfg != nil {
		kubeconfig = d.cfg.KubernetesProvider.Kubeconfig
		currentContext = d.cfg.KubernetesProvider.Context
	}

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
	d.cachedClientset = clientset
	d.cachedNamespace = namespace
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
	if scalingDefaults := spec.GetScalingDefaults(); scalingDefaults != nil {
		if scalingDefaults.GetMinReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "template scaling_defaults.min_replicas must be non-negative")
		}
		if scalingDefaults.GetMaxReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "template scaling_defaults.max_replicas must be non-negative")
		}
	}
	if overrides := req.GetOverrides(); overrides != nil {
		if overrides.GetMinReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "overrides.min_replicas must be non-negative")
		}
		if overrides.GetMaxReplicas() < 0 {
			return status.Error(codes.InvalidArgument, "overrides.max_replicas must be non-negative")
		}
	}
	if accelerator := spec.GetAccelerator(); accelerator != nil && accelerator.GetCount() < 0 {
		return status.Error(codes.InvalidArgument, "template accelerator.count must be non-negative")
	}
	return nil
}

func ensureNamespace(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	if namespace == "" {
		return status.Error(codes.InvalidArgument, "kubernetes namespace is required")
	}
	if _, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return status.Errorf(codes.Internal, "get namespace %q: %v", namespace, err)
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return status.Errorf(codes.Internal, "create namespace %q: %v", namespace, err)
	}
	return nil
}

func buildDeployment(name, namespace string, labels map[string]string, cfg *config.Config, spec *pb.ModelDeploymentTemplateSpec, replicas int32) *appsv1.Deployment {
	containerPort := int32(8000)
	if cfg != nil && cfg.KubernetesProvider.ContainerPort > 0 {
		containerPort = cfg.KubernetesProvider.ContainerPort
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
	if healthPath := engine.GetHealthEndpoint(); healthPath != "" {
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
			InitialDelaySeconds: 20,
			PeriodSeconds:       20,
		}
	}

	if accelerator := spec.GetAccelerator(); accelerator != nil && accelerator.GetCount() > 0 && !strings.EqualFold(accelerator.GetType(), "cpu") {
		gpuQty := resource.MustParse(fmt.Sprintf("%d", accelerator.GetCount()))
		container.Resources = corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{"nvidia.com/gpu": gpuQty},
			Requests: corev1.ResourceList{"nvidia.com/gpu": gpuQty},
		}
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

func buildService(name, namespace string, labels map[string]string, cfg *config.Config) *corev1.Service {
	servicePort := int32(8000)
	containerPort := int32(8000)
	serviceType := corev1.ServiceTypeClusterIP
	if cfg != nil {
		if cfg.KubernetesProvider.ServicePort > 0 {
			servicePort = cfg.KubernetesProvider.ServicePort
		}
		if cfg.KubernetesProvider.ContainerPort > 0 {
			containerPort = cfg.KubernetesProvider.ContainerPort
		}
		switch cfg.KubernetesProvider.ServiceType {
		case string(corev1.ServiceTypeNodePort):
			serviceType = corev1.ServiceTypeNodePort
		case string(corev1.ServiceTypeLoadBalancer):
			serviceType = corev1.ServiceTypeLoadBalancer
		}
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

func buildHPA(name, namespace string, cfg *config.Config, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	targetCPU := int32(80)
	if cfg != nil && cfg.KubernetesProvider.HPATargetCPUUtilization > 0 {
		targetCPU = cfg.KubernetesProvider.HPATargetCPUUtilization
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

	desired := int32(1)
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
		return 1, 1, false, nil
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
			minInt = 1
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
		replicaCount = 1
	}
	return int32(replicaCount), int32(replicaCount), false, nil
}

func formatReplicaSpec(deployment *appsv1.Deployment, hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	desired := int32(1)
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

func ApplyObservedStatus(deployment *pb.Deployment, observed *ObservedStatus) *pb.Deployment {
	current := cloneDeployment(deployment)
	if current == nil || observed == nil {
		return current
	}
	if observed.Status != "" {
		current.Status = observed.Status
	}
	if observed.Replicas != "" {
		current.Replicas = observed.Replicas
	}
	return current
}

func cloneDeployment(src *pb.Deployment) *pb.Deployment {
	if src == nil {
		return nil
	}
	return &pb.Deployment{
		Id:              src.GetId(),
		Name:            src.GetName(),
		DeploymentId:    src.GetDeploymentId(),
		BaseModel:       src.GetBaseModel(),
		BaseModelId:     src.GetBaseModelId(),
		Replicas:        src.GetReplicas(),
		GpusPerReplica:  src.GetGpusPerReplica(),
		GpuType:         src.GetGpuType(),
		Region:          src.GetRegion(),
		CreatedBy:       src.GetCreatedBy(),
		Status:          src.GetStatus(),
		TemplateId:      src.GetTemplateId(),
		TemplateVersion: src.GetTemplateVersion(),
		ProviderKind:    src.GetProviderKind(),
	}
}

func buildContainerArgs(spec *pb.ModelDeploymentTemplateSpec) []string {
	engine := spec.GetEngine()
	modelSource := spec.GetModelSource()
	args := append([]string(nil), engine.GetServeArgs()...)
	if modelSource != nil && modelSource.GetUri() != "" && !containsFlag(args, "--model") {
		args = append(args, "--model", modelSource.GetUri())
	}
	if modelSource != nil && modelSource.GetRevision() != "" && !containsFlag(args, "--revision") {
		args = append(args, "--revision", modelSource.GetRevision())
	}
	if modelSource != nil && modelSource.GetTokenizerPath() != "" && !containsFlag(args, "--tokenizer") {
		args = append(args, "--tokenizer", modelSource.GetTokenizerPath())
	}
	if modelSource != nil && modelSource.GetChatTemplatePath() != "" && !containsFlag(args, "--chat-template") {
		args = append(args, "--chat-template", modelSource.GetChatTemplatePath())
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
	for key, value := range spec.GetEngineArgs() {
		env = append(env, corev1.EnvVar{
			Name:  toEnvName("AIBRIX_ENGINE_ARG_" + key),
			Value: value,
		})
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
	full := fmt.Sprintf("aibrix-%s-%s", base, strings.ToLower(uuid.NewString()[:6]))
	if len(full) > 63 {
		full = full[:63]
	}
	return strings.Trim(full, "-")
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

func toEnvName(value string) string {
	value = strings.ToUpper(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func clientcmdapiContext(namespace string) clientcmdapi.Context {
	return clientcmdapi.Context{Namespace: namespace}
}
