package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/vllm-project/aibrix/apps/console/api/config"
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
	DefaultImplementationKind = "k8s-deployment"
)

type DeploymentDriver interface {
	Kind() string
	Validate(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error
	Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error)
	Get(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error)
	List(ctx context.Context, deployments []*pb.Deployment) ([]*pb.Deployment, error)
	Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error)
	Delete(ctx context.Context, deployment *pb.Deployment) error
}

type Registry struct {
	drivers map[string]DeploymentDriver
}

func NewRegistry(drivers ...DeploymentDriver) *Registry {
	r := &Registry{drivers: map[string]DeploymentDriver{}}
	for _, d := range drivers {
		r.drivers[d.Kind()] = d
	}
	return r
}

func (r *Registry) Get(kind string) (DeploymentDriver, error) {
	if kind == "" {
		kind = DefaultImplementationKind
	}
	d, ok := r.drivers[kind]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported deployment implementation kind %q", kind)
	}
	return d, nil
}

type K8sDeploymentDriver struct {
	cfg *config.Config
}

func NewK8sDeploymentDriver(cfg *config.Config) *K8sDeploymentDriver {
	return &K8sDeploymentDriver{cfg: cfg}
}

func (d *K8sDeploymentDriver) Kind() string {
	return DefaultImplementationKind
}

func (d *K8sDeploymentDriver) Validate(_ context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error {
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
	if compatibility != nil && len(compatibility.GetImplementationKinds()) > 0 {
		allowed := false
		for _, kind := range compatibility.GetImplementationKinds() {
			if kind == d.Kind() {
				allowed = true
				break
			}
		}
		if !allowed {
			return status.Errorf(codes.FailedPrecondition, "template %q does not support implementation %q", template.GetName(), d.Kind())
		}
	}

	if topology := template.GetSpec().GetTopology(); topology != nil && topology.GetKind() == "pd-disaggregated" {
		return status.Errorf(codes.FailedPrecondition, "implementation %q does not support topology %q", d.Kind(), topology.GetKind())
	}
	if template.GetSpec().GetEngine().GetImage() == "" {
		return status.Error(codes.InvalidArgument, "template engine.image is required for k8s-deployment")
	}

	return nil
}

func (d *K8sDeploymentDriver) Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error) {
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
		"aibrix.io/implementation":   d.Kind(),
		"aibrix.io/model-id":         template.GetModelId(),
	}

	replicasValue := minReplicas
	deployment := buildDeployment(resourceName, namespace, labels, d.cfg, spec, replicasValue)
	if _, err := clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		return nil, status.Errorf(codes.Internal, "create deployment %q: %v", resourceName, err)
	}

	serviceName := resourceName + "-svc"
	service := buildService(serviceName, namespace, labels, d.cfg)
	if _, err := clientset.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		return nil, status.Errorf(codes.Internal, "create service %q: %v", serviceName, err)
	}

	if enableAutoScaling && maxReplicas > minReplicas {
		hpa := buildHPA(resourceName, namespace, d.cfg, minReplicas, maxReplicas)
		if _, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa, metav1.CreateOptions{}); err != nil {
			return nil, status.Errorf(codes.Internal, "create hpa %q: %v", resourceName, err)
		}
	}

	return &pb.Deployment{
		Id:                 uuid.NewString(),
		Name:               req.GetName(),
		DeploymentId:       resourceName,
		BaseModel:          baseModel,
		BaseModelId:        baseModelID,
		Replicas:           replicas,
		GpusPerReplica:     accelerator.GetCount(),
		GpuType:            accelerator.GetType(),
		Region:             region,
		CreatedBy:          "console-template",
		Status:             "Deploying",
		TemplateId:         template.GetId(),
		TemplateVersion:    template.GetVersion(),
		ImplementationKind: d.Kind(),
	}, nil
}

func (d *K8sDeploymentDriver) Get(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
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

	current := cloneDeployment(deployment)
	k8sDeployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			current.Status = "Deleted"
			return current, nil
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

	current.Replicas = formatReplicaSpec(k8sDeployment, hpa)
	current.Status = resolveKubernetesDeploymentStatus(k8sDeployment, serviceFound)
	return current, nil
}

func (d *K8sDeploymentDriver) List(ctx context.Context, deployments []*pb.Deployment) ([]*pb.Deployment, error) {
	out := make([]*pb.Deployment, 0, len(deployments))
	for _, deployment := range deployments {
		current, err := d.Get(ctx, deployment)
		if err != nil {
			return nil, err
		}
		out = append(out, current)
	}
	return out, nil
}

func (d *K8sDeploymentDriver) Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error) {
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

	k8sDeployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get deployment %q: %v", resourceName, err)
	}
	k8sDeployment.Spec.Replicas = &minReplicas
	if _, updateErr := clientset.AppsV1().Deployments(namespace).Update(ctx, k8sDeployment, metav1.UpdateOptions{}); updateErr != nil {
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

	return d.Get(ctx, deployment)
}

func (d *K8sDeploymentDriver) Delete(ctx context.Context, deployment *pb.Deployment) error {
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

	deletePolicy := metav1.DeletePropagationBackground
	if err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, resourceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return status.Errorf(codes.Internal, "delete hpa %q: %v", resourceName, err)
	}
	if err := clientset.CoreV1().Services(namespace).Delete(ctx, resourceName+"-svc", metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return status.Errorf(codes.Internal, "delete service %q: %v", resourceName+"-svc", err)
	}
	if err := clientset.AppsV1().Deployments(namespace).Delete(ctx, resourceName, metav1.DeleteOptions{PropagationPolicy: &deletePolicy}); err != nil && !apierrors.IsNotFound(err) {
		return status.Errorf(codes.Internal, "delete deployment %q: %v", resourceName, err)
	}
	return nil
}

func (d *K8sDeploymentDriver) clientset() (kubernetes.Interface, string, error) {
	namespace := "default"
	if d.cfg != nil && d.cfg.K8sNamespace != "" {
		namespace = d.cfg.K8sNamespace
	}

	var restConfig *rest.Config
	var err error
	kubeconfig := ""
	currentContext := ""
	if d.cfg != nil {
		kubeconfig = d.cfg.K8sKubeconfig
		currentContext = d.cfg.K8sContext
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
			return nil, "", status.Errorf(codes.InvalidArgument, "kubernetes config is not set; configure K8S_KUBECONFIG or run in-cluster: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "create kubernetes client: %v", err)
	}
	return clientset, namespace, nil
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
	if cfg != nil && cfg.K8sContainerPort > 0 {
		containerPort = cfg.K8sContainerPort
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
		if cfg.K8sServicePort > 0 {
			servicePort = cfg.K8sServicePort
		}
		if cfg.K8sContainerPort > 0 {
			containerPort = cfg.K8sContainerPort
		}
		switch cfg.K8sServiceType {
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
	if cfg != nil && cfg.K8sHPATargetCPUUtilization > 0 {
		targetCPU = cfg.K8sHPATargetCPUUtilization
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

func resolveKubernetesDeploymentStatus(deployment *appsv1.Deployment, serviceFound bool) string {
	if deployment == nil {
		return "Failed"
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Reason == "ProgressDeadlineExceeded" {
			return "Failed"
		}
	}

	desired := int32(1)
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas > 0 {
		desired = *deployment.Spec.Replicas
	}
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return "Deploying"
	}
	if deployment.Status.AvailableReplicas >= desired && deployment.Status.ReadyReplicas >= desired {
		if serviceFound {
			return "Ready"
		}
		return "Degraded"
	}
	if deployment.Status.ReadyReplicas > 0 || deployment.Status.AvailableReplicas > 0 || deployment.Status.UpdatedReplicas > 0 {
		return "Scaling"
	}
	return "Deploying"
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

func cloneDeployment(src *pb.Deployment) *pb.Deployment {
	if src == nil {
		return nil
	}
	return &pb.Deployment{
		Id:                 src.GetId(),
		Name:               src.GetName(),
		DeploymentId:       src.GetDeploymentId(),
		BaseModel:          src.GetBaseModel(),
		BaseModelId:        src.GetBaseModelId(),
		Replicas:           src.GetReplicas(),
		GpusPerReplica:     src.GetGpusPerReplica(),
		GpuType:            src.GetGpuType(),
		Region:             src.GetRegion(),
		CreatedBy:          src.GetCreatedBy(),
		Status:             src.GetStatus(),
		TemplateId:         src.GetTemplateId(),
		TemplateVersion:    src.GetTemplateVersion(),
		ImplementationKind: src.GetImplementationKind(),
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
