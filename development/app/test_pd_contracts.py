import json

import pytest

from pd_contracts import (
    CONTRACT_SGLANG_HTTP,
    CONTRACT_TRTLLM_OPENAI,
    CONTRACT_VLLM_AIBRIX_NIXL,
    CONTRACT_VLLM_AIBRIX_SHFS,
    FrozenDict,
    OPAQUE_SENTINEL,
    parse_fault_headers,
    validate_or_build,
)


def assert_ok(result):
    assert result["status_code"] == 200
    assert result["metadata"]["error"] is None
    return result["body"]


def assert_bad(result, message):
    assert result["status_code"] == 400
    if message:
        assert result["metadata"]["error"] == message
    else:
        assert result["metadata"]["error"]


def test_unknown_non_empty_contract_is_rejected_with_http_400_metadata():
    result = validate_or_build("future-contract", "prefill", {})

    assert_bad(result, "unknown contract: future-contract")


@pytest.mark.parametrize("role", ["", "worker", None])
def test_role_must_be_prefill_or_decode(role):
    result = validate_or_build(CONTRACT_SGLANG_HTTP, role, {})

    assert result["status_code"] == 400
    assert "role" in result["metadata"]["error"]


def test_shfs_prefill_requires_remote_decode_and_returns_transfer_fields():
    payload = {
        "kv_transfer_params": {"do_remote_decode": True, "request_id": "req-1"}
    }

    body = assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "prefill", payload))
    transfer = body["kv_transfer_params"]

    assert transfer["do_remote_decode"] is False
    assert transfer["do_remote_prefill"] is True
    assert transfer["remote_engine_id"]
    assert transfer["remote_block_ids"]
    assert transfer["remote_host"]
    assert transfer["remote_port"] > 0
    assert transfer["opaque"] == OPAQUE_SENTINEL
    assert transfer["request_id"] == "req-1"
    assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "decode", body))


def test_shfs_prefill_replaces_null_gateway_skeleton_and_builds_decode_fixture():
    payload = {
        "kv_transfer_params": {
            "do_remote_decode": True,
            "do_remote_prefill": None,
            "remote_engine_id": None,
            "remote_block_ids": None,
            "remote_host": None,
            "remote_port": None,
            "opaque": None,
        }
    }

    body = assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "prefill", payload))
    transfer = body["kv_transfer_params"]

    assert transfer["do_remote_decode"] is False
    assert transfer["do_remote_prefill"] is True
    assert transfer["remote_engine_id"]
    assert transfer["remote_block_ids"]
    assert transfer["remote_host"]
    assert transfer["remote_port"] > 0
    assert transfer["opaque"] == OPAQUE_SENTINEL
    assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "decode", body))


def test_shfs_decode_requires_gateway_merged_transfer_fields_and_sentinel():
    payload = {
        "kv_transfer_params": {
            "do_remote_decode": False,
            "do_remote_prefill": True,
            "remote_engine_id": "prefill-engine",
            "remote_block_ids": ["block-1", "block-2"],
            "remote_host": "prefill.example",
            "remote_port": 8001,
            "opaque": OPAQUE_SENTINEL,
        }
    }

    assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "decode", payload))


@pytest.mark.parametrize(
    "field",
    [
        "do_remote_decode",
        "do_remote_prefill",
        "remote_engine_id",
        "remote_block_ids",
        "remote_host",
        "remote_port",
        "opaque",
    ],
)
def test_shfs_decode_rejects_missing_merged_field(field):
    payload = {
        "kv_transfer_params": {
            "do_remote_decode": False,
            "do_remote_prefill": True,
            "remote_engine_id": "prefill-engine",
            "remote_block_ids": ["block-1"],
            "remote_host": "prefill.example",
            "remote_port": 8001,
            "opaque": OPAQUE_SENTINEL,
        }
    }
    del payload["kv_transfer_params"][field]

    assert_bad(
        validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "decode", payload),
        f"missing kv_transfer_params.{field}",
    )


@pytest.mark.parametrize(
    "field,value",
    [
        ("do_remote_decode", True),
        ("do_remote_prefill", False),
        ("remote_engine_id", ""),
        ("remote_block_ids", []),
        ("remote_host", ""),
        ("remote_port", 0),
        ("opaque", "wrong-sentinel"),
    ],
)
def test_shfs_decode_rejects_invalid_merged_field_value(field, value):
    payload = {
        "kv_transfer_params": {
            "do_remote_decode": False,
            "do_remote_prefill": True,
            "remote_engine_id": "prefill-engine",
            "remote_block_ids": ["block-1"],
            "remote_host": "prefill.example",
            "remote_port": 8001,
            "opaque": OPAQUE_SENTINEL,
        }
    }
    payload["kv_transfer_params"][field] = value

    result = validate_or_build(CONTRACT_VLLM_AIBRIX_SHFS, "decode", payload)
    assert result["status_code"] == 400
    assert field in result["metadata"]["error"]


def test_nixl_prefill_rejects_shfs_skeleton_and_returns_unwrapped_response_for_gateway():
    assert_bad(
        validate_or_build(
            CONTRACT_VLLM_AIBRIX_NIXL,
            "prefill",
            {"kv_transfer_params": {"do_remote_decode": True}},
        ),
        "nixl prefill must not contain SHFS kv_transfer_params skeleton",
    )

    prefill_payload = {
        "model": "m",
        "prompt": "hello",
        "prompt_token_ids": [1, 2],
        "request_id": "req-2",
    }
    body = assert_ok(
        validate_or_build(
            CONTRACT_VLLM_AIBRIX_NIXL,
            "prefill",
            prefill_payload,
        )
    )
    assert "disagg_prefill_resp" not in body
    assert body["opaque"] == OPAQUE_SENTINEL
    assert body["prompt_token_ids"] == [1, 2]

    decode_payload = {
        "model": "m",
        "prompt": "hello",
        "disagg_prefill_resp": body,
    }
    decoded = assert_ok(
        validate_or_build(CONTRACT_VLLM_AIBRIX_NIXL, "decode", decode_payload)
    )
    assert decoded["disagg_prefill_resp"] == body


def test_nixl_decode_requires_top_level_complete_prefill_response_and_sentinel():
    payload = {
        "disagg_prefill_resp": {
            "opaque": OPAQUE_SENTINEL,
            "prompt_token_ids": [1, 2],
        }
    }

    assert_ok(validate_or_build(CONTRACT_VLLM_AIBRIX_NIXL, "decode", payload))
    assert_bad(
        validate_or_build(
            CONTRACT_VLLM_AIBRIX_NIXL,
            "decode",
            {"disagg_prefill_resp": {"prompt_token_ids": [1, 2]}},
        ),
        "missing disagg_prefill_resp.opaque sentinel",
    )


@pytest.mark.parametrize("role", ["prefill", "decode"])
def test_sglang_requires_bootstrap_fields_for_both_roles(role):
    payload = {
        "bootstrap_host": "127.0.0.1",
        "bootstrap_port": 12345,
        "bootstrap_room": 0,
    }
    assert_ok(validate_or_build(CONTRACT_SGLANG_HTTP, role, payload))
    for field, value in (("bootstrap_host", ""), ("bootstrap_port", 0), ("bootstrap_room", -1)):
        invalid = dict(payload)
        invalid[field] = value
        assert_bad(
            validate_or_build(CONTRACT_SGLANG_HTTP, role, invalid),
            f"invalid {field}",
        )


def test_trtllm_prefill_builds_openai_disaggregated_response():
    payload = {
        "disaggregated_params": {
            "request_type": "context_only",
            "disagg_request_id": "req-prefill",
        },
    }

    result = validate_or_build(CONTRACT_TRTLLM_OPENAI, "prefill", payload)
    assert_ok(result)

    assert result["choice_patch"]["disaggregated_params"]["request_type"] == "context_only"
    assert result["choice_patch"]["disaggregated_params"]["encoded_opaque_state"]
    assert "prompt_token_ids" not in result["response_patch"]
    assert "choices" not in result["response_patch"]


def test_trtllm_prefill_patch_preserves_existing_openai_choice_fields():
    result = validate_or_build(
        CONTRACT_TRTLLM_OPENAI,
        "prefill",
        {"disaggregated_params": {"request_type": "context_only"}, "prompt_token_ids": [3]},
    )

    existing_choice = {"message": {"content": "hello"}, "finish_reason": "stop"}
    merged_choice = {**existing_choice, **result["choice_patch"]}
    merged_response = {"choices": [merged_choice], **result["response_patch"]}
    assert merged_response["choices"][0]["message"] == existing_choice["message"]
    assert merged_response["choices"][0]["finish_reason"] == "stop"
    assert merged_response["choices"][0]["disaggregated_params"]["request_type"] == "context_only"


def test_trtllm_gateway_prefill_response_without_input_prompt_ids_builds_decode_fixture():
    prefill_request = {
        "model": "m",
        "messages": [{"role": "user", "content": "hello"}],
        "disaggregated_params": {
            "request_type": "context_only",
            "disagg_request_id": 123456789,
        },
    }

    prefill_result = validate_or_build(
        CONTRACT_TRTLLM_OPENAI, "prefill", prefill_request
    )
    assert_ok(prefill_result)
    prefill_response = {
        "choices": [{
            "message": {"role": "assistant", "content": "prefill"},
            "finish_reason": "stop",
            "disaggregated_params": prefill_result["choice_patch"]["disaggregated_params"],
        }],
        "prompt_token_ids": [11, 12, 13],
    }

    decode_request = {
        "model": "m",
        "messages": prefill_request["messages"],
        "prompt_token_ids": prefill_response["prompt_token_ids"],
        "disaggregated_params": dict(prefill_response["choices"][0]["disaggregated_params"]),
    }
    decode_request["disaggregated_params"]["request_type"] = "generation_only"

    decoded = assert_ok(
        validate_or_build(CONTRACT_TRTLLM_OPENAI, "decode", decode_request)
    )
    assert decoded["prompt_token_ids"] == [11, 12, 13]
    assert decoded["disaggregated_params"]["request_type"] == "generation_only"
    assert decoded["disaggregated_params"]["encoded_opaque_state"]


@pytest.mark.parametrize(
    "contract,role",
    [
        ([], "prefill"),
        ({"contract": "not-hashable"}, "prefill"),
        (CONTRACT_SGLANG_HTTP, []),
        (CONTRACT_SGLANG_HTTP, {"role": "not-hashable"}),
    ],
)
def test_non_string_or_unhashable_contract_and_role_return_400_result(contract, role):
    result = validate_or_build(contract, role, {})
    assert result["status_code"] == 400
    assert result["metadata"]["error_type"] == "invalid_contract_or_role"
    assert result["body"]["error"]["type"] == "invalid_contract_or_role"


@pytest.mark.parametrize(
    "payload",
    [
        {"disaggregated_params": [], "prompt_token_ids": [1]},
        {"disaggregated_params": {"request_type": 1}, "prompt_token_ids": [1]},
        {"disaggregated_params": {"request_type": "context_only"}, "prompt_token_ids": [True]},
        {"disaggregated_params": {"request_type": "context_only"}, "prompt_token_ids": "1"},
    ],
)
def test_trtllm_prefill_rejects_invalid_field_types(payload):
    assert_bad(validate_or_build(CONTRACT_TRTLLM_OPENAI, "prefill", payload), "")


@pytest.mark.parametrize(
    "field,value",
    [
        ("disagg_request_id", True),
        ("first_gen_tokens", ["token"]),
        ("encoded_opaque_state", 42),
        ("prompt_token_ids", ["token"]),
    ],
)
def test_trtllm_decode_rejects_invalid_field_types(field, value):
    payload = {
        "disaggregated_params": {
            "request_type": "generation_only",
            "disagg_request_id": 3,
            "first_gen_tokens": [5],
            "encoded_opaque_state": "state",
        },
        "prompt_token_ids": [3, 4],
    }
    if field == "prompt_token_ids":
        payload[field] = value
    else:
        payload["disaggregated_params"][field] = value
    assert_bad(validate_or_build(CONTRACT_TRTLLM_OPENAI, "decode", payload), "")


@pytest.mark.parametrize("request_id", [9223372036854775807, "9223372036854775807"])
def test_trtllm_decode_accepts_gateway_numeric_disagg_request_id(request_id):
    payload = {
        "disaggregated_params": {
            "request_type": "generation_only",
            "disagg_request_id": request_id,
            "first_gen_tokens": [5],
            "encoded_opaque_state": "state",
        },
        "prompt_token_ids": [3, 4],
    }

    result = validate_or_build(CONTRACT_TRTLLM_OPENAI, "decode", payload)

    assert_ok(result)
    assert result["body"]["disaggregated_params"]["disagg_request_id"] == request_id


def test_trtllm_decode_validates_generation_handoff_fields():
    payload = {
        "disaggregated_params": {
            "request_type": "generation_only",
            "disagg_request_id": 3,
            "first_gen_tokens": [5],
            "encoded_opaque_state": "state",
        },
        "prompt_token_ids": [3, 4],
    }

    assert_ok(validate_or_build(CONTRACT_TRTLLM_OPENAI, "decode", payload))
    invalid = json.loads(json.dumps(payload))
    del invalid["disaggregated_params"]["encoded_opaque_state"]
    assert_bad(
        validate_or_build(CONTRACT_TRTLLM_OPENAI, "decode", invalid),
        "missing disaggregated_params.encoded_opaque_state",
    )


def test_metadata_includes_request_id_without_flask_dependency():
    result = validate_or_build(
        CONTRACT_SGLANG_HTTP,
        "prefill",
        {"bootstrap_host": "host", "bootstrap_port": 1, "bootstrap_room": 0},
        request_id="req-meta",
    )

    assert result["metadata"]["request_id"] == "req-meta"


@pytest.mark.parametrize("role", ["prefill", "decode"])
def test_fault_header_matching_role_injects_only_that_role(role):
    result = parse_fault_headers({"x-aibrix-mock-fail": role}, role)

    assert result.delay_ms == 0
    assert result.injected_status_code == 500
    assert result.validation_status_code == 200
    assert result.metadata == {}


@pytest.mark.parametrize("role,fail_value", [("prefill", "decode"), ("decode", "prefill")])
def test_fault_header_for_other_role_does_not_fail(role, fail_value):
    result = parse_fault_headers({"x-aibrix-mock-fail": fail_value}, role)

    assert result.injected_status_code is None
    assert result.delay_ms == 0
    assert result.validation_status_code == 200
    assert result.metadata == {}


@pytest.mark.parametrize("fail_value", ["", "worker", "PREFILL", "true", None])
def test_invalid_fail_header_is_ignored(fail_value):
    result = parse_fault_headers({"x-aibrix-mock-fail": fail_value}, "prefill")

    assert result.injected_status_code is None
    assert result.validation_status_code == 200
    assert result.metadata == {}


@pytest.mark.parametrize("delay_value,expected", [("0", 0), ("1", 1), ("30000", 30000)])
def test_delay_header_accepts_non_negative_integer_within_limit(delay_value, expected):
    result = parse_fault_headers({"x-aibrix-mock-delay-ms": delay_value}, "prefill")

    assert result.delay_ms == expected
    assert result.injected_status_code is None
    assert result.validation_status_code == 200
    assert result.metadata == {}


@pytest.mark.parametrize("delay_value", ["-1", "30001", "1.5", "", None, True])
def test_invalid_delay_header_returns_bad_request_metadata(delay_value):
    result = parse_fault_headers({"x-aibrix-mock-delay-ms": delay_value}, "prefill")

    assert result.validation_status_code == 400
    assert result.delay_ms == 0
    assert result.injected_status_code is None
    assert result.metadata["error"] == "invalid x-aibrix-mock-delay-ms"


def test_fault_parse_result_is_immutable_and_parser_does_not_mutate_headers():
    headers = {"x-aibrix-mock-delay-ms": "25", "x-aibrix-mock-fail": "decode"}
    result = parse_fault_headers(headers, "decode")

    assert headers == {"x-aibrix-mock-delay-ms": "25", "x-aibrix-mock-fail": "decode"}
    json.dumps(result._asdict())
    assert isinstance(result.metadata, dict)
    with pytest.raises(AttributeError):
        result.delay_ms = 30


@pytest.mark.parametrize(
    "mutate",
    [
        lambda metadata: metadata.__setitem__("error", "changed"),
        lambda metadata: metadata.update(error="changed"),
        lambda metadata: metadata.pop("error"),
        lambda metadata: metadata.popitem(),
        lambda metadata: metadata.clear(),
        lambda metadata: metadata.setdefault("other", "value"),
        lambda metadata: metadata.__delitem__("error"),
        lambda metadata: metadata.__ior__({"other": "value"}),
    ],
)
def test_fault_result_metadata_is_deeply_immutable(mutate):
    result = parse_fault_headers({"x-aibrix-mock-delay-ms": "invalid"}, "prefill")

    with pytest.raises(TypeError, match="^FrozenDict is immutable$"):
        mutate(result.metadata)
    json.dumps(result._asdict())


@pytest.mark.parametrize(
    "mutate",
    [
        lambda value: value.__setitem__("key", "value"),
        lambda value: value.__delitem__("error"),
        lambda value: value.clear(),
        lambda value: value.pop("error"),
        lambda value: value.popitem(),
        lambda value: value.setdefault("key", "value"),
        lambda value: value.update(key="value"),
        lambda value: value.__ior__({"key": "value"}),
    ],
)
def test_frozen_dict_mutators_raise_the_same_immutable_error_and_remain_json_serializable(mutate):
    value = FrozenDict(error="immutable")

    with pytest.raises(TypeError, match="^FrozenDict is immutable$"):
        mutate(value)

    assert value == {"error": "immutable"}
    json.dumps(value)


def test_fault_result_metadata_rejects_reinitialization():
    result = parse_fault_headers({"x-aibrix-mock-delay-ms": "invalid"}, "prefill")

    with pytest.raises(TypeError):
        result.metadata.__init__({"error": "changed"})
    assert result.metadata == {"error": "invalid x-aibrix-mock-delay-ms"}
    json.dumps(result._asdict())
