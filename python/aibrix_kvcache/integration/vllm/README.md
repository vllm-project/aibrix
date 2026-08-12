# AIBrix KVCache Integration for vLLM

## Deprecation Notice

The patch-based integration under `python/aibrix_kvcache/integration/vllm/patches` is **no longer recommended** for building images.

- Those patch files are **not actively maintained**.
- New users should **not** patch vLLM source code during image build.

## Recommended Integration (Monkey-Patch Based)

Use the integration provided by `python/aibrix_kvcache/aibrix_kvcache/integration`.

You only need to:
1. Install `vllm`
2. Install `aibrix-kvcache`
3. Start vLLM with proper KV connector arguments

No explicit source patching of vLLM is required.

## vLLM Connector Configuration Change

Compared with the old patch workflow, KV connector configuration now requires specifying a module path from AIBrix KVCache.

Example:

```json
{
  "kv_connector": "AIBrixPDReuseConnector",
  "kv_role": "kv_both",
  "kv_connector_module_path": "aibrix_kvcache.integration.vllm.kv_connector.aibrix_pd_reuse_connector"
}
```

`kv_connector_module_path` must point to the KV connector module provided by AIBrix KVCache.
