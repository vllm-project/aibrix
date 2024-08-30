# AIBrix PodAutoscaler Demo

This demonstration will showcase how the AIBrix PodAutoscaler (abbreviated as AIBrix-pa) dynamically 
adjusts the number of replicas for an Nginx service based on CPU utilization.

# Build CRDs and Run Manager

## Compile, Build Docker or Run Local

Go into the root directory:

```shell
cd $AIBrix_HOME
```

First, build and install the Custom Resource Definitions (CRDs) for AIBrix:

```shell

make manifests && make build && make install
```

Verify the installation:

```shell
kubectl get crds | grep podautoscalers
```

The expected output is as follows:

```log
# podautoscalers.autoscaling.aibrix.ai
```
## Start the AIBrix Manager

Open a separate terminal to start the AIBrix manager. This process is synchronous:

```shell
make run
```

You should see the following logs if the manager launches successfully:

```log
2024-07-29T11:37:40+08:00	INFO	setup	starting manager
2024-07-29T11:37:40+08:00	INFO	starting server	{"kind": "health probe", "addr": "[::]:8081"}
2024-07-29T11:37:40+08:00	INFO	controller-runtime.metrics	Starting metrics server
...
Starting workers	{"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "worker count": 1}
...

```

## Start 2: Build and Deploy Manager
It's different from `make run`, since it may reveal the RBAC problem when manager what to watch HPA.

```shell
make docker-build IMG=aibrix/aibrix-controller-manager:v0.1.0-rc.0
make deploy IMG=aibrix/aibrix-controller-manager:v0.1.0-rc.0
```

check the deployed manager logs:
```shell
kubectl get pods -n aibrix-system -o name | grep aibrix-controller-manager | head -n 1 | xargs -I {} kubectl logs {} -n aibrix-system
```


Or you can add `-f` to watch manager's logs continuously:

```shell
kubectl get pods -n aibrix-system -o name | grep aibrix-controller-manager | head -n 1 | xargs -I {} kubectl logs -f {} -n aibrix-system
```

Expected output (no warnings, no errors):

```log
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "source": "kind source: *v1alpha1.PodAutoscaler"}
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "source": "kind source: *v2.HorizontalPodAutoscaler"}
2024-08-05T10:20:03Z    INFO    Starting Controller     {"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler"}
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter", "source": "kind source: *v1alpha1.ModelAdapter"}
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter", "source": "kind source: *v1.Service"}
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter", "source": "kind source: *v1.EndpointSlice"}
2024-08-05T10:20:03Z    INFO    Starting EventSource    {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter", "source": "kind source: *v1.Pod"}
2024-08-05T10:20:03Z    INFO    Starting Controller     {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter"}
2024-08-05T10:20:03Z    INFO    Starting workers        {"controller": "modeladapter", "controllerGroup": "model.aibrix.ai", "controllerKind": "ModelAdapter", "worker count": 1}
2024-08-05T10:20:03Z    INFO    Starting workers        {"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "worker count": 1}
```


# Case 1: Create HPA-based AIBrix PodAutoscaler to scale the Demo Nginx App 

Deploy an Nginx application and an AIBrix-pa designed to maintain the CPU usage of the Nginx pods below 10%. 
The AIBrix-pa will automatically create a corresponding Horizontal Pod Autoscaler (HPA) to achieve this target.

```shell
# Create nginx
kubectl apply -f config/samples/autoscaling_v1alpha1_demo_nginx.yaml
# Create AIBrix-pa
kubectl apply -f config/samples/autoscaling_v1alpha1_podautoscaler.yaml
```

After applying the configurations, you should see:

```shell
kubectl get podautoscalers --all-namespaces
```

The expected output is as follows:

```log
>>> NAMESPACE   NAME                    AGE
>>> default     podautoscaler-example   24s

kubectl get deployments.apps

>>> NAME               READY   UP-TO-DATE   AVAILABLE   AGE
>>> nginx-deployment   1/1     1            1           8s

```

A corresponding HPA will also be created:

```shell
kubectl get hpa
```

The expected output is as follows:

```log
>>> NAME                        REFERENCE                     TARGETS   MINPODS   MAXPODS   REPLICAS   AGE
>>> podautoscaler-example-hpa   Deployment/nginx-deployment   0%/10%    1         10        1          2m28s
```

## Apply Pressure to See the Effect

Use a simple workload generator to increase load:

```shell
kubectl run load-generator --image=busybox -- /bin/sh -c "while true; do wget -q -O- http://nginx-service.default.svc.cluster.local; done"
```

The CPU usage of Nginx will increase to above 40%. After about 30 seconds, 
you should observe an increase in the number of Nginx replicas:

```shell
kubectl get pods
```

The expected output is as follows:

```log
>>> NAME                                READY   STATUS    RESTARTS   AGE
>>> load-generator                      1/1     Running   0          86s
>>> nginx-deployment-5b85cc87b7-gr94j   1/1     Running   0          56s
>>> nginx-deployment-5b85cc87b7-lwqqk   1/1     Running   0          56s
>>> nginx-deployment-5b85cc87b7-q2gmp   1/1     Running   0          4m33s
```

Note: The reactive speed of the default HPA is limited; AIBrix plans to optimize this in future releases.



## Apply Pressure to See the Effect

Use a simple workload generator to increase load:

```shell
kubectl run load-generator --image=busybox -- /bin/sh -c "while true; do wget -q -O- http://nginx-service.default.svc.cluster.local; done"
```

The CPU usage of Nginx will increase to above 40%. After about 30 seconds,
you should observe an increase in the number of Nginx replicas:

```shell
kubectl get pods
```

The expected output is as follows:

```log
>>> NAME                                READY   STATUS    RESTARTS   AGE
>>> load-generator                      1/1     Running   0          86s
>>> nginx-deployment-5b85cc87b7-gr94j   1/1     Running   0          56s
>>> nginx-deployment-5b85cc87b7-lwqqk   1/1     Running   0          56s
>>> nginx-deployment-5b85cc87b7-q2gmp   1/1     Running   0          4m33s
```

Note: The reactive speed of the default HPA is limited; AIBrix plans to optimize this in future releases.


# [WIP] Case 2: Create KPA-based AIBrix PodAutoscaler

Create Nginx App, the default replicas is 1:
```shell
kubectl apply -f config/samples/autoscaling_v1alpha1_demo_nginx.yaml
kubectl get deployments -n default
```
```log
NAME               READY   UP-TO-DATE   AVAILABLE   AGE
nginx-deployment   1/1     1            1           24s
```

Create an autoscaler with type of KPA:
```shell
kubectl apply -f config/samples/autoscaling_v1alpha1_kpa.yaml
```

You can see the kpa scaler has been created:

```shell
kubectl get podautoscalers --all-namespaces
```

```log
>>> NAMESPACE   NAME                    AGE
>>> default     podautoscaler-example-kpa   5m1s
```

**Note that**: Since we pass CPU target value 0 to KPA, it will try to scale the pod down to 0.

You can see logs like `KPA algorithm run...` in `aibrix-controller-manager`:

```shell
kubectl get pods -n aibrix-system -o name | grep aibrix-controller-manager | head -n 1 | xargs -I {} kubectl logs {} -n aibrix-system
```

```log
2024-08-28T03:52:01Z	INFO	Obtained selector and get ReadyPodsCount	{"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "PodAutoscaler": {"name":"podautoscaler-example-kpa","namespace":"default"}, "namespace": "default", "name": "podautoscaler-example-kpa", "reconcileID": "2a811d63-9181-46b5-ac63-a512cb6c4e17", "selector": "app=nginx", "originalReadyPodsCount": 1}
I0828 03:52:01.735190       1 kpa.go:245] "Operating in stable mode."
2024-08-28T03:52:01Z	INFO	Successfully called Scale Algorithm	{"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "PodAutoscaler": {"name":"podautoscaler-example-kpa","namespace":"default"}, "namespace": "default", "name": "podautoscaler-example-kpa", "reconcileID": "2a811d63-9181-46b5-ac63-a512cb6c4e17", "scaleResult": {"DesiredPodCount":0,"ExcessBurstCapacity":98,"ScaleValid":true}}
2024-08-28T03:52:01Z	INFO	Proposing desired replicas	{"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "PodAutoscaler": {"name":"podautoscaler-example-kpa","namespace":"default"}, "namespace": "default", "name": "podautoscaler-example-kpa", "reconcileID": "2a811d63-9181-46b5-ac63-a512cb6c4e17", "desiredReplicas": 0, "metric": "", "timestamp": "2024-08-28T03:52:01Z", "scaleTarget": "Deployment/default/nginx-deployment"}
2024-08-28T03:52:01Z	DEBUG	events	KPA algorithm run. desiredReplicas: 0, currentReplicas: 1	{"type": "Normal", "object": {"kind":"PodAutoscaler","namespace":"default","name":"podautoscaler-example-kpa","uid":"f1dda4ea-4627-47d8-aaa8-a5499ed20698","apiVersion":"autoscaling.aibrix.ai/v1alpha1","resourceVersion":"2348298"}, "reason": "KPAAlgorithmRun"}
2024-08-28T03:52:01Z	INFO	Successfully rescaled	{"controller": "podautoscaler", "controllerGroup": "autoscaling.aibrix.ai", "controllerKind": "PodAutoscaler", "PodAutoscaler": {"name":"podautoscaler-example-kpa","namespace":"default"}, "namespace": "default", "name": "podautoscaler-example-kpa", "reconcileID": "2a811d63-9181-46b5-ac63-a512cb6c4e17", "currentReplicas": 1, "desiredReplicas": 0, "reason": "All metrics below target"}
2024-08-28T03:52:01Z	DEBUG	events	New size: 0; reason: All metrics below target	{"type": "Normal", "object": {"kind":"PodAutoscaler","namespace":"default","name":"podautoscaler-example-kpa","uid":"f1dda4ea-4627-47d8-aaa8-a5499ed20698","apiVersion":"autoscaling.aibrix.ai/v1alpha1","resourceVersion":"2348298"}, "reason": "SuccessfulRescale"}
2024-08-28T03:52:01Z	DEBUG	events	KPA algorithm run. desiredReplicas: 0, currentReplicas: 0	{"type": "Normal", "object": {"kind":"PodAutoscaler","namespace":"default","name":"podautoscaler-example-kpa","uid":"f1dda4ea-4627-47d8-aaa8-a5499ed20698","apiVersion":"autoscaling.aibrix.ai/v1alpha1","resourceVersion":"2348301"}, "reason": "KPAAlgorithmRun"}
2024-08-28T03:58:41Z	DEBUG	events	KPA algorithm run. desiredReplicas: 0, currentReplicas: 0	{"type": "Normal", "object": {"kind":"PodAutoscaler","namespace":"default","name":"podautoscaler-example-kpa","uid":"f1dda4ea-4627-47d8-aaa8-a5499ed20698","apiVersion":"autoscaling.aibrix.ai/v1alpha1","resourceVersion":"2348301"}, "reason": "KPAAlgorithmRun"}
```

Some events from `podautoscaler-example-kpa` are shown below, KPA succeeded to scale down the pods to 0:

```shell
kubectl describe podautoscalers podautoscaler-example-kpa
```
```log
Events:
  Type    Reason             Age    From           Message
  ----    ------             ----   ----           -------
  Normal  KPAAlgorithmRun    2m23s  PodAutoscaler  KPA algorithm run. desiredReplicas: 0, currentReplicas: 1
  Normal  SuccessfulRescale  2m23s  PodAutoscaler  New size: 0; reason: All metrics below target
  Normal  KPAAlgorithmRun    2m23s  PodAutoscaler  KPA algorithm run. desiredReplicas: 0, currentReplicas: 0
```


# Cleanup

To clean up the resources:

```shell
# Remove AIBrix resources
kubectl delete podautoscalers.autoscaling.aibrix.ai podautoscaler-example
kubectl delete podautoscalers.autoscaling.aibrix.ai podautoscaler-example-kpa

make uninstall && make undeploy

# Remove the cascaded HPA
kubectl delete hpa podautoscaler-example-hpa

# Remove the demo Nginx deployment and load generator
kubectl delete deployment nginx-deployment
kubectl delete pod load-generator
```
