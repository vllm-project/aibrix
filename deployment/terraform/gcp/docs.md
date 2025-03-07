# AIBrix GCP Terraform Module
This module deploys an AIBrix cluster in its entirety onto a Google Container Cluster. It is the quickest way to get up and running with AIBrix. The purpose of this module is to both allow developers to quickly spin up the stack, and allow for the team to test on the GCP cloud environment. 

**NOTE: This module was created to allow users to quickly spin up AIBrix on GCP. It is not currently built for production deployments. The user is is responsible for any costs incurred by running this module.**

## Quickstart

### Prerequisites
- [GCloud CLI](https://cloud.google.com/sdk/docs/install) 
- A quota of at least 1 GPU within your GCP project. More information can be found on the topic [here](https://cloud.google.com/compute/resource-usage#gpu_quota).
- [Terraform CLI](https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli)

### Quickstart
1. Run `gcloud auth application-default login` to setup credentials to Google.
2. Install cluster auth plugin with `gcloud components install gke-gcloud-auth-plugin`. 
3. Rename `terraform.tfvars.example` to `terraform.tfvars` and fill in the required variables. You can also add any optional overrides here as well.
4. Run `terraform init` to initialize the module.
5. Run `terraform plan` to see details on the resources created by this module.
6. When you are satisfied with the plan and want to create the resources, run `terraform apply`. NOTE: if you recieve `NodePool aibrix-gpu-nodes was created in the error state "ERROR"` while running the script, check your quotas for GPUs and the specific instances you're trying to deploy.
7. When you are finished testing and no longer want the resources, run `terraform destroy`. **Ensure that you complete this step once you are done trying it out, as GPUs are expensive.**

## Testing
Testing is done completely end to end, no resources need to be initialized beforehand. The testing script will spin up the entire stack, run its tests with the generic OpenAI client against the created resources, and then will destroy everything it has created. Tests are modeled after the E2E tests found in `test/e2e/e2e_test.go`.

1. Ensure you have required Prerequisites from Quickstart above.
2. Complete steps 1-3 in Quickstart steps above. Only fill in the rquired variables within `terraform.tfvars`.
3. Change directory to `/deployment/terraform` and run `go test -v -timeout 60m tests/gcp_test.go`. NOTE: this test takes a while to run as its spinning up its own kubernetes cluster.
4. If the test times out, to ensure resource deletion, simply run the test again.