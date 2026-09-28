# External replica router sample

This sample demonstrates post-discovery replica selection. It is distinct from
semantic routing: the request model is already fixed, and the service can select
only a candidate supplied by Gateway.

The default Advisory rule chooses the lexicographically first eligible candidate,
preferring the configured zone and requiring an H100 label for ``premium-model``.
It returns ``NoDecision`` when no premium candidate matches, allowing the explicit
``least-request`` fallback.

## Build and install

~~~bash
docker build -t example.com/aibrix/external-replica-router:v1alpha1 \
  samples/external-replica-router/decision-service
docker push example.com/aibrix/external-replica-router:v1alpha1
kubectl apply -k samples/external-replica-router
kubectl patch deployment aibrix-gateway-plugins -n aibrix-system \
  --type strategic \
  --patch-file samples/external-replica-router/manifests/gateway-plugin-patch.yaml
~~~

Send a request with header ``routing-strategy: external`` and inspect the
``target-pod`` response header.

## Add a custom case

To route ``premium-model`` to H100 replicas in zone ``cn-east-1a``:

1. Label model pods with
   ``routing.example.com/accelerator-class=h100`` and
   ``topology.kubernetes.io/zone=cn-east-1a``.
2. Add exactly those keys to
   ``AIBRIX_EXTERNAL_ROUTER_CANDIDATE_ATTRIBUTES``. Labels outside the allowlist
   never leave Gateway.
3. Update the named predicate in ``decide``; read only model and
   candidate.attributes.
4. Add table cases for a match, no match, missing attributes, and deterministic
   tie-breaking.
5. Rebuild/push the image and update the Deployment image.
6. Apply the Kustomization and wait for both model and decision service.
7. Send an OpenAI-compatible request with ``routing-strategy: external``.
8. Return ``NoDecision`` to exercise Advisory fallback. Use ``Denied`` only in a
   separately tested Authoritative+FailClosed deployment.
9. Inspect Gateway external-router metrics and logs.

Trusted policy attributes are deployment-specific. Populate them only from
validated in-process identity or configuration; never copy raw tenant,
authorization, or arbitrary client headers.

## Validate

~~~bash
python samples/external-replica-router/decision-service/test_router.py
kubectl kustomize samples/external-replica-router >/dev/null
kubectl apply --dry-run=client -k samples/external-replica-router
kubectl patch deployment aibrix-gateway-plugins -n aibrix-system \
  --type strategic --dry-run=client \
  --patch-file samples/external-replica-router/manifests/gateway-plugin-patch.yaml
~~~

This service is educational and not a production HA policy store.
