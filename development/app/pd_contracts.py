"""Pure-Python validators and response builders for gateway PD contracts."""

from copy import deepcopy
from typing import NamedTuple


CONTRACT_VLLM_AIBRIX_SHFS = "vllm-aibrix-shfs"
CONTRACT_VLLM_AIBRIX_NIXL = "vllm-aibrix-nixl"
CONTRACT_SGLANG_HTTP = "sglang-http"
CONTRACT_TRTLLM_OPENAI = "trtllm-openai"

PREFILL = "prefill"
DECODE = "decode"
OPAQUE_SENTINEL = "aibrix-pd-contract-opaque-sentinel"

_CONTRACTS = frozenset(
    (
        CONTRACT_VLLM_AIBRIX_SHFS,
        CONTRACT_VLLM_AIBRIX_NIXL,
        CONTRACT_SGLANG_HTTP,
        CONTRACT_TRTLLM_OPENAI,
    )
)
_ROLES = frozenset((PREFILL, DECODE))
MAX_FAULT_DELAY_MS = 30_000


class FrozenDict(dict):
    """A JSON-serializable dict that cannot be mutated after construction."""

    def __init__(self, *args, **kwargs):
        if getattr(self, "_initialized", False):
            raise TypeError("FrozenDict is already initialized")
        values = dict(*args, **kwargs)
        for key, value in values.items():
            dict.__setitem__(self, key, value)
        object.__setattr__(self, "_initialized", True)

    def __setattr__(self, name, value):
        raise TypeError("FrozenDict is immutable")

    def _raise_immutable(self, *args, **kwargs):
        raise TypeError("FrozenDict is immutable")

    __setitem__ = _raise_immutable
    __delitem__ = _raise_immutable
    clear = _raise_immutable
    pop = _raise_immutable
    popitem = _raise_immutable
    setdefault = _raise_immutable
    update = _raise_immutable
    __ior__ = _raise_immutable


class FaultParseResult(NamedTuple):
    """Immutable result of parsing request-scoped mock fault headers."""

    delay_ms: int
    injected_status_code: int | None
    validation_status_code: int
    metadata: FrozenDict


def parse_fault_headers(headers, role):
    """Parse fault-injection headers without sleeping or changing shared state."""
    normalized_headers = {str(key).lower(): value for key, value in headers.items()}
    delay_value = normalized_headers.get("x-aibrix-mock-delay-ms")

    if "x-aibrix-mock-delay-ms" not in normalized_headers:
        delay_ms = 0
    elif isinstance(delay_value, bool):
        return FaultParseResult(
            delay_ms=0,
            injected_status_code=None,
            validation_status_code=400,
            metadata=FrozenDict(error="invalid x-aibrix-mock-delay-ms"),
        )
    else:
        try:
            delay_ms = int(delay_value)
        except (TypeError, ValueError):
            return FaultParseResult(
                delay_ms=0,
                injected_status_code=None,
                validation_status_code=400,
                metadata=FrozenDict(error="invalid x-aibrix-mock-delay-ms"),
            )
        if str(delay_value) != str(delay_ms) or not 0 <= delay_ms <= MAX_FAULT_DELAY_MS:
            return FaultParseResult(
                delay_ms=0,
                injected_status_code=None,
                validation_status_code=400,
                metadata=FrozenDict(error="invalid x-aibrix-mock-delay-ms"),
            )

    fail_value = normalized_headers.get("x-aibrix-mock-fail")
    injected_status_code = 500 if fail_value in _ROLES and fail_value == role else None
    return FaultParseResult(
        delay_ms=delay_ms,
        injected_status_code=injected_status_code,
        validation_status_code=200,
        metadata=FrozenDict(),
    )


def _result(
    contract,
    role,
    request_id,
    status_code,
    body=None,
    error=None,
    response_patch=None,
    choice_patch=None,
):
    return {
        "status_code": status_code,
        "metadata": {
            "contract": contract,
            "role": role,
            "request_id": request_id,
            "error": error,
            "error_type": None,
            "message": error,
        },
        "body": body,
        "response_patch": response_patch or {},
        "choice_patch": choice_patch or {},
    }


def _error(contract, role, request_id, message):
    error_type = "invalid_contract_or_role" if "contract" in message or "role" in message else "invalid_request"
    result = _result(contract, role, request_id, 400, error=message)
    result["metadata"]["error_type"] = error_type
    result["body"] = {"error": {"type": error_type, "message": message}}
    return result


def _require_mapping(payload, contract, role, request_id):
    if not isinstance(payload, dict):
        return _error(contract, role, request_id, "payload must be an object")
    return None


def _nested_mapping(payload, key, contract, role, request_id):
    value = payload.get(key)
    if not isinstance(value, dict):
        return None, _error(contract, role, request_id, f"missing {key}")
    return value, None


def _validate_shfs(contract, role, payload, request_id):
    params, error = _nested_mapping(payload, "kv_transfer_params", contract, role, request_id)
    if error:
        return error
    if role == DECODE:
        required = (
            "do_remote_decode",
            "do_remote_prefill",
            "remote_engine_id",
            "remote_block_ids",
            "remote_host",
            "remote_port",
            "opaque",
        )
        for key in required:
            if key not in params:
                return _error(contract, role, request_id, f"missing kv_transfer_params.{key}")
        if params["do_remote_decode"] is not False:
            return _error(contract, role, request_id, "kv_transfer_params.do_remote_decode must be false")
        if params["do_remote_prefill"] is not True:
            return _error(contract, role, request_id, "kv_transfer_params.do_remote_prefill must be true")
        if not isinstance(params["remote_engine_id"], str) or not params["remote_engine_id"]:
            return _error(contract, role, request_id, "invalid kv_transfer_params.remote_engine_id")
        if not isinstance(params["remote_block_ids"], list) or not params["remote_block_ids"]:
            return _error(contract, role, request_id, "invalid kv_transfer_params.remote_block_ids")
        if not isinstance(params["remote_host"], str) or not params["remote_host"]:
            return _error(contract, role, request_id, "invalid kv_transfer_params.remote_host")
        if (
            isinstance(params["remote_port"], bool)
            or not isinstance(params["remote_port"], int)
            or params["remote_port"] <= 0
        ):
            return _error(contract, role, request_id, "invalid kv_transfer_params.remote_port")
        if params["opaque"] != OPAQUE_SENTINEL:
            return _error(contract, role, request_id, "invalid kv_transfer_params.opaque sentinel")
        return _result(contract, role, request_id, 200, deepcopy(payload))

    if params.get("do_remote_decode") is not True:
        return _error(contract, role, request_id, "kv_transfer_params.do_remote_decode must be true")
    body = deepcopy(payload)
    transfer = body["kv_transfer_params"]
    transfer.update(
        {
            "do_remote_decode": False,
            "do_remote_prefill": True,
            "remote_engine_id": transfer.get("remote_engine_id") or "prefill-engine",
            "remote_block_ids": transfer.get("remote_block_ids") or ["block-1"],
            "remote_host": transfer.get("remote_host") or "prefill.example",
            "remote_port": transfer.get("remote_port") or 8001,
            "opaque": OPAQUE_SENTINEL,
        }
    )
    return _result(contract, role, request_id, 200, body)


def _validate_nixl(contract, role, payload, request_id):
    if role == PREFILL:
        params = payload.get("kv_transfer_params")
        if isinstance(params, dict) and "do_remote_decode" in params:
            return _error(
                contract,
                role,
                request_id,
                "nixl prefill must not contain SHFS kv_transfer_params skeleton",
            )
        response = deepcopy(payload)
        response["opaque"] = OPAQUE_SENTINEL
        # The gateway wraps the complete prefill HTTP response itself when it
        # builds the decode request. Keep the mock response unwrapped.
        return _result(contract, role, request_id, 200, response)

    response, error = _nested_mapping(payload, "disagg_prefill_resp", contract, role, request_id)
    if error:
        return error
    if response.get("opaque") != OPAQUE_SENTINEL:
        return _error(contract, role, request_id, "missing disagg_prefill_resp.opaque sentinel")
    return _result(contract, role, request_id, 200, deepcopy(payload))


def _validate_sglang(contract, role, payload, request_id):
    host = payload.get("bootstrap_host")
    port = payload.get("bootstrap_port")
    room = payload.get("bootstrap_room")
    if not isinstance(host, str) or not host:
        return _error(contract, role, request_id, "invalid bootstrap_host")
    if isinstance(port, bool) or not isinstance(port, int) or port <= 0:
        return _error(contract, role, request_id, "invalid bootstrap_port")
    if isinstance(room, bool) or not isinstance(room, int) or room < 0:
        return _error(contract, role, request_id, "invalid bootstrap_room")
    return _result(contract, role, request_id, 200, deepcopy(payload))


def _validate_trtllm(contract, role, payload, request_id):
    params, error = _nested_mapping(payload, "disaggregated_params", contract, role, request_id)
    if error:
        return error
    expected = "context_only" if role == PREFILL else "generation_only"
    if params.get("request_type") != expected:
        return _error(contract, role, request_id, f"disaggregated_params.request_type must be {expected}")
    if role == DECODE:
        token_ids = payload.get("prompt_token_ids")
        if not isinstance(token_ids, list) or any(
            isinstance(token, bool) or not isinstance(token, int) for token in token_ids
        ):
            return _error(contract, role, request_id, "missing prompt_token_ids")
        for key in ("disagg_request_id", "first_gen_tokens", "encoded_opaque_state"):
            if key not in params:
                return _error(contract, role, request_id, f"missing disaggregated_params.{key}")
        disagg_request_id = params["disagg_request_id"]
        valid_request_id = (
            isinstance(disagg_request_id, int)
            and not isinstance(disagg_request_id, bool)
        ) or (
            isinstance(disagg_request_id, str)
            and bool(disagg_request_id)
            and disagg_request_id.lstrip("-").isdigit()
        )
        if not valid_request_id:
            return _error(contract, role, request_id, "invalid disaggregated_params.disagg_request_id")
        if not isinstance(params["first_gen_tokens"], list) or any(
            isinstance(token, bool) or not isinstance(token, int) for token in params["first_gen_tokens"]
        ):
            return _error(contract, role, request_id, "invalid disaggregated_params.first_gen_tokens")
        if not isinstance(params["encoded_opaque_state"], str):
            return _error(contract, role, request_id, "invalid disaggregated_params.encoded_opaque_state")
        return _result(contract, role, request_id, 200, deepcopy(payload))

    prefill_params = deepcopy(params)
    prefill_params.setdefault("first_gen_tokens", [1])
    prefill_params.setdefault("encoded_opaque_state", OPAQUE_SENTINEL)
    response_patch = {}
    token_ids = payload.get("prompt_token_ids")
    if token_ids is not None:
        if not isinstance(token_ids, list) or any(
            isinstance(token, bool) or not isinstance(token, int) for token in token_ids
        ):
            return _error(contract, role, request_id, "invalid prompt_token_ids")
        response_patch["prompt_token_ids"] = deepcopy(token_ids)

    return _result(
        contract,
        role,
        request_id,
        200,
        response_patch=response_patch,
        choice_patch={"disaggregated_params": prefill_params},
    )


def add_prompt_token_ids(result, prompt_token_count):
    """Add gateway-facing TRT-LLM prompt IDs when the request omitted them."""
    if result["status_code"] != 200 or not isinstance(prompt_token_count, int):
        return result

    if "prompt_token_ids" in result.get("response_patch", {}):
        return result

    enriched = deepcopy(result)
    enriched.setdefault("response_patch", {})["prompt_token_ids"] = list(
        range(max(prompt_token_count, 0))
    )
    return enriched


def validate_or_build(contract, role, payload, request_id=None):
    """Validate a gateway handoff and return a transport-neutral HTTP result."""
    if not isinstance(contract, str):
        return _error(contract, role, request_id, "contract must be a string")
    if not isinstance(role, str) or role not in _ROLES:
        return _error(contract, role, request_id, "role must be prefill or decode")
    if contract and contract not in _CONTRACTS:
        return _error(contract, role, request_id, f"unknown contract: {contract}")
    if not contract:
        return _result(contract, role, request_id, 200, deepcopy(payload))
    error = _require_mapping(payload, contract, role, request_id)
    if error:
        return error
    validators = {
        CONTRACT_VLLM_AIBRIX_SHFS: _validate_shfs,
        CONTRACT_VLLM_AIBRIX_NIXL: _validate_nixl,
        CONTRACT_SGLANG_HTTP: _validate_sglang,
        CONTRACT_TRTLLM_OPENAI: _validate_trtllm,
    }
    return validators[contract](contract, role, payload, request_id)
