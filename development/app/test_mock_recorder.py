import base64
import json
from concurrent.futures import ThreadPoolExecutor
import math
import threading

import pytest

from mock_recorder import RequestRecorder, _RecordHandle


def make_record(recorder, request_id="request-1", raw_body=b'{"prompt":"hello"}'):
    return recorder.start(
        path="/v1/chat/completions",
        headers={"x-request-id": request_id, "content-type": "application/json"},
        raw_body=raw_body,
        parsed_json={"prompt": "hello"},
        pod="mock-pod-0",
        engine="vllm",
        role="prefill",
    )


def test_start_and_finish_record_request_metadata_and_response():
    recorder = RequestRecorder(capacity=4)
    record = make_record(recorder)

    recorder.finish(
        record,
        outcome="success",
        status_code=200,
        response={"choices": []},
        delay_ms=25,
    )

    result = recorder.query(request_id="request-1")
    assert len(result) == 1
    assert result[0]["sequence"] == 1
    assert result[0]["request_id"] == "request-1"
    assert result[0]["path"] == "/v1/chat/completions"
    assert result[0]["headers"]["x-request-id"] == "request-1"
    assert base64.b64decode(result[0]["raw_body_base64"]) == b'{"prompt":"hello"}'
    assert result[0]["parsed_json"] == {"prompt": "hello"}
    assert result[0]["pod"] == "mock-pod-0"
    assert result[0]["engine"] == "vllm"
    assert result[0]["role"] == "prefill"
    assert result[0]["outcome"] == "success"
    assert result[0]["status_code"] == 200
    assert result[0]["response"] == {"choices": []}
    assert result[0]["delay_ms"] == 25
    assert result[0]["timestamp_started"]
    assert result[0]["timestamp_finished"]


def test_sequence_is_monotonic_and_ring_buffer_evicts_oldest_record():
    recorder = RequestRecorder(capacity=2)
    first = make_record(recorder, request_id="request-1")
    recorder.finish(first, outcome="success", status_code=200)
    second = make_record(recorder, request_id="request-2")
    recorder.finish(second, outcome="rejected", status_code=400)
    third = make_record(recorder, request_id="request-3")
    recorder.finish(third, outcome="failed", status_code=500)

    result = recorder.query()
    assert [entry["sequence"] for entry in result] == [2, 3]
    assert recorder.query(request_id="request-1") == []


def test_query_returns_json_serializable_copies():
    recorder = RequestRecorder(capacity=2)
    record = make_record(recorder)
    recorder.finish(record, outcome="success", status_code=200)

    result = recorder.query(request_id="request-1")
    json.dumps(result)
    result[0]["headers"]["x-request-id"] = "mutated"
    assert recorder.query(request_id="request-1")[0]["headers"]["x-request-id"] == "request-1"


def test_query_sanitizes_non_json_values_to_json_serializable_values():
    recorder = RequestRecorder(capacity=2)
    record = recorder.start(
        path="/v1/chat/completions",
        headers={"x-request-id": "request-1", "x-test": {"nested", "set"}},
        raw_body=b"{}",
        parsed_json={"values": {1, 2}},
        pod="mock-pod-0",
        engine="vllm",
        role="decode",
    )
    recorder.finish(
        record,
        outcome="failed",
        status_code=500,
        error=RuntimeError("backend failed"),
        response={"unserializable": {"value"}},
    )

    result = recorder.query()
    json.dumps(result)
    assert isinstance(result[0]["parsed_json"]["values"], list)
    assert result[0]["error"] == "backend failed"
    assert result[0]["response"]["unserializable"] == ["value"]


def test_real_threading_lock_inputs_do_not_break_recording():
    recorder = RequestRecorder(capacity=2)
    input_lock = threading.Lock()
    record = recorder.start(
        path="/v1/chat/completions",
        headers={"x-request-id": "request-lock", "lock": input_lock},
        raw_body=b"{}",
        parsed_json={"lock": input_lock},
        pod="mock-pod-0",
        engine="vllm",
        role="prefill",
    )
    recorder.finish(
        record,
        outcome="failed",
        status_code=500,
        error=input_lock,
        response={"lock": input_lock},
    )

    result = recorder.query(request_id="request-lock")
    json.dumps(result)
    assert result[0]["headers"]["lock"] == str(input_lock)
    assert result[0]["parsed_json"]["lock"] == str(input_lock)
    assert result[0]["error"] == str(input_lock)
    assert result[0]["response"]["lock"] == str(input_lock)


def test_non_finite_floats_become_json_null():
    recorder = RequestRecorder(capacity=2)
    record = recorder.start(
        path="/v1/chat/completions",
        headers={"x-request-id": "request-floats", "nan": math.nan},
        raw_body=b"{}",
        parsed_json={"positive_inf": math.inf, "negative_inf": -math.inf},
        pod="mock-pod-0",
        engine="vllm",
        role="prefill",
    )
    recorder.finish(
        record,
        outcome="success",
        status_code=200,
        response={"nan": math.nan, "inf": math.inf},
    )

    result = recorder.query(request_id="request-floats")
    json.dumps(result, allow_nan=False)
    assert result[0]["headers"]["nan"] is None
    assert result[0]["parsed_json"] == {"positive_inf": None, "negative_inf": None}
    assert result[0]["response"] == {"nan": None, "inf": None}


def test_malformed_json_can_be_recorded_with_null_parsed_json():
    recorder = RequestRecorder(capacity=2)
    record = recorder.start(
        path="/v1/chat/completions",
        headers={},
        raw_body=b"not-json",
        parsed_json=None,
        pod="mock-pod-0",
        engine="vllm",
        role=None,
    )
    recorder.finish(record, outcome="rejected", status_code=400)

    result = recorder.query()
    assert result[0]["request_id"] is None
    assert base64.b64decode(result[0]["raw_body_base64"]) == b"not-json"
    assert result[0]["parsed_json"] is None
    assert result[0]["outcome"] == "rejected"


def test_concurrent_starts_finishes_and_queries_have_unique_monotonic_sequences():
    recorder = RequestRecorder(capacity=128)

    def create_request(index):
        record = make_record(recorder, request_id=f"request-{index}")
        recorder.query(request_id=f"request-{index}")
        recorder.finish(record, outcome="success", status_code=200)
        recorder.query()

    with ThreadPoolExecutor(max_workers=8) as executor:
        list(executor.map(create_request, range(32)))

    sequences = [entry["sequence"] for entry in recorder.query()]
    assert sequences == list(range(1, 33))
    assert len(set(sequences)) == 32


def test_finish_rejects_unknown_outcome():
    recorder = RequestRecorder(capacity=2)
    record = make_record(recorder)

    with pytest.raises(ValueError, match="outcome"):
        recorder.finish(record, outcome="delayed", status_code=200)


def test_finish_rejects_handle_from_another_recorder():
    owner = RequestRecorder(capacity=2)
    other = RequestRecorder(capacity=2)
    record = make_record(owner)

    with pytest.raises(ValueError, match="recorder"):
        other.finish(record, outcome="success", status_code=200)


def test_finish_rejects_forged_handle_with_matching_sequence():
    recorder = RequestRecorder(capacity=2)
    record = make_record(recorder)
    forged = _RecordHandle(record.sequence)

    with pytest.raises(ValueError, match="recorder"):
        recorder.finish(forged, outcome="success", status_code=200)

    assert recorder.query()[0]["outcome"] is None


def test_finish_is_idempotent_by_rejecting_duplicate_completion():
    recorder = RequestRecorder(capacity=2)
    record = make_record(recorder)
    recorder.finish(record, outcome="success", status_code=200, response={"ok": True})
    first_result = recorder.query()[0]

    with pytest.raises(RuntimeError, match="already finished"):
        recorder.finish(record, outcome="failed", status_code=500, response={"ok": False})

    assert recorder.query()[0] == first_result
