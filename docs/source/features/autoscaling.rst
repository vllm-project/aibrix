.. _autoscaling:

===========
Autoscaling
===========

Overview of AIBrix Autoscaler
-----------------------------

Autoscaling is crucial for deploying Large Language Model (LLM) services on Kubernetes (k8s), as timely scaling up handles peaks in request traffic, and scaling down conserves resources when demand wanes.

AIBrix Autoscaler includes various autoscaling components, allowing users to conveniently select the appropriate scaler. These options include the Knative-based Kubernetes Pod Autoscaler (KPA), the native Kubernetes Horizontal Pod Autoscaler (HPA), and AIBrix’s custom Advanced Pod Autoscaler (APA) tailored for LLM-serving.

In the following sections, we will demonstrate how users can create various types of autoscalers within AIBrix.

KPA Autoscaler
--------------

The KPA, inspired by Knative, maintains two time windows: a longer ``stable window`` and a shorter ``panic window``. It rapidly scales up resources in response to sudden spikes in traffic based on the panic window measurements.

Unlike other solutions that might rely on Prometheus for gathering deployment metrics, AIBrix fetches and maintains metrics internally, enabling faster response times.

Example of a KPA scaling operation using a mocked vllm-based Llama2-7b deployment

.. code-block:: bash

    kubectl apply -f docs/development/app/deployment.yaml
    kubectl get deployments --all-namespaces | grep llama2

Expected deployment status

.. code-block:: console

    NAME         READY   UP-TO-DATE   AVAILABLE   AGE
    llama2-70b   3/3     3            3           16s

Create an autoscaler of type KPA

.. code-block:: bash

    kubectl apply -f config/samples/autoscaling_v1alpha1_mock_llama.yaml
    kubectl get podautoscalers --all-namespaces

Expected KPA scaler status

.. code-block:: console

    NAMESPACE   NAME                               AGE
    default     podautoscaler-example-mock-llama   10s

Deployment scaled-up example

.. code-block:: console

    kubectl get deployments --all-namespaces | grep llama2
    NAME         READY   UP-TO-DATE   AVAILABLE   AGE
    llama2-70b   5/5     5            5           9m47s

HPA Autoscaler
--------------

HPA, the native Kubernetes autoscaler, is utilized when users deploy a specification with AIBrix that calls for an HPA. This setup scales the replicas of a demo deployment based on CPU utilization.

Example of setting up an HPA with an Nginx application

.. code-block:: bash

    kubectl apply -f config/samples/autoscaling_v1alpha1_demo_nginx.yaml
    kubectl apply -f config/samples/autoscaling_v1alpha1_podautoscaler.yaml
    kubectl get podautoscalers --all-namespaces
    kubectl get deployments.apps
    kubectl get hpa

Expected outputs

.. code-block:: console

    NAMESPACE   NAME                    AGE
    default     podautoscaler-example   24s

    NAME               READY   UP-TO-DATE   AVAILABLE   AGE
    nginx-deployment   1/1     1            1           8s

    NAME                        REFERENCE                     TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
    podautoscaler-example-hpa   Deployment/nginx-deployment   0%/10%    1         10        1          2m28s

Increase load to trigger autoscaling

.. code-block:: bash

    kubectl run load-generator --image=busybox -- /bin/sh -c "while true; do wget -q -O- http://nginx-service.default.svc.cluster.local; done"

Observe the scaling effect

.. code-block:: console

    kubectl get pods --all-namespaces

Expected scaling response

.. code-block:: console

    NAME                                READY   STATUS    RESTARTS   AGE
    load-generator                      1/1     Running   0          86s
    nginx-deployment-5b85cc87b7-gr94j   1/1     Running   0          56s
    nginx-deployment-5b85cc87b7-lwqqk   1/1     Running   0          56s
    nginx-deployment-5b85cc87b7-q2gmp   1/1     Running   0          4m33s

APA Autoscaler
--------------

While HPA and KPA are widely used, they are not specifically designed and optimized for LLM serving, which has distinct optimization points. AIBrix's custom APA (AIBrix Pod Autoscaler) solution will gradually introduce features such as:

1. Selecting appropriate metrics for scaling based on AI Runtime metrics standardization, allowing autoscaling across various LLM-serving engines (e.g., vllm, hgi, triton) based on LLM-specific metrics.
2. For users with heterogeneous GPU resources, combining LLM and GPU features.
3. Implementing a proactive scaling algorithm rather than a reactive one.