import json
import logging
import openai
import threading
from typing import List, Any, Dict, Optional

# Matches the gateway's caller-owned session affinity header. Keep in sync
# with HeaderSessionKey in pkg/plugins/gateway/types.go.
AIBRIX_SESSION_KEY_HEADER = "x-aibrix-session-key"

# The gateway drops session keys longer than this and silently falls back to
# load-balanced routing. Keep in sync with maxSessionKeyLen in
# pkg/plugins/gateway/algorithms/simple_session_affinity.go.
GATEWAY_MAX_SESSION_KEY_LEN = 256

# Session ids already warned about, so an over-long id logs once per session
# rather than once per request.
_warned_session_keys = set()

def load_workload(input_path: str) -> List[Any]:
    load_struct = None
    if input_path.endswith(".jsonl"):
        with open(input_path, "r") as file:
            load_struct = [json.loads(line) for line in file]
    else:
        with open(input_path, "r") as file:
            load_struct = json.load(file)
    return load_struct

def session_key_headers(session_id: Optional[Any], enabled: bool) -> Optional[Dict[str, str]]:
    """
    Build per-request headers carrying the workload session identity.

    When enabled and the request has a session_id, returns the
    x-aibrix-session-key header so the gateway's session-affinity routing
    can be exercised during replay. Returns None otherwise, so the request
    goes out without extra headers (existing behavior).

    :param session_id: The session identifier from the workload request, or None.
    :param enabled: Whether session key header injection is enabled.
    :return: A headers dict or None.
    """
    if not enabled or session_id is None:
        return None
    session_key = str(session_id)
    # The gateway measures the key in bytes (Go len()); mirror that here.
    if len(session_key.encode("utf-8")) > GATEWAY_MAX_SESSION_KEY_LEN and session_key not in _warned_session_keys:
        _warned_session_keys.add(session_key)
        logging.warning(
            f"Session key exceeds the gateway's {GATEWAY_MAX_SESSION_KEY_LEN}-byte limit and will be "
            f"ignored by session-affinity routing (requests fall back to load-balanced routing): "
            f"{session_key[:64]}..."
        )
    return {AIBRIX_SESSION_KEY_HEADER: session_key}

# Function to wrap the prompt into OpenAI's chat completion message format.
def prepare_prompt(prompt: str,
                   session_id: str = None, 
                   history: Dict = None,
                   history_lock: threading.Lock = None) -> List[Dict]:
    """
    Wrap the prompt into OpenAI's chat completion message format.

    :param prompt: The user prompt to be converted.
    :param session_id: Optional session ID for conversation history.
    :param history: Optional history dictionary to store conversation.
    :param history_lock: Optional threading lock for thread safety.
    :return: A list containing chat completion messages.
    """
    if session_id is not None and history is not None:
        if history_lock:
            with history_lock:
                past_history = history.get(session_id, [])
                user_message = {"role": "user", "content": f"{prompt}"}
                past_history.append(user_message) 
                history[session_id] = past_history
                return list(past_history)
        else:
            # Fallback for when no lock is provided (not thread-safe)
            past_history = history.get(session_id, [])
            user_message = {"role": "user", "content": f"{prompt}"}
            past_history.append(user_message) 
            history[session_id] = past_history
            return list(past_history)
    else:    
        user_message = {"role": "user", "content": prompt}
        return [user_message]
    
def update_response(response: str, 
                    session_id: str = None, 
                    history: Dict = None,
                    history_lock: threading.Lock = None):
    """
    Update the conversation history with the assistant's response.

    :param response: The assistant's response to add to history.
    :param session_id: Optional session ID for conversation history.
    :param history: Optional history dictionary to store conversation.
    :param history_lock: Optional threading lock for thread safety.
    """
    if session_id is not None and history is not None:
        if history_lock:
            with history_lock:
                past_history = history.get(session_id, [])
                assistant_message = {"role": "assistant", "content": f"{response}"}
                past_history.append(assistant_message) 
        else:
            # Fallback for when no lock is provided (not thread-safe)
            past_history = history.get(session_id, [])
            assistant_message = {"role": "assistant", "content": f"{response}"}
            past_history.append(assistant_message)

def create_client(api_key: str,
                  endpoint: str,
                  max_retries: int,
                  timeout: float,
                  routing_strategy: str,
                  ):
    if api_key is None:
        client = openai.AsyncOpenAI(
            base_url=endpoint + "/v1",
            max_retries=max_retries,
            timeout=timeout,
        )
    else:
        client = openai.AsyncOpenAI(
            api_key=api_key,
            base_url=endpoint + "/v1",
            max_retries=max_retries,
            timeout=timeout,
        )
    if routing_strategy is not None:
        client = client.with_options(
            default_headers={"routing-strategy": routing_strategy}
        )
    return client