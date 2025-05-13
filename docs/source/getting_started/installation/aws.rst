.. _aws:

=========================
Amazon Web Services (AWS)
=========================

Introduction
------------

This module deploys an AIBrix cluster in its entirety onto AWS EKS Cluster. It is the quickest way to get up and running with AIBrix.

Quickstart
----------

Prerequisites
~~~~~~~~~~~~~

- `eksctl <https://eksctl.io/installation/>`_
- A quota of at least 1 GPU within your AWS project.

Steps
~~~~~

1. Create an eks cluster: ``eksctl create cluster --name aibrix --node-type=g5.4xlarge --nodes 2 --auto-kubeconfig``.

    .. code-block:: console

        eksctl create cluster --name aibrix --node-type=g5.4xlarge --nodes 2 --auto-kubeconfig
        2025-04-28 21:47:55 [ℹ]  eksctl version 0.187.0-dev+707c73b66.2024-07-16T06:38:53Z
        2025-04-28 21:47:55 [ℹ]  using region us-west-2
        2025-04-28 21:47:55 [ℹ]  skipping us-west-2d from selection because it doesn't support the following instance type(s): g5.4xlarge
        2025-04-28 21:47:55 [ℹ]  setting availability zones to [us-west-2a us-west-2c us-west-2b]
        2025-04-28 21:47:55 [ℹ]  subnets for us-west-2a - public:192.168.0.0/19 private:192.168.96.0/19
        2025-04-28 21:47:55 [ℹ]  subnets for us-west-2c - public:192.168.32.0/19 private:192.168.128.0/19
        2025-04-28 21:47:55 [ℹ]  subnets for us-west-2b - public:192.168.64.0/19 private:192.168.160.0/19
        2025-04-28 21:47:55 [ℹ]  nodegroup "ng-fc753bf9" will use "" [AmazonLinux2/1.30]
        2025-04-28 21:47:55 [ℹ]  using Kubernetes version 1.30
        2025-04-28 21:47:55 [ℹ]  creating EKS cluster "aibrix" in "us-west-2" region with managed nodes
        2025-04-28 21:47:55 [ℹ]  will create 2 separate CloudFormation stacks for cluster itself and the initial managed nodegroup
        2025-04-28 21:47:55 [ℹ]  if you encounter any issues, check CloudFormation console or try 'eksctl utils describe-stacks --region=us-west-2 --cluster=aibrix'
        2025-04-28 21:47:55 [ℹ]  Kubernetes API endpoint access will use default of {publicAccess=true, privateAccess=false} for cluster "aibrix" in "us-west-2"
        2025-04-28 21:47:55 [ℹ]  CloudWatch logging will not be enabled for cluster "aibrix" in "us-west-2"
        2025-04-28 21:47:55 [ℹ]  you can enable it with 'eksctl utils update-cluster-logging --enable-types={SPECIFY-YOUR-LOG-TYPES-HERE (e.g. all)} --region=us-west-2 --cluster=aibrix'
        2025-04-28 21:47:55 [ℹ]  default addons vpc-cni, kube-proxy, coredns were not specified, will install them as EKS addons
        2025-04-28 21:47:55 [ℹ]
        2 sequential tasks: { create cluster control plane "aibrix",
            2 sequential sub-tasks: {
                2 sequential sub-tasks: {
                    1 task: { create addons },
                    wait for control plane to become ready,
                },
                create managed nodegroup "ng-fc753bf9",
            }
        }
        2025-04-28 21:47:55 [ℹ]  building cluster stack "eksctl-aibrix-cluster"
        2025-04-28 21:47:56 [ℹ]  deploying stack "eksctl-aibrix-cluster"
        2025-04-28 21:48:26 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:48:56 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:49:56 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:50:56 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:51:56 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:52:56 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:53:57 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:54:57 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:55:57 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-cluster"
        2025-04-28 21:55:59 [!]  recommended policies were found for "vpc-cni" addon, but since OIDC is disabled on the cluster, eksctl cannot configure the requested permissions; the recommended way to provide IAM permissions for "vpc-cni" addon is via pod identity associations; after addon creation is completed, add all recommended policies to the config file, under `addon.PodIdentityAssociations`, and run `eksctl update addon`
        2025-04-28 21:55:59 [ℹ]  creating addon
        2025-04-28 21:55:59 [ℹ]  successfully created addon
        2025-04-28 21:55:59 [ℹ]  creating addon
        2025-04-28 21:56:00 [ℹ]  successfully created addon
        2025-04-28 21:56:00 [ℹ]  creating addon
        2025-04-28 21:56:00 [ℹ]  successfully created addon
        2025-04-28 21:58:01 [ℹ]  building managed nodegroup stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 21:58:01 [ℹ]  deploying stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 21:58:02 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 21:58:32 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 21:59:15 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 21:59:51 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 22:01:51 [ℹ]  waiting for CloudFormation stack "eksctl-aibrix-nodegroup-ng-fc753bf9"
        2025-04-28 22:01:51 [ℹ]  waiting for the control plane to become ready
        2025-04-28 22:01:52 [✔]  saved kubeconfig as "/Users/bytedance/.kube/eksctl/clusters/aibrix"
        2025-04-28 22:01:52 [ℹ]  1 task: { install Nvidia device plugin }
        W0428 22:01:52.922061   12610 warnings.go:70] spec.template.metadata.annotations[scheduler.alpha.kubernetes.io/critical-pod]: non-functional in v1.16+; use the "priorityClassName" field instead
        2025-04-28 22:01:52 [ℹ]  created "kube-system:DaemonSet.apps/nvidia-device-plugin-daemonset"
        2025-04-28 22:01:52 [ℹ]  as you are using the EKS-Optimized Accelerated AMI with a GPU-enabled instance type, the Nvidia Kubernetes device plugin was automatically installed.
            to skip installing it, use --install-nvidia-plugin=false.
        2025-04-28 22:01:52 [✔]  all EKS cluster resources for "aibrix" have been created
        2025-04-28 22:01:52 [✔]  created 0 nodegroup(s) in cluster "aibrix"
        2025-04-28 22:01:53 [ℹ]  nodegroup "ng-fc753bf9" has 2 node(s)
        2025-04-28 22:01:53 [ℹ]  node "ip-192-168-24-13.us-west-2.compute.internal" is ready
        2025-04-28 22:01:53 [ℹ]  node "ip-192-168-49-240.us-west-2.compute.internal" is ready
        2025-04-28 22:01:53 [ℹ]  waiting for at least 2 node(s) to become ready in "ng-fc753bf9"
        2025-04-28 22:01:53 [ℹ]  nodegroup "ng-fc753bf9" has 2 node(s)
        2025-04-28 22:01:53 [ℹ]  node "ip-192-168-24-13.us-west-2.compute.internal" is ready
        2025-04-28 22:01:53 [ℹ]  node "ip-192-168-49-240.us-west-2.compute.internal" is ready
        2025-04-28 22:01:53 [✔]  created 1 managed nodegroup(s) in cluster "aibrix"
        2025-04-28 22:01:54 [ℹ]  kubectl command should work with "/Users/user/.kube/eksctl/clusters/aibrix", try 'kubectl --kubeconfig=/Users/user/.kube/eksctl/clusters/aibrix get nodes'
        2025-04-28 22:01:54 [✔]  EKS cluster "aibrix" in "us-west-2" region is ready


2. Clone AIBrix code repo ``git clone https://github.com/vllm-project/aibrix.git``.
3. Install AIBrix ``kubectl apply -k config/dependency --server-side`` and ``kubectl apply -k config/default``.
4. Wait for components to complete running.
5. Deploy a model by following the instructions in :doc:`../quickstart`.
6. Once the model is ready and running, you can test it by running:

    .. code-block:: bash

        LB_IP=$(kubectl get svc/envoy-aibrix-system-aibrix-eg-903790dc -n envoy-gateway-system -o=jsonpath='{.status.loadBalancer.ingress[0].hostname}')
        ENDPOINT="${LB_IP}:80"

        curl http://${ENDPOINT}/v1/chat/completions \
          -H "Content-Type: application/json" \
          -d '{
              "model": "deepseek-r1-distill-llama-8b",
              "messages": [
                  {"role": "system", "content": "You are a helpful assistant."},
                  {"role": "user", "content": "help me write a random generator in python"}
              ]
          }'

7. When you are finished testing and no longer want the resources, run ``eksctl delete cluster --name aibrix``.
