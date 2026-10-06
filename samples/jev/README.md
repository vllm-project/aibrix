# Jev-compatible decision model

Serves SGLang's `POST /v1/systemone` behind the AIBrix gateway. The route takes a
`state` and a map of `noul`, `choice` and `score` questions keyed by your own ids
and answers each with a probability per option, without generating text. Clients of
the System One API, including the TypeSafe SDKs, work by pointing their base URL at
the gateway.

[`model.yaml`](model.yaml) is adapted from
[`../quickstart/vke/model.yaml`](../quickstart/vke/model.yaml). It deploys one
SGLang pod and a Service, both named `jev-latest`.

## Prerequisites

- AIBrix installed with the gateway enabled. The gateway must include the
  `/v1/systemone` route; see
  [SGLang System One API](../../docs/source/features/multi-engine.rst).
- A GPU node that can hold Qwen3.8-27B in BF16 (about 54 GB of weights). SGLang
  validates this model on a single H200.
- A cluster that can pull the images and reach huggingface.co. If it cannot, see the
  comments on the init container in `model.yaml`.

## Deploy

```bash
kubectl apply -f samples/jev/model.yaml
kubectl wait --for=condition=Available deployment/jev-latest --timeout=30m
```

The first start downloads the weights, loads them and captures CUDA graphs, so the
pod stays unready for several minutes.

## Query

Reach the gateway as in the [quickstart](../../docs/source/getting_started/quickstart.rst),
then send a request. The `model` must be the name the gateway serves, which here is
`jev-latest`:

```bash
curl http://${ENDPOINT}/v1/systemone \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "jev-latest",
    "state": "My Stripe integration keeps failing. I am losing sales. Please help ASAP.",
    "questions": {
      "team": {
        "type": "choice",
        "instructions": "Which team should handle this request?",
        "criteria": {
          "billing": "Charges and refunds",
          "technical_support": "Integration errors",
          "sales": "Questions about buying a product"
        }
      },
      "urgent": {"type": "noul", "instructions": "Does this message express urgency?"}
    }
  }'
```

A `choice` answer carries the most probable option, a `confidence` and a
`probabilities` map; a `noul` answer carries the probability of yes. The `usage` block
reports `input_tokens` and `output_tokens` only; the gateway counts their sum for rate
limits and request metrics.

## Notes

- **Model name.** The gateway answers `400` `model_not_found` for a model that no pod
  advertises. The TypeSafe SDKs default to the model name `jev-latest`, so naming the
  deployment that lets an SDK client work without setting a model. To serve a name of
  your own, rename the Deployment, both `model.aibrix.ai/name` labels,
  `--served-model-name` and the Service.
- **Image and model.** `v0.5.21` is the first SGLang release with `/v1/systemone`. It
  does not include support for decision checkpoints such as pplx-decider, which SGLang
  documents as needing a nightly build, so this sample serves a general chat model
  instead. With an image that has that support, point `--model-uri` and `--model-path`
  at the checkpoint.
- **Engine.** The gateway does not check the engine behind a model, so only route
  `/v1/systemone` to pods labelled `model.aibrix.ai/engine: sglang`, as `model.yaml`
  does.
