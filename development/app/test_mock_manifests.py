from pathlib import Path

import yaml


APP_DIR = Path(__file__).parent
EXPECTED_WORKLOADS = {
    "vllm-pd-config.yaml": "mock-llama2-7b-pd-vllm",
    "sglang-pd-config.yaml": "mock-llama2-7b-pd-sglang",
    "trtllm-pd-config.yaml": "mock-llama2-7b-pd-trtllm",
    "nixl-pd-config.yaml": "mock-llama2-7b-pd-vllm-nixl",
}


def load_yaml(path):
    with path.open() as stream:
        return list(yaml.safe_load_all(stream))


def test_pd_role_pod_templates_have_workload_identity_and_app_label():
    for filename, workload in EXPECTED_WORKLOADS.items():
        document = load_yaml(APP_DIR / "config" / "mock" / filename)[0]
        roles = document["spec"]["template"]["spec"]["roles"]

        assert {role["name"] for role in roles} == {"prefill", "decode"}
        for role in roles:
            labels = role["template"]["metadata"]["labels"]
            assert labels["app.kubernetes.io/name"] == workload
            assert labels["app"] == workload
            env = role["template"]["spec"]["containers"][0]["env"]
            deployment_name = next(item for item in env if item["name"] == "DEPLOYMENT_NAME")
            assert deployment_name["valueFrom"]["fieldRef"]["fieldPath"] == "metadata.labels['app']"


def test_config_profile_pod_template_has_model_port_label():
    documents = load_yaml(APP_DIR / "config" / "mock" / "config-profile.yaml")
    deployment = next(document for document in documents if document["kind"] == "Deployment")

    assert deployment["spec"]["template"]["metadata"]["labels"]["model.aibrix.ai/port"] == "8000"


def test_bucketing_role_pod_templates_have_workload_identity_labels():
    documents = load_yaml(APP_DIR / "config" / "mock" / "vllm-pd-bucketing-config.yaml")
    expected = {
        "mock-llama2-pd-bkt-sht": {"prefill", "decode"},
        "mock-llama2-pd-bkt-med": {"prefill", "decode"},
        "mock-llama2-pd-bkt-comb": {"all"},
    }

    for document in documents:
        workload = document["metadata"]["name"]
        roles = document["spec"]["template"]["spec"]["roles"]
        assert {role["name"] for role in roles} == expected[workload]
        for role in roles:
            labels = role["template"]["metadata"]["labels"]
            assert labels["app"] == workload
            assert labels["app.kubernetes.io/name"] == workload
