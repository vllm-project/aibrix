# Generating the ext_proc xDS golden

`pkg/plugins/gateway/testdata/envoy_gateway_v1.9.4_xds.json` records the listener
and route configuration produced by the pinned Envoy Gateway v1.9.4 from
`config/gateway`. The Videos API depends on response buffering and route
precedence both before and after the plugin clears the route cache. The golden
lets the tests check those properties against translator output.

Use the unmodified `egctl` binary from the v1.9.4 GitHub release. Unlike the old
v1.2.8 procedure, these manifests translate without a custom wasm-cache stub.
Do not use a translator from a different release to regenerate this golden.

```sh
kustomize build hack/extproc-xds > /tmp/rendered.yaml
egctl x translate --from gateway-api --to xds -t all -o json \
  --add-missing-resources -f /tmp/rendered.yaml > /tmp/xds.json
python3 - <<'PY'
import json
from pathlib import Path
d = json.loads(Path('/tmp/xds.json').read_text())
configs = d['xds']['aibrix-system/aibrix-eg']['configs']
keep = [c for c in configs if c['@type'].endswith(('ListenersConfigDump', 'RoutesConfigDump'))]
out = {'xds': {'aibrix-system/aibrix-eg': {'configs': keep}}}
Path('pkg/plugins/gateway/testdata/envoy_gateway_v1.9.4_xds.json').write_text(
    json.dumps(out, indent=2, sort_keys=True) + '\n')
PY
go test ./pkg/plugins/gateway -run 'TestGatewayPluginManifest|TestGeneratedXDS'
```

`--add-missing-resources` supplies backend EndpointSlices for offline translation;
it does not replace live installation and traffic tests. The overlay applies the
same namespace and name prefix as `config/default`.

## Changes from the v1.2.8 golden

- v1.9.4 adds a separate proxy-readiness listener on port 19003 with the `/ready`
  health-check filter. This is not the application listener.
- The application listener sets `maxConnectionsToAcceptPerSocketEvent: 1`.
- The three disabled-by-default, route-scoped ext_proc filters are reordered.
  Their names, processing modes and per-route enablement are unchanged.
- Equal-length, disjoint path-prefix routes can be reordered. This does not
  change a matching result; the tests still check overlapping Videos paths and
  pinning-route precedence in both routing phases.

- With `--add-missing-resources`, v1.9.4 synthesizes a backend for the metadata
  Service missing from this overlay. Its three routes now point at the generated
  metadata cluster instead of returning the old offline `direct_response: 500`.
  They still bypass ext_proc. A live install must verify the real metadata
  Service rather than treat this synthetic offline backend as evidence.

The `original_route*` routes come from `EnvoyPatchPolicy`, preserve the correct
Videos/shared filter choice, and send pinned requests to the `ORIGINAL_DST`
cluster. The listener's `preserve_external_request_id: false` policy stays
unchanged (proto JSON omits that default). Review the full generated diff when
regenerating, not only test output.
