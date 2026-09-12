import base64
import importlib.util
import json
import sys
import threading
import time
from pathlib import Path

import pytest
from mock_recorder import RequestRecorder


APP_DIR = Path(__file__).parent
APP_PATH = APP_DIR / "app.py"


def load_mock_module(monkeypatch, *, contract="", role="", api_key=None):
    module_name = "aibrix_mock_app_endpoints_under_test"
    monkeypatch.chdir(APP_DIR)
    monkeypatch.setenv("STANDALONE_MODE", "true")
    monkeypatch.setenv("SIMULATION", "disabled")
    monkeypatch.setenv("MOCK_REQUEST_DURATION_SECONDS", "0")
    monkeypatch.setenv("MOCK_CAPACITY_AWARE_LATENCY", "false")
    monkeypatch.setenv("MOCK_PD_CONTRACT", contract)
    monkeypatch.setenv("MOCK_PD_ROLE", role)
    if api_key is None:
        monkeypatch.setattr(sys, "argv", ["app.py"])
    else:
        monkeypatch.setattr(sys, "argv", ["app.py", "--api_key", api_key])
    sys.modules.pop(module_name, None)

    spec = importlib.util.spec_from_file_location(module_name, APP_PATH)
    module = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = module
    spec.loader.exec_module(module)
    return module


def post_json(client, path, payload, request_id="req-1", **headers):
    request_headers = {"Content-Type": "application/json", "X-Request-ID": request_id}
    request_headers.update(headers)
    raw_body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    return client.post(path, data=raw_body, headers=request_headers)


def query_records(client, request_id):
    response = client.get(f"/debug/requests?request_id={request_id}")
    assert response.status_code == 200
    return response.get_json()


def test_sglang_prefill_failure_is_observed_by_decode(monkeypatch):
    module = load_mock_module(monkeypatch, contract="sglang-http", role="prefill")
    client = module.app.test_client()
    payload = {
        "model": "m",
        "messages": [{"role": "user", "content": "hello"}],
        "bootstrap_host": "10.0.0.1",
        "bootstrap_port": 8998,
        "bootstrap_room": 123,
    }
    request_id = "sglang-failure"

    prefill_response = post_json(
        client,
        "/v1/chat/completions",
        payload,
        request_id=request_id,
        **{"x-aibrix-mock-fail": "prefill"},
    )
    assert prefill_response.status_code == 500
    assert query_records(client, request_id)[0]["outcome"] == "failed"

    monkeypatch.setenv("MOCK_PD_ROLE", "decode")
    decode_response = post_json(
        client,
        "/v1/chat/completions",
        payload,
        request_id=request_id,
        **{"x-aibrix-mock-fail": "prefill"},
    )

    assert decode_response.status_code == 500
    assert "prefill handoff failed" in decode_response.get_json()["error"]["message"]
    records = query_records(client, request_id)
    assert records[-1]["role"] == "decode"
    assert records[-1]["outcome"] == "failed"


def test_legacy_completion_records_exact_raw_body_and_debug_endpoint_is_public(monkeypatch):
    module = load_mock_module(monkeypatch, api_key="secret")
    client = module.app.test_client()
    raw_body = b'{ "model": "m", "prompt": "hello", "max_tokens": 1 }'

    response = client.post(
        "/v1/completions",
        data=raw_body,
        headers={
            "Content-Type": "application/json",
            "X-Request-ID": "legacy-raw",
            "Authorization": "Bearer secret",
        },
    )

    assert response.status_code == 200
    records = query_records(client, "legacy-raw")
    assert len(records) == 1
    record = records[0]
    assert base64.b64decode(record["raw_body_base64"]) == raw_body
    assert record["parsed_json"] == {"model": "m", "prompt": "hello", "max_tokens": 1}
    assert record["path"] == "/v1/completions"
    assert record["outcome"] == "success"
    assert record["status_code"] == 200
    assert record["response"]["object"] == "text_completion"
    assert record["timestamp_finished"]

    debug_response = client.get("/debug/requests?request_id=legacy-raw")
    assert debug_response.status_code == 200


@pytest.mark.parametrize(
    "header_name",
    ["authorization", "COOKIE", "Proxy-Authorization", "x-api-key"],
)
def test_recorder_redacts_sensitive_headers_case_insensitively(monkeypatch, header_name):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    secret = f"{header_name}-secret"
    header_value = (
        f"session={secret}" if header_name.lower() == "cookie" else secret
    )

    if header_name.lower() == "cookie":
        client.set_cookie("session", secret)
        response = client.post(
            "/v1/completions",
            data=json.dumps(
                {"model": "m", "prompt": "hello", "max_tokens": 1},
                separators=(",", ":"),
            ).encode("utf-8"),
            headers={
                "Content-Type": "application/json",
                "X-Request-ID": f"redact-{header_name}",
            },
        )
    else:
        response = post_json(
            client,
            "/v1/completions",
            {"model": "m", "prompt": "hello", "max_tokens": 1},
            request_id=f"redact-{header_name}",
            **{header_name: header_value},
        )

    assert response.status_code == 200
    record = query_records(client, f"redact-{header_name}")[0]
    recorded_value = next(
        value
        for key, value in record["headers"].items()
        if key.lower() == header_name.lower()
    )
    assert recorded_value == "<redacted>"
    assert secret not in json.dumps(record)


@pytest.mark.parametrize("status_code", [400, 500])
def test_safe_merge_does_not_modify_error_responses(monkeypatch, status_code):
    module = load_mock_module(monkeypatch)

    with module.app.test_request_context("/"):
        response = module.make_response(
            module.jsonify({"error": {"message": "backend rejected"}}),
            status_code,
        )
        contract_result = {
            "body": {
                "opaque": "contract-opaque",
                "disagg_prefill_resp": {"should": "not merge"},
            },
            "response_patch": {"prompt_token_ids": [1, 2]},
            "choice_patch": {"disaggregated_params": {"request_type": "context_only"}},
        }

        merged = module._safe_merge_contract_response(
            response,
            contract_result,
            "/v1/chat/completions",
            {"model": "m"},
        )

    assert merged.status_code == status_code
    assert merged.get_json() == {"error": {"message": "backend rejected"}}


def test_evicted_inflight_record_does_not_change_success_response_to_500(monkeypatch):
    module = load_mock_module(monkeypatch)
    module.request_recorder = RequestRecorder(capacity=1)
    first_sleep_started = threading.Event()
    real_sleep = time.sleep

    def controlled_sleep(seconds):
        if seconds == 0.025:
            first_sleep_started.set()
        real_sleep(seconds)

    monkeypatch.setattr(module.time, "sleep", controlled_sleep)
    first_result = {}

    def send_first_request():
        client = module.app.test_client()
        first_result["response"] = post_json(
            client,
            "/v1/completions",
            {"model": "m", "prompt": "first", "max_tokens": 1},
            request_id="evicted-first",
            **{"X-Aibrix-Mock-Delay-Ms": "25"},
        )

    first_thread = threading.Thread(target=send_first_request)
    first_thread.start()
    assert first_sleep_started.wait(timeout=1)

    second_response = post_json(
        module.app.test_client(),
        "/v1/completions",
        {"model": "m", "prompt": "second", "max_tokens": 1},
        request_id="evicted-second",
    )
    first_thread.join(timeout=1)

    assert not first_thread.is_alive()
    assert first_result["response"].status_code == 200
    assert second_response.status_code == 200
    assert query_records(module.app.test_client(), "evicted-second")[0]["outcome"] == "success"


@pytest.mark.parametrize("path,payload", [
    ("/v1/completions", {"model": "m", "prompt": "hello"}),
    ("/v1/chat/completions", {"model": "m", "messages": [{"role": "user", "content": "hello"}]}),
])
@pytest.mark.parametrize("authorization", [None, "Bearer wrong"])
def test_authentication_failures_are_recorded_before_returning_401(
    monkeypatch, path, payload, authorization
):
    module = load_mock_module(monkeypatch, api_key="secret")
    client = module.app.test_client()
    request_id = (
        f"auth-failure-{path.rsplit('/', 1)[-1]}-"
        f"{'missing' if authorization is None else 'wrong'}"
    )
    headers = {
        "Content-Type": "application/json",
        "X-Request-ID": request_id,
    }
    if authorization is not None:
        headers["Authorization"] = authorization

    response = client.post(
        path,
        data=json.dumps(payload).encode("utf-8"),
        headers=headers,
    )

    assert response.status_code == 401
    records = query_records(client, request_id)
    assert len(records) == 1
    assert records[0]["outcome"] == "rejected"
    assert records[0]["status_code"] == 401


@pytest.mark.parametrize("path", ["/v1/completions", "/v1/chat/completions"])
@pytest.mark.parametrize("case", [
    "malformed-json",
    "invalid-delay",
    "matched-fail",
    "contract-mismatch",
])
def test_unauthenticated_requests_return_401_before_body_fault_or_contract_processing(
    monkeypatch, path, case
):
    module = load_mock_module(
        monkeypatch,
        contract="vllm-aibrix-shfs",
        role="prefill",
        api_key="secret",
    )
    client = module.app.test_client()
    sleeps = []
    monkeypatch.setattr(module.time, "sleep", sleeps.append)
    request_id = f"unauth-before-{path.rsplit('/', 1)[-1]}-{case}"
    headers = {
        "Content-Type": "application/json",
        "X-Request-ID": request_id,
        "Authorization": "Bearer wrong",
    }
    payload = {
        "model": "m",
        "prompt": "hello",
        "messages": [{"role": "user", "content": "hello"}],
    }
    if case == "malformed-json":
        raw_body = b'{"model":'
    else:
        raw_body = json.dumps(payload).encode("utf-8")
    if case == "invalid-delay":
        headers["X-Aibrix-Mock-Delay-Ms"] = "not-an-int"
    elif case == "matched-fail":
        headers["X-Aibrix-Mock-Fail"] = "prefill"

    response = client.post(path, data=raw_body, headers=headers)

    assert response.status_code == 401
    record = query_records(client, request_id)[0]
    assert record["outcome"] == "rejected"
    assert record["status_code"] == 401
    assert sleeps == []


@pytest.mark.parametrize("path,payload", [
    ("/v1/completions", {"model": "m", "prompt": "hello", "max_tokens": 1}),
    ("/v1/chat/completions", {"model": "m", "messages": [{"role": "user", "content": "hello"}], "max_tokens": 1}),
])
def test_both_completion_endpoints_record_request_metadata(monkeypatch, path, payload):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    response = post_json(client, path, payload, request_id=path)

    assert response.status_code == 200
    records = query_records(client, path)
    assert len(records) == 1
    assert records[0]["path"] == path
    assert records[0]["engine"]
    assert records[0]["role"] == ""
    assert records[0]["response"]
    assert records[0]["error"] is None
    assert records[0]["delay_ms"] == 0


def test_malformed_legacy_json_returns_bad_request_and_finalizes_rejected_record(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    response = client.post(
        "/v1/chat/completions",
        data=b'{"model":',
        headers={"Content-Type": "application/json", "X-Request-ID": "bad-json"},
    )

    assert response.status_code == 400
    record = query_records(client, "bad-json")[0]
    assert record["parsed_json"] is None
    assert record["outcome"] == "rejected"
    assert record["status_code"] == 400
    assert record["error"]


@pytest.mark.parametrize(
    "path,payload,ordinary_field,finish_reason",
    [
        (
            "/v1/completions",
            {
                "model": "m",
                "prompt": "hello",
                "max_tokens": 1,
                "disaggregated_params": {"request_type": "context_only"},
            },
            "text",
            "length",
        ),
        (
            "/v1/chat/completions",
            {
                "model": "m",
                "messages": [{"role": "user", "content": "hello"}],
                "max_tokens": 1,
                "disaggregated_params": {"request_type": "context_only"},
            },
            "message",
            "stop",
        ),
    ],
)
def test_strict_trtllm_uses_contract_response_without_legacy_duplicate_fields(
    monkeypatch, path, payload, ordinary_field, finish_reason
):
    module = load_mock_module(
        monkeypatch,
        contract="trtllm-openai",
        role="prefill",
    )

    response = post_json(module.app.test_client(), path, payload, request_id=path)

    assert response.status_code == 200
    body = response.get_json()
    assert body["prompt_token_ids"] == list(range(body["usage"]["prompt_tokens"]))
    assert body["prompt_token_ids"]
    assert "disaggregated_params" not in body
    choice = body["choices"][0]
    assert ordinary_field in choice
    assert choice["finish_reason"] == finish_reason
    assert choice["disaggregated_params"]["request_type"] == "context_only"


def test_shfs_prefill_contract_builds_transfer_fields_and_records_response(monkeypatch):
    module = load_mock_module(monkeypatch, contract="vllm-aibrix-shfs", role="prefill")
    client = module.app.test_client()
    payload = {
        "model": "m",
        "prompt": "hello",
        "max_tokens": 1,
        "kv_transfer_params": {"do_remote_decode": True},
    }

    response = post_json(client, "/v1/completions", payload, request_id="shfs-prefill")

    assert response.status_code == 200
    body = response.get_json()
    transfer = body["kv_transfer_params"]
    assert transfer["do_remote_decode"] is False
    assert transfer["do_remote_prefill"] is True
    assert transfer["opaque"] == "aibrix-pd-contract-opaque-sentinel"
    record = query_records(client, "shfs-prefill")[0]
    assert record["role"] == "prefill"
    assert record["outcome"] == "success"
    assert record["response"]["kv_transfer_params"] == transfer


def test_trtllm_prefill_merges_patches_without_overwriting_normal_choice_fields(monkeypatch):
    module = load_mock_module(monkeypatch, contract="trtllm-openai", role="prefill")
    client = module.app.test_client()
    payload = {
        "model": "m",
        "messages": [{"role": "user", "content": "hello"}],
        "max_tokens": 1,
        "disaggregated_params": {"request_type": "context_only"},
    }

    response = post_json(client, "/v1/chat/completions", payload, request_id="trt-prefill")

    assert response.status_code == 200
    body = response.get_json()
    assert body["prompt_token_ids"] == list(range(body["usage"]["prompt_tokens"]))
    assert body["prompt_token_ids"]
    assert "disaggregated_params" not in body
    assert body["choices"][0]["disaggregated_params"]["request_type"] == "context_only"
    assert body["choices"][0]["disaggregated_params"]["encoded_opaque_state"]
    assert body["choices"][0]["message"]["role"] == "assistant"
    assert body["choices"][0]["finish_reason"] == "stop"
    assert query_records(client, "trt-prefill")[0]["outcome"] == "success"


def test_legacy_trtllm_prefill_uses_http_encoded_opaque_state(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    payload = {
        "model": "m",
        "prompt": "hello",
        "max_tokens": 1,
        "disaggregated_params": {"request_type": "context_only"},
    }

    response = post_json(client, "/v1/completions", payload, request_id="legacy-trt")

    assert response.status_code == 200
    disaggregated = response.get_json()["disaggregated_params"]
    assert disaggregated["encoded_opaque_state"]
    assert "opaque_state" not in disaggregated


def test_nixl_endpoint_returns_unwrapped_prefill_body_that_gateway_can_wrap(monkeypatch):
    module = load_mock_module(monkeypatch, contract="vllm-aibrix-nixl", role="prefill")
    client = module.app.test_client()
    payload = {"model": "m", "prompt": "hello", "max_tokens": 1}

    prefill_response = post_json(
        client,
        "/v1/completions",
        payload,
        request_id="nixl-prefill-shape",
    )

    assert prefill_response.status_code == 200
    prefill_body = prefill_response.get_json()
    assert "disagg_prefill_resp" not in prefill_body
    assert prefill_body["opaque"] == "aibrix-pd-contract-opaque-sentinel"

    gateway_decode_body = dict(payload)
    gateway_decode_body["disagg_prefill_resp"] = prefill_body
    module = load_mock_module(monkeypatch, contract="vllm-aibrix-nixl", role="decode")
    decode_response = post_json(
        module.app.test_client(),
        "/v1/completions",
        gateway_decode_body,
        request_id="nixl-decode-shape",
    )

    assert decode_response.status_code == 200
def test_contract_rejection_is_recorded_and_does_not_fall_through_to_success(monkeypatch):
    module = load_mock_module(monkeypatch, contract="vllm-aibrix-shfs", role="prefill")
    client = module.app.test_client()
    payload = {"model": "m", "prompt": "hello", "max_tokens": 1}

    response = post_json(client, "/v1/completions", payload, request_id="contract-reject")

    assert response.status_code == 400
    assert response.get_json()["error"]["type"] == "invalid_request"
    record = query_records(client, "contract-reject")[0]
    assert record["outcome"] == "rejected"
    assert record["status_code"] == 400
    assert record["response"]["error"]["type"] == "invalid_request"
    assert record["error"]


def test_role_mismatch_is_rejected_by_configured_contract(monkeypatch):
    module = load_mock_module(monkeypatch, contract="trtllm-openai", role="worker")
    client = module.app.test_client()

    response = post_json(
        client,
        "/v1/completions",
        {"model": "m", "prompt": "hello"},
        request_id="bad-role",
    )

    assert response.status_code == 400
    record = query_records(client, "bad-role")[0]
    assert record["outcome"] == "rejected"
    assert record["status_code"] == 400


def test_matching_fault_fails_and_nonmatching_fault_does_not(monkeypatch):
    module = load_mock_module(monkeypatch, contract="sglang-http", role="prefill")
    client = module.app.test_client()
    payload = {
        "model": "m",
        "prompt": "hello",
        "max_tokens": 1,
        "bootstrap_host": "host",
        "bootstrap_port": 1234,
        "bootstrap_room": 7,
    }

    failed = post_json(
        client,
        "/v1/completions",
        payload,
        request_id="fault-match",
        **{"X-Aibrix-Mock-Fail": "prefill"},
    )
    unaffected = post_json(
        client,
        "/v1/completions",
        payload,
        request_id="fault-other",
        **{"X-Aibrix-Mock-Fail": "decode"},
    )

    assert failed.status_code == 500
    assert unaffected.status_code == 200
    assert query_records(client, "fault-match")[0]["outcome"] == "failed"
    assert query_records(client, "fault-other")[0]["outcome"] == "success"


def test_valid_delay_sleeps_once_and_is_recorded(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    slept = []
    monkeypatch.setattr(module.time, "sleep", slept.append)

    response = post_json(
        client,
        "/v1/completions",
        {"model": "m", "prompt": "hello", "max_tokens": 1},
        request_id="delayed",
        **{"X-Aibrix-Mock-Delay-Ms": "25"},
    )

    assert response.status_code == 200
    assert 0.025 in slept
    assert query_records(client, "delayed")[0]["delay_ms"] == 25


def test_invalid_delay_is_bad_request_and_recorded(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    response = post_json(
        client,
        "/v1/chat/completions",
        {"model": "m", "messages": [{"role": "user", "content": "hello"}]},
        request_id="bad-delay",
        **{"X-Aibrix-Mock-Delay-Ms": "not-an-int"},
    )

    assert response.status_code == 400
    record = query_records(client, "bad-delay")[0]
    assert record["outcome"] == "rejected"
    assert record["status_code"] == 400
    assert record["delay_ms"] == 0
    assert record["error"] == "invalid x-aibrix-mock-delay-ms"


def test_streaming_response_is_not_consumed_by_route_but_is_recorded(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()

    response = client.post(
        "/v1/chat/completions",
        data=json.dumps({
            "model": "m",
            "messages": [{"role": "user", "content": "hello"}],
            "stream": True,
        }).encode(),
        headers={"Content-Type": "application/json", "X-Request-ID": "stream"},
        buffered=False,
    )

    assert response.status_code == 200
    assert response.mimetype == "text/event-stream"
    record = query_records(client, "stream")[0]
    assert record["outcome"] == "success"
    assert record["response"]["stream"] is True
    response.close()


def test_legacy_omni_image_and_audio_shapes_remain_available(monkeypatch):
    module = load_mock_module(monkeypatch)
    client = module.app.test_client()
    image_payload = {
        "model": "qwen-image",
        "messages": [{"role": "user", "content": "draw a cat"}],
    }
    audio_payload = {
        "model": "m",
        "messages": [{"role": "user", "content": "speak"}],
        "modalities": ["text", "audio"],
    }

    image_response = post_json(client, "/v1/chat/completions", image_payload, "omni-image")
    audio_response = post_json(client, "/v1/chat/completions", audio_payload, "omni-audio")

    assert image_response.status_code == 200
    assert image_response.get_json()["choices"][0]["message"]["content"][0]["type"] == "image_url"
    assert audio_response.status_code == 200
    assert audio_response.get_json()["choices"][0]["message"]["audio"]["format"] == "wav"
