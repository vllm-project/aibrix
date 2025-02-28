## Prerequisites
- [GCloud CLI](https://cloud.google.com/sdk/docs/install) 
- [Terraform CLI](https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli)

## Quickstart Steps
1. Run `gcloud auth application-default login` to setup credentials to Google.
2. Install cluster auth plugin with `gcloud components install gke-gcloud-auth-plugin`.
3. Run `terraform init` to initialize the module.
4. Rename `terraform.tfvars.example` to `terraform.tfvars` and fill in the required variables. You can also add any optional overrides here as well.
5. Run `terraform plan` to see details on the resources created by this module.
6. When you are satisfied with the plan and want to create the resources, run `terraform apply`. NOTE: if you recieve `NodePool aibrix-gpu-nodes was created in the error state "ERROR"` while running the script, check your quotas for GPUs and the specific instances you're trying to deploy.
7. When you are finished testing and no longer want the resources, run `terraform destroy`.