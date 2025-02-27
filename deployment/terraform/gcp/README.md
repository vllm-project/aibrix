## Prerequisites
- [GCloud CLI](https://cloud.google.com/sdk/docs/install) 
- [Terraform CLI](https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli)

## Quickstart Steps
1. Run `gcloud auth application-default login` to setup credentials to Google.
2. Run `terraform init` to initialize the module.
3. Rename `terraform.tfvars.example` to `terraform.tfvars` and fill in the required variables. You can also add any optional overrides here as well.
4. Run `terraform plan` to see details on the resources created by this module.
5. When you are satisfied with the plan and want to create the resources, run `terraform apply`.
6. When you are finished testing and no longer want the resources, run `terraform destroy`.