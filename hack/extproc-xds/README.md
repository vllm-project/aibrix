# Generating the ext_proc xDS golden

`pkg/plugins/gateway/testdata/envoy_gateway_v1.2.8_xds.json` is the Envoy
configuration that Envoy Gateway v1.2.8 -- the version pinned by
`config/dependency/envoy-gateway/kustomization.yaml` -- produces from
`config/gateway`. The Videos API's response handling depends on the ext_proc
`processing_mode` and on route precedence, and neither is visible in the Gateway
API manifest: they are the translator's output. The golden is that output, so
`gateway_extproc_config_test.go` can assert on generated configuration instead of
on the input it was generated from.

Regenerate after any change to the routes or the EnvoyExtensionPolicies:

```sh
# 1. Render what is actually deployed (config/default's namePrefix + namespace).
kustomize build hack/extproc-xds > /tmp/rendered.yaml

# 2. egctl cannot translate an EnvoyExtensionPolicy as shipped: its translator
#    fails with "wasm cache is not initialized" for every policy, wasm or not
#    (internal/gatewayapi/envoyextensionpolicy.go, buildWasms). Build egctl from
#    the v1.2.8 source with a no-op wasm cache; nothing else is changed, and the
#    ext_proc translation is untouched by it.
curl -sSLO https://github.com/envoyproxy/gateway/archive/refs/tags/v1.2.8.tar.gz
tar xzf v1.2.8.tar.gz && cd gateway-1.2.8
cat > internal/cmd/egctl/wasmstub.go <<'GO'
package egctl

import (
	"context"

	"github.com/envoyproxy/gateway/internal/wasm"
)

type noopWasmCache struct{}

func (noopWasmCache) Get(downloadURL string, opts wasm.GetOptions) (string, string, error) {
	return downloadURL, "", nil
}
func (noopWasmCache) Start(ctx context.Context) {}
GO
# add `WasmCache: noopWasmCache{},` to every gatewayapi.Translator literal in
# internal/cmd/egctl/translate.go, then:
go build -o /tmp/egctl ./cmd/egctl

# 3. Translate. --add-missing-resources fills in the EndpointSlices of the
#    backends, which the translation of ext_proc does not depend on.
/tmp/egctl x translate --from gateway-api --to xds -t all -o json \
  --add-missing-resources -f /tmp/rendered.yaml > /tmp/xds.json

# 4. Keep only the listener and route dumps, indented and key-sorted:
python3 - <<'PY'
import json
d = json.load(open('/tmp/xds.json'))
cfgs = d['xds']['aibrix-system/aibrix-eg']['configs']
keep = [c for c in cfgs if c['@type'].endswith(('ListenersConfigDump', 'RoutesConfigDump'))]
out = {'xds': {'aibrix-system/aibrix-eg': {'configs': keep}}}
open('pkg/plugins/gateway/testdata/envoy_gateway_v1.2.8_xds.json', 'w').write(
    json.dumps(out, indent=2, sort_keys=True) + "\n")
PY
```

The test cross-checks the golden against `config/gateway/gateway-plugin/gateway-plugin.yaml`,
so a manifest change that is not regenerated here fails the build rather than
silently leaving the golden describing configuration nobody deploys.

Two things about the golden that are expected rather than broken:

* The `aibrix-reserved-router-metadata-endpoint` routes translate to a
  `direct_response` 500, because the Service they reference lives in
  `config/metadata`, outside the overlay. Those routes carry no ext_proc filter
  either way, so nothing the tests assert depends on them.
* The routes named `original_route*` come from the `EnvoyPatchPolicy` in
  `config/gateway/gateway.yaml`, not from any HTTPRoute. They are the pinning
  routes every request re-matches once the plugin has set `routing-strategy`,
  and they are what decides which ext_proc filter handles the response.
