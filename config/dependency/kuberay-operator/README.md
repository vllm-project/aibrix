## KubeRay upstream manifests

Commands to export manifest from helm package. After you got manifest, copy to this folder.

```shell
helm template kuberay-operator kuberay/kuberay-operator --namespace aibrix-system --version 1.2.1 --include-crds --set env[0].name=ENABLE_PROBES_INJECTION --set env[0].value=\"false\" --set fullnameOverride=kuberay-operator --set featureGates[0].name=RayClusterStatusConditions --set featureGates[0].enabled=true --output-dir ./config/dependency
```

If you use zsh, please use `noglob helm ...` to skip the brace check.
