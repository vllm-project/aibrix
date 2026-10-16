import json
from typing import Optional

from aibrix.batch.job_entity import BatchJobEndpoint
from aibrix.logger import init_logger
from aibrix.storage.base import BaseStorage

logger = init_logger(__name__)

MAX_BATCH_REQUESTS = 50000
REQUIRED_FIELDS = ["custom_id", "method", "url", "body"]
VALID_HTTP_METHODS = {"GET", "POST", "PUT", "DELETE", "PATCH"}
ENDPOINT_REQUIRED_BODY_FIELDS: dict[str, list[str]] = {
    BatchJobEndpoint.CHAT_COMPLETIONS.value: ["model", "messages"],
    BatchJobEndpoint.COMPLETIONS.value: ["model", "prompt"],
    BatchJobEndpoint.EMBEDDINGS.value: ["model", "input"],
    BatchJobEndpoint.RERANK.value: ["model", "query", "documents"],
}


def validate_request_body_for_endpoint(
    body: dict, endpoint: str, line_num: int
) -> Optional[str]:
    required_fields = ENDPOINT_REQUIRED_BODY_FIELDS.get(endpoint)
    if required_fields is None:
        return None

    for field in required_fields:
        if field not in body:
            return (
                f"Line {line_num}: Request body for endpoint '{endpoint}' "
                f"is missing required field '{field}'"
            )

    if endpoint == BatchJobEndpoint.CHAT_COMPLETIONS.value:
        if not isinstance(body.get("messages"), list):
            return f"Line {line_num}: 'messages' must be a list for {endpoint}"
    elif endpoint == BatchJobEndpoint.COMPLETIONS.value:
        prompt = body.get("prompt")
        if not isinstance(prompt, (str, list)):
            return f"Line {line_num}: 'prompt' must be a string or list for {endpoint}"
    elif endpoint == BatchJobEndpoint.EMBEDDINGS.value:
        input_val = body.get("input")
        if not isinstance(input_val, (str, list)):
            return f"Line {line_num}: 'input' must be a string or list for {endpoint}"
    elif endpoint == BatchJobEndpoint.RERANK.value:
        if not isinstance(body.get("query"), str):
            return f"Line {line_num}: 'query' must be a string for {endpoint}"
        if not isinstance(body.get("documents"), list):
            return f"Line {line_num}: 'documents' must be a list for {endpoint}"

    return None


async def validate_batch_input_file(
    storage: BaseStorage, file_id: str, endpoint: str
) -> tuple[int, Optional[str]]:
    try:
        request_count = 0
        line_num = 0

        async for line in storage.readline_iter(file_id):
            line_num += 1
            line_stripped = line.strip()

            if not line_stripped:
                continue

            request_count += 1

            if request_count > MAX_BATCH_REQUESTS:
                return (
                    request_count,
                    f"Batch input contains more than {MAX_BATCH_REQUESTS} requests",
                )

            try:
                request = json.loads(line_stripped)
            except json.JSONDecodeError as e:
                return 0, f"Line {line_num}: Invalid JSON - {str(e)}"

            for field in REQUIRED_FIELDS:
                if field not in request:
                    return 0, f"Line {line_num}: Missing required field '{field}'"

            if not isinstance(request.get("custom_id"), str):
                return 0, f"Line {line_num}: 'custom_id' must be a string"

            if not isinstance(request.get("method"), str):
                return 0, f"Line {line_num}: 'method' must be a string"

            if not isinstance(request.get("url"), str):
                return 0, f"Line {line_num}: 'url' must be a string"

            if not isinstance(request.get("body"), dict):
                return 0, f"Line {line_num}: 'body' must be an object"

            if request["method"].upper() not in VALID_HTTP_METHODS:
                return (
                    0,
                    f"Line {line_num}: Invalid HTTP method '{request['method']}'",
                )

            request_url = request["url"]
            if endpoint and not request_url.endswith(endpoint):
                return (
                    0,
                    f"Line {line_num}: Request URL '{request_url}' does not match "
                    f"batch endpoint '{endpoint}'",
                )

            body_error = validate_request_body_for_endpoint(
                request["body"], endpoint, line_num
            )
            if body_error:
                return 0, body_error

        if request_count == 0:
            return 0, "Batch input file is empty or contains only empty lines"

        return request_count, None

    except FileNotFoundError:
        return 0, f"Input file '{file_id}' not found"
    except Exception as e:
        logger.error("Error validating batch input", file_id=file_id, error=str(e))  # type: ignore[call-arg]
        return 0, f"Failed to validate input file: {str(e)}"
