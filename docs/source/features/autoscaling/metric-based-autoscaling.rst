.. _metric-based-autoscaling:

===========================
Metric-based Autoscaling
===========================


AIBrix Autoscaler includes various metric-based autoscaling components, allowing users to conveniently select the appropriate scaler. These options include the Knative-based Kubernetes Pod Autoscaler (KPA), the native Kubernetes Horizontal Pod Autoscaler (HPA), and AIBrix’s custom Advanced Pod Autoscaler (APA) tailored for LLM-serving.

In the following sections, we will demonstrate how users can create various types of autoscalers within AIBrix.


Supported Autoscaling Mechanism
-------------------------------

- HPA: it is same as vanilla K8s HPA. HPA, the native Kubernetes autoscaler, is utilized when users deploy a specification with AIBrix that calls for an HPA. This setup scales the replicas of a demo deployment based on CPU utilization.
- KPA: it is from Knative. KPA has panic mode which scales up more quickly based on short term history. More rapid scaling is possible. The KPA, inspired by Knative, maintains two time windows: a longer ``stable window`` and a shorter ``panic window``. It rapidly scales up resources in response to sudden spikes in traffic based on the panic window measurements. Unlike other solutions that might rely on Prometheus for gathering deployment metrics, AIBrix fetches and maintains metrics internally, enabling faster response times. Example of a KPA scaling operation using a mocked vllm-based Llama2-7b deployment
- APA: similar as HPA but it has fluctuation parameter which acts as minimum buffer before triggering scaling up and down to prevent oscillation.

While HPA and KPA are widely used, they are not specifically designed and optimized for LLM serving, which has distinct optimization points. AIBrix's custom APA (AIBrix Pod Autoscaler) solution will gradually introduce features such as:

- Selecting appropriate LLM-specific metrics for scaling based on AI Runtime metrics standardization.
- Proactive scaling algorithm rather than a reactive one. (WIP)
- Profiling & SLO driven autoscaling solution. (Testing Phase)


Metrics
-------

AiBrix supports all the vllm metrics. Please refer to https://docs.vllm.ai/en/stable/design/metrics.html

How to deploy autoscaling policy
--------------------------------

It is simply applying PodAutoscaler yaml file.
One important thing you should note is that the deployment name and the name in `scaleTargetRef` in PodAutoscaler must be same.
That's how AiBrix PodAutoscaler refers to the right deployment.

All the sample files can be found in the following directory. 

.. code-block:: bash
    
    https://github.com/vllm-project/aibrix/tree/main/samples/autoscaling

Example HPA yaml config
^^^^^^^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../../samples/autoscaling/hpa.yaml
   :language: yaml

Example KPA yaml config
^^^^^^^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../../samples/autoscaling/kpa.yaml
   :language: yaml


Example APA yaml config
^^^^^^^^^^^^^^^^^^^^^^^

.. literalinclude:: ../../../../samples/autoscaling/apa.yaml
   :language: yaml


StormService Role-Level Autoscaling
------------------------------------

For StormService in pooled mode (``replicas=1``), different roles (e.g., prefill and decode) can be autoscaled independently. This enables fine-grained control where each role scales based on its specific metrics.

Use the ``subTargetSelector`` field to target a specific role within a StormService. Please also add the annotation `autoscaling.aibrix.ai/storm-service-mode: "pool"` in PodAutoscaler object for aibrix autoscaler to better
distinguish the ``replicas=1`` scenarios.

**Key features:**

- Each role has its own PodAutoscaler with independent metrics and scaling policies
- Works with StormService in pooled mode (``replicas=1``)
- Supports different scaling strategies (HPA, KPA, APA) per role
- Allows different min/max replicas and scaling behaviors per role

**Complete example:**

.. literalinclude:: ../../../../samples/autoscaling/stormservice-pool.yaml
   :language: yaml

**When to use:**

- **Pooled mode**: StormService with ``replicas=1`` where roles need independent scaling
- **Different workload patterns**: Prefill and decode have different resource needs and traffic patterns
- **Independent metrics**: Each role has its own metrics (e.g., queue length, batch utilization)


Check autoscaling logs
----------------------

Pod Autoscaler Logs
^^^^^^^^^^^^^^^^^^^

Pod autoscaler is part of aibrix controller manager which plays the role of collecting the metrics from each pod. You can
check its logs in this way.

.. code-block:: bash

    kubectl logs <aibrix-controller-manager-podname> -n aibrix-system -f

Expected log output. You can see the current metric is gpu_cache_usage_perc. You can check each pod's current metric value.

.. image:: ../../assets/images/autoscaler/aibrix-controller-manager-output.png
   :alt: AiBrix controller manager output
   :width: 100%
   :align: center


Custom Resource Status
^^^^^^^^^^^^^^^^^^^^^^

To describe the PodAutoscaler custom resource, you can run

.. code-block:: bash

    kubectl describe podautoscaler <podautoscaler-name>

Example output is here, you can explore the scaling conditions and events for more details.

.. image:: ../../assets/images/autoscaler/podautoscaler-describe.png
   :alt: PodAutoscaler describe
   :width: 100%
   :align: center


Preliminary experiments with different autoscalers
--------------------------------------------------

Here we show the preliminary experiment results to show how different autoscaling mechanism and configuration for autoscaler affect the performance(latency) and cost(compute cost).
In AiBrix, user can easily deploy different autoscaler by simply applying K8s yaml.

- Set up
    - Model: Deepseek 7B chatbot model
    - GPU type: V100
    - Max number of GPU: 8
- Target metric and value
    - Target metric: gpu_kv_cache_utilization
    - Target value: 50%
- Workload
    - The overall RPS trend starts with low RPS and goes up relatively fast until T=500 to evaluate how different autoscaler and config reacts to the rapid load increase. After that, it goes down to low RPS quickly to evaluate scaling down behavior and goes up again slowly.
        - Average RPS trend: 1 RPS -> 4 RPS -> 8 RPS -> 10 RPS -> 2 RPS -> 6 RPS
    - RPS can be found in the second subfigure.
- Performance
    - HPA has the highest latency since its slow reaction. KPA is the most reactive with panic mode. APA was running with small delay window to save cost. It does save cost but ends up having higher latency than KPA when it scales down too aggressively from T=700 to T=1000. 
- Cost
    - The fourth figure shows the relative accumulated compute cost over time. The accumulated cost is calculated by multiplying the time by unit cost (in this example, 1). The actual compute cost can be calculated by multiplying the actual cost per unit time.
    - HPA is the most expensive due to the longer delay window for scaling down.
    - APA is the most responsive and saves the cost most. You can see it fluctuating more than other two autoscalers.
    - Note that scaling down window is not inherent feature of each autoscaling mechanism. It is configurable variable. We use the default value for HPA (300s).
- Conclusion
    - There is no one autoscaler that outperforms others for all metrics (latency, cost). In addition, the results might depend on the workloads. Infrastructure should provide easy way to configure whichever autoscaling mechanism they want and should be easily configurable since different users have different preference. For example, one might prefer cost over performance or vice versa. 


.. image:: ../../assets/images/autoscaler/autoscaling_result.png
   :alt: result
   :width: 70%
   :align: center
