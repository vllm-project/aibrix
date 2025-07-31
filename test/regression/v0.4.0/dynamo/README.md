# Dynamo Installation Instruction


We follow the instruction in [dynamo](https://github.com/ai-dynamo/dynamo) to deploy the Dynamo Cloud in Kubernetes. The detailed instrunction can be found in Section 1.  `1. Installing Dynamo Cloud from Published Artifacts` from dynamo's [quickstart guide](https://github.com/ai-dynamo/dynamo/blob/main/docs/guides/dynamo_deploy/quickstart.md). We use the most recent release images (version: 0.3.2) published by Dynamo team.


### Model Deployment

We use sample deployment yamls from the dynamo repo in the v0.3.2 release for PD disaggration testing. https://github.com/ai-dynamo/dynamo/blob/v0.3.2/examples/llm/deploy/agg.yaml and https://github.com/ai-dynamo/dynamo/blob/v0.3.2/examples/llm/deploy/agg-router.yaml.


> Note: There are some configuration changes in terms of image downloading and model downloading due to the testing environment difference.

> 1. We download container image from VKE docker registry aibrix-cn-beijing.cr.volces.com. The images are synced from dockerhub and nvidia ngc.
> 2. We download model from VKE object storage, which are synced from Huggingface model hub.
