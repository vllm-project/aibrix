# AIBrix PodAutoscaler Demo

This demonstration will showcase how the AIBrix PodAutoscaler (abbreviated as AIBrix-pa) dynamically 
adjusts the number of replicas for an Nginx service based on CPU utilization.

## Build and Install AIBrix CRDs

First, build and install the Custom Resource Definitions (CRDs) for AIBrix:

```shell
cd $AIBrix_HOME

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
## Create the Demo App and AIBrix PodAutoscaler

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


## Cleanup

To clean up the resources:

```shell
# Remove AIBrix resources
kubectl delete podautoscalers.autoscaling.aibrix.ai podautoscaler-example

make uninstall && make undeploy

# Remove the cascaded HPA
kubectl delete hpa podautoscaler-example-hpa

# Remove the demo Nginx deployment and load generator
kubectl delete deployment nginx-deployment
kubectl delete pod load-generator
```
