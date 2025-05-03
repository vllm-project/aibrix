.. _observability:

=============
Observability
=============

To enable observability for your AIBrix deployment, we provide **Built-in Grafana Dashboards** that cover the key system components:

1. **Control Plane Runtime Dashboard**
   - Monitors controller runtime performance, reconciliation behavior, and health status of the control plane.

2. **Envoy Gateway Dashboard**
   - Visualizes traffic metrics including request counts, latencies, and external processing statistics.

3. **Model Service Dashboard**
   - Tracks per-model service metrics such as request QPS, prompt and output length, TTFT/TPOT, and stop reasons etc.

Prerequisites
-------------

Before enabling metrics and dashboards, make sure the `kube-prometheus-stack <https://github.com/prometheus-community/helm-charts/blob/main/charts/kube-prometheus-stack/README.md>`_ is installed in your cluster. This provides Prometheus, Grafana, and CRDs like `ServiceMonitor` required for scraping metrics.

.. code-block:: bash

    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm repo update
    helm install prometheus prometheus-community/kube-prometheus-stack --namespace prometheus

Metric Enablement Steps
-----------------------

To activate metric collection for each component:

1. **Control Plane Runtime**
   - The default controller manager installation already expose the metrics.

.. literalinclude:: ../../../observability/monitor/service_monitor_controller_manager.yaml
   :language: yaml

3. **Envoy Gateway**
   - In addition to a `ServiceMonitor`, you must deploy an **auxiliary metrics service** that exposes Envoy's admin interface metrics (e.g., `/stats/prometheus`) to Prometheus.

.. literalinclude:: ../../../observability/monitor/envoy_metrics_service.yaml
   :language: yaml

.. literalinclude:: ../../../observability/monitor/service_monitor_gateway.yaml
   :language: yaml


3. **Model Service**
   - We provides a sample `ServiceMonitor` as a reference, you can change the definition based on your model setups.

.. literalinclude:: ../../../observability/monitor/service_monitor_vllm.yaml
   :language: yaml

Import Grafana Dashboard
------------------------

For production monitoring, we provide pre-built Grafana dashboards to visualize metrics from the control plane, Envoy Gateway, and model services.
These dashboards offer insights into system performance, request patterns, error rates, and more.
You can import them into your Grafana instance by uploading the corresponding JSON files.
Ensure your Prometheus data source is correctly configured before importing. Once imported, the dashboards will begin displaying live metrics as long as `ServiceMonitor` resources are properly set up and the kube-prometheus stack is actively scraping data.

- `AIBrix Control Plane Runtime Dashboard <../../../observability/grafana/AIBrix_Control_Plane_Runtime_Dashboard.json>`_
- `AIBrix Envoy Gateway Dashboard <../../../observability/grafana/AIBrix_Envoy_Gateway_Dashboard.json>`_
- `AIBrix vLLM Engine Dashboard <../../../observability/grafana/AIBrix_vLLM_Engine_Dashboard.json>`_

Production Monitoring
---------------------

 TODO: Screenshots and visual examples will be added soon to illustrate key views and usage patterns.
