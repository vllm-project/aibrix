.. _gcp:

===========================
Google Cloud Platform (GCP)
===========================

Introduction
------------

This module deploys an AIBrix cluster in its entirety onto a Google Container Cluster. It is the quickest way to get up and running with AIBrix. The purpose of this module is to both allow developers to quickly spin up the stack, and allow for the team to test on the Google Cloud Platform.

.. warning::
   1. This module was created to allow users to quickly spin up AIBrix on GCP. It is not currently built for production deployments. The user is responsible for any costs incurred by running this module.
   2. This module use terraform as the infrastructure as code tool. If you are looking for other means, feel free to cut an `issue <https://github.com/vllm-project/aibrix/issues>`_.

Quickstart
----------

Prerequisites
~~~~~~~~~~~~~

- `GCloud CLI <https://cloud.google.com/sdk/docs/install>`_
- A quota of at least 1 GPU within your GCP project. More information can be found on the topic `here <https://cloud.google.com/compute/resource-usage#gpu_quota>`_.
- `Terraform CLI <https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli>`_

Steps
~~~~~

1. Change directory to the module location: ``cd /deployment/terraform/gcp``
2. Run ``gcloud auth application-default login`` to setup credentials to Google.
3. Install cluster auth plugin with ``gcloud components install gke-gcloud-auth-plugin``.
4. Rename ``terraform.tfvars.example`` to ``terraform.tfvars`` and fill in the required variables. You can also add any optional overrides here as well.
5. Run ``terraform init`` to initialize the module.
6. Run ``terraform plan`` to see details on the resources created by this module.
7. When you are satisfied with the plan and want to create the resources, run ``terraform apply``. 

   .. note::
      If you receive ``NodePool aibrix-gpu-nodes was created in the error state "ERROR"`` while running the script, check your quotas for GPUs and the specific instances you're trying to deploy.

8. Wait for module to complete running. It will output a command to receive the kubernetes config file and a public IP address.
9. Run a command against the public IP:

   .. code-block:: bash

      ENDPOINT="<YOUR PUBLIC IP>"

      curl http://${ENDPOINT}/v1/chat/completions \
          -H "Content-Type: application/json" \
          -d '{
              "model": "deepseek-r1-distill-llama-8b",
              "messages": [
                  {"role": "system", "content": "You are a helpful assistant."},
                  {"role": "user", "content": "help me write a random generator in python"}
              ]
          }'

10. When you are finished testing and no longer want the resources, run ``terraform destroy``. 

.. warning::
  Ensure that you complete this step once you are done trying it out, as GPUs are expensive.

Inputs
------

.. list-table::
   :header-rows: 1
   :widths: 20 50 10 10 10

   * - Name
     - Description
     - Type
     - Default
     - Required
   * - aibrix_release_version
     - The version of AIBrix to deploy.
     - string
     - "v0.2.0"
     - no
   * - cluster_name
     - Name of the GKE cluster.
     - string
     - "aibrix-inference-cluster"
     - no
   * - cluster_zone
     - Zone to deploy cluster within. If not provided will be deployed to default region.
     - string
     - ""
     - no
   * - default_region
     - Default region to deploy resources within.
     - string
     - n/a
     - yes
   * - deploy_example_model
     - Whether to deploy the example model.
     - bool
     - true
     - no
   * - node_pool_machine_count
     - Machine count for the node pool.
     - number
     - 1
     - no
   * - node_pool_machine_type
     - Machine type for the node pool. Must be in the A3, A2, or G2 series.
     - string
     - "g2-standard-4"
     - no
   * - node_pool_name
     - Name of the GPU node pool.
     - string
     - "aibrix-gpu-nodes"
     - no
   * - node_pool_zone
     - Zone to deploy GPU node pool within. If not provided will be deployed to zone in default region which has capacity for machine type.
     - string
     - ""
     - no
   * - project_id
     - GCP project to deploy resources within.
     - string
     - n/a
     - yes

Outputs
-------

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Name
     - Description
   * - aibrix_service_public_ip
     - Public IP address for AIBrix service.
   * - configure_kubectl_command
     - Command to run which will allow kubectl access.

Modules
-------

.. list-table::
   :header-rows: 1
   :widths: 20 40 40

   * - Name
     - Source
     - Version
   * - aibrix
     - deployment/terraform/kubernetes
     - n/a
   * - cluster
     - deployment/terraform/gcp/cluster
     - n/a

Providers
---------

.. list-table::
   :header-rows: 1
   :widths: 30 70

   * - Name
     - Version
   * - google
     - 6.22.0
   * - kubernetes
     - 2.36.0
