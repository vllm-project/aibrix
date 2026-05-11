from pathlib import Path

import yaml

from aibrix.batch.manifest import DeploymentManifestRenderer, build_downloader_env
from aibrix.batch.template import local_profile_registry, local_template_registry

_TESTDATA_DIR = Path(__file__).parent / "testdata"


def _write_yaml(path: Path, obj) -> Path:
    path.write_text(yaml.safe_dump(obj, sort_keys=False))
    return path


def test_deployment_manifest_renderer_matches_example_with_local_model(tmp_path):
    template_path = _write_yaml(
        tmp_path / "templates.yaml",
        [
            {
                "name": "deepseek-coder-7b",
                "version": "v1",
                "status": "active",
                "spec": {
                    "engine": {
                        "type": "vllm",
                        "version": "0.6.2",
                        "image": "ignored-by-override",
                        "health_endpoint": "/health",
                        "serve_args": ["--port", "8000"],
                    },
                    "model_source": {
                        "type": "local",
                        "uri": "/models/deepseek-coder-6.7b-instruct",
                    },
                    "accelerator": {
                        "type": "NVIDIA-L20",
                        "count": 1,
                    },
                    "parallelism": {
                        "tp": 1,
                    },
                    "supported_endpoints": ["/v1/chat/completions"],
                    "deployment_mode": "dedicated",
                },
            }
        ],
    )
    profile_path = _write_yaml(tmp_path / "profiles.yaml", {"items": []})

    template_registry = local_template_registry(template_path)
    profile_registry = local_profile_registry(profile_path)
    template_registry.reload()
    profile_registry.reload()

    renderer = DeploymentManifestRenderer(template_registry, profile_registry)
    rendered = renderer.render(
        template_name="deepseek-coder-7b",
        deployment_name="deepseek-coder-7b-l20",
        replicas=4,
        gpu_type="NVIDIA-L20",
    )
    expected_deployment = yaml.safe_load(
        (_TESTDATA_DIR / "deployment_example.yaml").read_text()
    )
    expected_service = yaml.safe_load(
        (_TESTDATA_DIR / "deployment_service_example.yaml").read_text()
    )

    assert rendered["deployment"] == expected_deployment
    assert rendered["service"] == expected_service


def test_build_downloader_env_and_remote_init_container(tmp_path):
    template_path = _write_yaml(
        tmp_path / "templates.yaml",
        [
            {
                "name": "deepseek-coder-7b-tos",
                "version": "v1",
                "status": "active",
                "spec": {
                    "engine": {
                        "type": "vllm",
                        "version": "0.6.2",
                        "image": "ignored-by-override",
                        "health_endpoint": "/health",
                        "serve_args": ["--port", "8000"],
                    },
                    "model_source": {
                        "type": "tos",
                        "uri": "tos://aibrix-artifact-testing/models/deepseek-ai/deepseek-coder-6.7b-instruct/",
                    },
                    "accelerator": {
                        "type": "NVIDIA-L20",
                        "count": 1,
                    },
                    "parallelism": {
                        "tp": 1,
                    },
                    "supported_endpoints": ["/v1/chat/completions"],
                    "deployment_mode": "dedicated",
                },
            }
        ],
    )
    profile_path = _write_yaml(
        tmp_path / "profiles.yaml",
        {
            "default": "tos-profile",
            "items": [
                {
                    "name": "tos-profile",
                    "spec": {
                        "storage": {
                            "backend": "tos",
                            "bucket": "aibrix-artifact-testing",
                            "region": "cn-beijing",
                            "endpoint_url": "tos-cn-beijing.ivolces.com",
                            "credentials_secret_ref": "tos-credential",
                        }
                    },
                }
            ],
        },
    )

    template_registry = local_template_registry(template_path)
    profile_registry = local_profile_registry(profile_path)
    template_registry.reload()
    profile_registry.reload()

    template = template_registry.get("deepseek-coder-7b-tos")
    profile = profile_registry.get("tos-profile")
    assert template is not None
    assert profile is not None

    assert build_downloader_env(template, profile) == [
        {
            "name": "DOWNLOADER_MODEL_NAME",
            "value": "deepseek-coder-6.7b-instruct",
        },
        {
            "name": "DOWNLOADER_ALLOW_FILE_SUFFIX",
            "value": "json, safetensors",
        },
        {
            "name": "DOWNLOADER_TOS_VERSION",
            "value": "v2",
        },
        {
            "name": "TOS_ACCESS_KEY",
            "valueFrom": {
                "secretKeyRef": {
                    "name": "tos-credential",
                    "key": "access-key",
                }
            },
        },
        {
            "name": "TOS_SECRET_KEY",
            "valueFrom": {
                "secretKeyRef": {
                    "name": "tos-credential",
                    "key": "secret-key",
                }
            },
        },
        {
            "name": "TOS_ENDPOINT",
            "value": "tos-cn-beijing.ivolces.com",
        },
        {
            "name": "TOS_REGION",
            "value": "cn-beijing",
        },
    ]

    renderer = DeploymentManifestRenderer(template_registry, profile_registry)
    rendered = renderer.render(
        template_name="deepseek-coder-7b-tos",
        deployment_name="deepseek-coder-7b-tos",
    )

    deployment = rendered["deployment"]
    init_container = deployment["spec"]["template"]["spec"]["initContainers"][0]
    engine_container = deployment["spec"]["template"]["spec"]["containers"][0]

    assert init_container["command"] == [
        "aibrix_download",
        "--model-uri",
        "tos://aibrix-artifact-testing/models/deepseek-ai/deepseek-coder-6.7b-instruct/",
        "--local-dir",
        "/models/",
    ]
    assert init_container["env"][0]["value"] == "deepseek-coder-6.7b-instruct"
    assert engine_container["args"][0:2] == [
        "--model",
        "/models/deepseek-coder-6.7b-instruct",
    ]
    assert {
        "name": "model-hostpath",
        "mountPath": "/models",
    } in engine_container["volumeMounts"]
    assert {
        "name": "model-hostpath",
        "hostPath": {
            "path": "/root/models",
            "type": "DirectoryOrCreate",
        },
    } in deployment["spec"]["template"]["spec"]["volumes"]
