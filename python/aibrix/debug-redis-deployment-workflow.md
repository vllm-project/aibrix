[OPEN] redis-deployment-workflow

- Symptom: `test_openai_batch_api_metadata_server_workflow_with_redis_cache_and_deployment_driver` does not observe Deployment creation as expected.
- Goal: determine whether scheduler-side driver selection, deployment-driver execution, or test interception is failing.
- Hypotheses:
  - scheduler does not select `DeploymentDriver`
  - deployment creation fails before Kubernetes create calls
  - test patches observe different API instances than runtime uses
  - Redis-backed state flow skips deployment execution path
  - scheduler/factory inference-client integration regressed local-vs-deployment selection
