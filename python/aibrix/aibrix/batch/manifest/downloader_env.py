from typing import Any, Dict, List

from aibrix.batch.template import BatchProfile, ModelDeploymentTemplate
from aibrix.downloader.utils import infer_model_name

_DEFAULT_ALLOW_FILE_SUFFIX = "json, safetensors"
_DEFAULT_TOS_VERSION = "v2"


def build_downloader_env(
    template: ModelDeploymentTemplate, profile: BatchProfile
) -> List[Dict[str, Any]]:
    model_uri = template.spec.model_source.uri
    source_type = template.spec.model_source.type.value
    env = [
        {
            "name": "DOWNLOADER_MODEL_NAME",
            "value": infer_model_name(model_uri),
        },
        {
            "name": "DOWNLOADER_ALLOW_FILE_SUFFIX",
            "value": _DEFAULT_ALLOW_FILE_SUFFIX,
        },
    ]

    if _is_tos_uri(model_uri, source_type):
        return env + _tos_env(profile)
    if _is_s3_uri(model_uri, source_type):
        return env + _s3_env(profile)
    return env + _huggingface_env(template)


def _huggingface_env(template: ModelDeploymentTemplate) -> List[Dict[str, Any]]:
    env: List[Dict[str, Any]] = []
    if template.spec.model_source.auth_secret_ref:
        env.append(
            _secret_env("HF_TOKEN", template.spec.model_source.auth_secret_ref, "token")
        )
    if template.spec.model_source.revision:
        env.append(
            {
                "name": "HF_REVISION",
                "value": template.spec.model_source.revision,
            }
        )
    return env


def _s3_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    env: List[Dict[str, Any]] = []
    secret_ref = profile.spec.storage.credentials_secret_ref
    if secret_ref:
        env.extend(
            [
                _secret_env("AWS_ACCESS_KEY_ID", secret_ref, "access-key-id"),
                _secret_env("AWS_SECRET_ACCESS_KEY", secret_ref, "secret-access-key"),
            ]
        )
    if profile.spec.storage.endpoint_url:
        env.append(
            {
                "name": "AWS_ENDPOINT_URL",
                "value": profile.spec.storage.endpoint_url,
            }
        )
    if profile.spec.storage.region:
        env.append(
            {
                "name": "AWS_REGION",
                "value": profile.spec.storage.region,
            }
        )
    return env


def _tos_env(profile: BatchProfile) -> List[Dict[str, Any]]:
    env: List[Dict[str, Any]] = [
        {
            "name": "DOWNLOADER_TOS_VERSION",
            "value": _DEFAULT_TOS_VERSION,
        }
    ]
    secret_ref = profile.spec.storage.credentials_secret_ref
    if secret_ref:
        env.extend(
            [
                _secret_env("TOS_ACCESS_KEY", secret_ref, "access-key"),
                _secret_env("TOS_SECRET_KEY", secret_ref, "secret-key"),
            ]
        )
    if profile.spec.storage.endpoint_url:
        env.append(
            {
                "name": "TOS_ENDPOINT",
                "value": profile.spec.storage.endpoint_url,
            }
        )
    elif secret_ref:
        env.append(_secret_env("TOS_ENDPOINT", secret_ref, "endpoint"))
    if profile.spec.storage.region:
        env.append(
            {
                "name": "TOS_REGION",
                "value": profile.spec.storage.region,
            }
        )
    elif secret_ref:
        env.append(_secret_env("TOS_REGION", secret_ref, "region"))
    return env


def _secret_env(name: str, secret_ref: str, key: str) -> Dict[str, Any]:
    return {
        "name": name,
        "valueFrom": {
            "secretKeyRef": {
                "name": secret_ref,
                "key": key,
            }
        },
    }


def _is_s3_uri(uri: str, source_type: str) -> bool:
    return uri.startswith("s3://") or source_type == "s3"


def _is_tos_uri(uri: str, source_type: str) -> bool:
    return uri.startswith("tos://") or source_type == "tos"
