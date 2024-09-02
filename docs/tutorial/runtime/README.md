# AIBrix Runtime Demo

## Model Download
AIBrix runtime support to download model from different sources.

- Download model from HuggingFace
```shell
kubectl apply -f runtime-hf-download.yaml
```

- Download model from AWS S3
```shell
kubectl apply -f runtime-s3-download.yaml
```

- Download model from TOS
```shell
kubectl apply -f runtime-tos-download.yaml
```

## Metrics Merge
