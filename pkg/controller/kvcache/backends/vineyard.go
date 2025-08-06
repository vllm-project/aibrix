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

package backends

import (
	"context"
	"errors"
	"fmt"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type VineyardReconciler struct {
	*BaseReconciler
}

func NewVineyardReconciler(c client.Client) *VineyardReconciler {
	return &VineyardReconciler{&BaseReconciler{Client: c}}
}

func (r VineyardReconciler) Reconcile(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) (reconcile.Result, error) {
	// Handle metadata Pods like etcd or redis in future.
	err := r.reconcileMetadataService(ctx, kvCache)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle Vineyard kvCache Deployment
	deployment := buildVineyardDeployment(kvCache)
	if err := r.ReconcileDeploymentObject(ctx, deployment); err != nil {
		return ctrl.Result{}, err
	}

	// Handle Vineyard Services
	service := buildVineyardRpcService(kvCache)
	if err := r.ReconcileServiceObject(ctx, service); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *VineyardReconciler) reconcileMetadataService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	if kvCache.Spec.Metadata != nil && kvCache.Spec.Metadata.Etcd == nil && kvCache.Spec.Metadata.Redis == nil {
		return errors.New("either etcd or redis configuration is required")
	}

	if kvCache.Spec.Metadata.Etcd != nil {
		return r.reconcileEtcdService(ctx, kvCache)
	}

	return nil
}

func (r *VineyardReconciler) reconcileEtcdService(ctx context.Context, kvCache *orchestrationv1alpha1.KVCache) error {
	// We only support etcd at this moment, redis will be supported later.
	replicas := int(kvCache.Spec.Metadata.Etcd.Runtime.Replicas)

	// Reconcile etcd pods and services based on the number of replicas specified
	for i := 0; i < replicas; i++ {
		etcdPodName := fmt.Sprintf("%s-etcd-%d", kvCache.Name, i)

		// Create or update the etcd pod
		pod := buildEtcdPod(kvCache, etcdPodName)
		if err := r.ReconcilePodObject(ctx, pod); err != nil {
			return err
		}

		// Create or update the etcd service for each pod
		etcdService := buildEtcdPodService(etcdPodName, kvCache)
		if err := r.ReconcileServiceObject(ctx, etcdService); err != nil {
			return err
		}
	}

	// Reconcile the RPC service once for all etcd instances
	etcdService := buildEtcdAggregateService(kvCache)

	if err := r.ReconcileServiceObject(ctx, etcdService); err != nil {
		return err
	}

	return nil
}

func buildEtcdPod(kvCache *orchestrationv1alpha1.KVCache, etcdPodName string) *corev1.Pod {
	image := kvCache.Spec.Cache.Image
	if kvCache.Spec.Metadata.Etcd.Runtime.Image != "" {
		image = kvCache.Spec.Metadata.Etcd.Runtime.Image
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      etcdPodName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier:    kvCache.Name,
				constants.KVCacheLabelKeyRole:          constants.KVCacheLabelValueRoleMetadata,
				constants.KVCacheLabelKeyMetadataIndex: etcdPodName,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "etcd",
					Image: image, // share the same kv cache image
					Ports: []corev1.ContainerPort{
						{Name: "client", ContainerPort: 2379, Protocol: corev1.ProtocolTCP},
						{Name: "server", ContainerPort: 2380, Protocol: corev1.ProtocolTCP},
					},
					Command: []string{
						"etcd",
						"--name",
						etcdPodName,
						"--initial-advertise-peer-urls",
						fmt.Sprintf("http://%s:2380", etcdPodName),
						"--advertise-client-urls",
						fmt.Sprintf("http://%s:2379", etcdPodName),
						"--listen-peer-urls",
						"http://0.0.0.0:2380",
						"--listen-client-urls",
						"http://0.0.0.0:2379",
						"--initial-cluster",
						fmt.Sprintf("%s=http://%s:2380", etcdPodName, etcdPodName),
						"--initial-cluster-state",
						"new",
					},
				},
			},
		},
	}
	return pod
}

func buildEtcdPodService(etcdPodName string, kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	etcdService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      etcdPodName,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier:    kvCache.Name,
				constants.KVCacheLabelKeyRole:          constants.KVCacheLabelValueRoleMetadata,
				constants.KVCacheLabelKeyMetadataIndex: etcdPodName,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "client", Port: 2379, TargetPort: intstr.FromInt32(2379), Protocol: corev1.ProtocolTCP},
				{Name: "server", Port: 2380, TargetPort: intstr.FromInt32(2380), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier:    kvCache.Name,
				constants.KVCacheLabelKeyRole:          constants.KVCacheLabelValueRoleMetadata,
				constants.KVCacheLabelKeyMetadataIndex: etcdPodName,
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	return etcdService
}

func buildEtcdAggregateService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	etcdService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-etcd-service", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "etcd-port", Port: 2379, TargetPort: intstr.FromInt32(2379), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleMetadata,
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	return etcdService
}

func buildVineyardDeployment(kvCache *orchestrationv1alpha1.KVCache) *appsv1.Deployment {
	envs := []corev1.EnvVar{
		{Name: "VINEYARDD_UID", Value: string(kvCache.ObjectMeta.UID)},
		{Name: "VINEYARDD_NAME", Value: kvCache.Name},
		{Name: "VINEYARDD_NAMESPACE", Value: kvCache.Namespace},
		{Name: "MY_NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "MY_POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "MY_POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "MY_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "MY_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "MY_HOST_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "USER", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	}

	affinity := corev1.Affinity{}

	if value, ok := kvCache.Annotations[constants.KVCacheAnnotationNodeAffinityGPUType]; ok {
		matchKey := constants.KVCacheAnnotationNodeAffinityDefaultKey
		if key, exist := kvCache.Annotations[constants.KVCacheAnnotationNodeAffinityKey]; exist {
			matchKey = key
		}

		nodeAffinity := &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      matchKey,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{value},
							},
						},
					},
				},
			},
		}
		affinity.NodeAffinity = nodeAffinity
	}

	if value, ok := kvCache.Annotations[constants.KVCacheAnnotationPodAffinityKey]; ok {
		podAffinity := &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      constants.ModelLabelName,
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{value},
							},
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		}
		affinity.PodAffinity = podAffinity
	}

	if _, ok := kvCache.Annotations[constants.KVCacheAnnotationPodAntiAffinity]; ok {
		podAntiAffinity := &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							constants.KVCacheLabelKeyIdentifier: kvCache.Name,
							constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		}
		affinity.PodAntiAffinity = podAntiAffinity
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kvCache.Name,
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       "cache",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &kvCache.Spec.Cache.Replicas,
			Selector: &metav1.LabelSelector{
				// TODO: update metadata
				MatchLabels: map[string]string{
					constants.KVCacheLabelKeyIdentifier: kvCache.Name,
					constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						constants.KVCacheLabelKeyIdentifier: kvCache.Name,
						constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "kvcache",
							Image: kvCache.Spec.Cache.Image,
							Ports: []corev1.ContainerPort{
								{Name: "rpc", ContainerPort: 9600, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{
								"/bin/bash",
								"-c",
								fmt.Sprintf(`/usr/local/bin/vineyardd --sync_crds true --socket /var/run/vineyard.sock --size --stream_threshold 80 --etcd_cmd etcd --etcd_prefix /vineyard --etcd_endpoint http://%s-etcd-service:2379`, kvCache.Name),
							},
							Env:             append(envs, kvCache.Spec.Cache.Env...),
							Resources:       kvCache.Spec.Cache.Resources,
							ImagePullPolicy: corev1.PullPolicy(kvCache.Spec.Cache.ImagePullPolicy),
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"ls", "/var/run/vineyard.sock"},
									},
								},
							},
							LivenessProbe: &corev1.Probe{
								FailureThreshold: 3,
								PeriodSeconds:    60,
								SuccessThreshold: 1,
								TimeoutSeconds:   1,
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt32(9600),
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "vineyard-socket", MountPath: "/var/run"},
								{Name: "shm", MountPath: "/dev/shm"},
								{Name: "log", MountPath: "/var/log/vineyard"},
							},
						},
					},
					Affinity: &affinity,
					Tolerations: []corev1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "vineyard-socket",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: fmt.Sprintf("/var/run/vineyard-kubernetes/%s/%s", kvCache.Namespace, kvCache.Name),
								},
							},
						},
						{
							Name: "shm",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									Medium: "Memory",
								},
							},
						},
						{
							Name: "log",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	return deployment
}

func buildVineyardRpcService(kvCache *orchestrationv1alpha1.KVCache) *corev1.Service {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-rpc", kvCache.Name),
			Namespace: kvCache.Namespace,
			Labels: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(kvCache, orchestrationv1alpha1.GroupVersion.WithKind("KVCache")),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "vineyard-rpc", Port: 9600, TargetPort: intstr.FromInt32(9600), Protocol: corev1.ProtocolTCP},
			},
			Selector: map[string]string{
				constants.KVCacheLabelKeyIdentifier: kvCache.Name,
				constants.KVCacheLabelKeyRole:       constants.KVCacheLabelValueRoleCache,
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	return service
}
