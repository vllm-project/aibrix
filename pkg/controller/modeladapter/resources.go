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

package modeladapter

import (
	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func buildModelAdapterEndpointSlice(instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) *discoveryv1.EndpointSlice {
	serviceLabels := map[string]string{
		"kubernetes.io/service-name": instance.Name,
	}

	addresses := []discoveryv1.Endpoint{
		{
			Addresses: []string{pod.Status.PodIP},
		},
	}

	ports := []discoveryv1.EndpointPort{
		{
			Name:     ptr.To("http"),
			Protocol: ptr.To(corev1.ProtocolTCP),
			Port:     ptr.To(int32(8000)),
		},
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:        instance.Name,
			Namespace:   instance.Namespace,
			Labels:      serviceLabels,
			Annotations: make(map[string]string),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(instance, controllerKind),
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   addresses,
		Ports:       ports,
	}
}

func buildModelAdapterService(instance *modelv1alpha1.ModelAdapter) *corev1.Service {
	labels := map[string]string{
		"model.aibrix.ai/name":         instance.Spec.BaseModel,
		"adapter.model.aibrix.ai/name": instance.Name,
	}

	ports := []corev1.ServicePort{
		{
			Name: "http",
			// it should use the base model service port.
			// make sure this can be dynamically configured later.
			Port: 8000,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: 8000,
			},
			Protocol: corev1.ProtocolTCP,
		},
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        instance.Name,
			Namespace:   instance.Namespace,
			Labels:      labels,
			Annotations: make(map[string]string),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(instance, controllerKind),
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Ports:                    ports,
		},
	}
}
