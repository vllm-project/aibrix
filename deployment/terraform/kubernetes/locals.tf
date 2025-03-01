locals {
  # AIBrix dependency YAML from http retrieval
  aibrix_dependency_yaml = { for index, resource in provider::kubernetes::manifest_decode_multi(data.http.aibrix_depenency.response_body) : "${resource.kind}/${resource.metadata.name}" => yamlencode(resource) }

  # AIBrix core YAML from http retrieval
  aibrix_core_yaml = { for index, resource in provider::kubernetes::manifest_decode_multi(data.http.aibrix_core.response_body) : "${resource.kind}/${resource.metadata.name}" => yamlencode(resource) }

  # Model YAML from local repository
  model_yaml = { for index, resource in provider::kubernetes::manifest_decode_multi(data.http.model.response_body) : "${resource.kind}" => yamlencode(resource) }
}