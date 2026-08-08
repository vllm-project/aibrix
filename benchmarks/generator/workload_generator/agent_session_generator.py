"""Synthetic, privacy-preserving Agent session workload generation.

The existing AIBrix workload client consumes ``timestamp`` and ``requests``.
Agent workloads need additional structure (tools, retries, waits, and joins),
so this module adds an ``events`` side channel while keeping ``requests``
backward compatible with the existing client.
"""

from __future__ import annotations

import hashlib
import random
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Mapping, Optional, Tuple


SCHEMA_VERSION = "aibrix.agent-session.v1"
PromptFactory = Callable[[int, str, str], Tuple[str, int]]


@dataclass(frozen=True)
class IntRange:
    minimum: int
    maximum: int

    @classmethod
    def from_value(
        cls, value: Any, field_name: str, *, allow_zero: bool = True
    ) -> "IntRange":
        if isinstance(value, int) and not isinstance(value, bool):
            minimum = maximum = value
        elif (
            isinstance(value, (list, tuple))
            and len(value) == 2
            and all(
                isinstance(item, int) and not isinstance(item, bool) for item in value
            )
        ):
            minimum, maximum = value
        else:
            raise ValueError(
                f"{field_name} must be an integer or a two-item integer range"
            )

        lower_bound = 0 if allow_zero else 1
        if minimum < lower_bound or maximum < minimum:
            raise ValueError(f"{field_name} must satisfy {lower_bound} <= min <= max")
        return cls(minimum=minimum, maximum=maximum)

    def sample(self, rng: random.Random) -> int:
        return rng.randint(self.minimum, self.maximum)


@dataclass(frozen=True)
class AgentSessionConfig:
    num_sessions: int
    session_arrival_rate_rps: float
    turns_per_session: IntRange
    parallel_tools: IntRange
    tool_latency_ms: IntRange
    model_latency_ms: IntRange
    think_time_ms: IntRange
    retry_probability: float
    max_tool_retries: int
    initial_prompt_tokens: int
    context_growth_tokens: int
    tool_result_tokens: int
    output_tokens: int
    seed: int

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> "AgentSessionConfig":
        num_sessions = _positive_int(raw.get("num_sessions", 10), "num_sessions")
        session_arrival_rate_rps = raw.get("session_arrival_rate_rps", 1.0)
        if (
            isinstance(session_arrival_rate_rps, bool)
            or not isinstance(session_arrival_rate_rps, (int, float))
            or session_arrival_rate_rps <= 0
        ):
            raise ValueError("session_arrival_rate_rps must be greater than zero")

        retry_probability = raw.get("retry_probability", 0.0)
        if (
            isinstance(retry_probability, bool)
            or not isinstance(retry_probability, (int, float))
            or not 0 <= retry_probability <= 1
        ):
            raise ValueError("retry_probability must be between zero and one")

        seed = raw.get("seed", 0)
        if not isinstance(seed, int) or isinstance(seed, bool):
            raise ValueError("seed must be an integer")

        return cls(
            num_sessions=num_sessions,
            session_arrival_rate_rps=float(session_arrival_rate_rps),
            turns_per_session=IntRange.from_value(
                raw.get("turns_per_session", 3),
                "turns_per_session",
                allow_zero=False,
            ),
            parallel_tools=IntRange.from_value(
                raw.get("parallel_tools", 2),
                "parallel_tools",
                allow_zero=False,
            ),
            tool_latency_ms=IntRange.from_value(
                raw.get("tool_latency_ms", [100, 500]), "tool_latency_ms"
            ),
            model_latency_ms=IntRange.from_value(
                raw.get("model_latency_ms", [50, 150]), "model_latency_ms"
            ),
            think_time_ms=IntRange.from_value(
                raw.get("think_time_ms", [0, 1000]), "think_time_ms"
            ),
            retry_probability=float(retry_probability),
            max_tool_retries=_non_negative_int(
                raw.get("max_tool_retries", 1), "max_tool_retries"
            ),
            initial_prompt_tokens=_positive_int(
                raw.get("initial_prompt_tokens", 256), "initial_prompt_tokens"
            ),
            context_growth_tokens=_non_negative_int(
                raw.get("context_growth_tokens", 128), "context_growth_tokens"
            ),
            tool_result_tokens=_non_negative_int(
                raw.get("tool_result_tokens", 64), "tool_result_tokens"
            ),
            output_tokens=_positive_int(raw.get("output_tokens", 64), "output_tokens"),
            seed=seed,
        )


def generate_agent_session_workload(
    raw_config: Mapping[str, Any],
    prompt_factory: Optional[PromptFactory] = None,
) -> List[Dict[str, Any]]:
    """Generate a workload containing replayable model calls and Agent DAG events.

    Tool and control-flow nodes are emitted in ``events``. Model nodes are also
    projected into ``requests`` so the output remains consumable by the current
    AIBrix benchmark client.
    """

    config = AgentSessionConfig.from_dict(raw_config)
    rng = random.Random(config.seed)
    prompt_factory = prompt_factory or _default_prompt_factory
    timeline: Dict[int, Dict[str, Any]] = {}

    session_start_ms = 0
    for session_index in range(config.num_sessions):
        if session_index:
            inter_arrival_seconds = rng.expovariate(config.session_arrival_rate_rps)
            session_start_ms += max(1, round(inter_arrival_seconds * 1000))

        session_id = _anonymous_id(config.seed, "session", session_index)
        task_id = _anonymous_id(config.seed, "task", session_index)
        state_ref = _anonymous_id(config.seed, "state", session_index)
        turn_count = config.turns_per_session.sample(rng)
        turn_start_ms = session_start_ms
        previous_node: Optional[str] = None

        for turn in range(turn_count):
            plan_node = _node_id(config.seed, session_index, turn, "plan")
            plan_parents = [previous_node] if previous_node else []
            plan_prompt_tokens = (
                config.initial_prompt_tokens + turn * config.context_growth_tokens
            )
            plan_latency_ms = config.model_latency_ms.sample(rng)
            _add_model_request(
                timeline=timeline,
                timestamp=turn_start_ms,
                session_id=session_id,
                task_id=task_id,
                node_id=plan_node,
                parent_ids=plan_parents,
                turn=turn,
                phase="plan",
                state_ref=state_ref,
                prompt_tokens=plan_prompt_tokens,
                output_tokens=config.output_tokens,
                expected_latency_ms=plan_latency_ms,
                context_delta_tokens=(
                    config.initial_prompt_tokens
                    if turn == 0
                    else config.context_growth_tokens
                ),
                prompt_factory=prompt_factory,
            )

            tool_start_ms = turn_start_ms + plan_latency_ms
            join_parents: List[str] = []
            join_timestamp = tool_start_ms

            tool_count = config.parallel_tools.sample(rng)
            for tool_index in range(tool_count):
                parent_id = plan_node
                attempt = 0
                attempt_start_ms = tool_start_ms
                while True:
                    is_retry = attempt > 0
                    node_kind = "retry" if is_retry else "tool"
                    tool_node = _node_id(
                        config.seed,
                        session_index,
                        turn,
                        f"tool-{tool_index}-{attempt}",
                    )
                    latency_ms = config.tool_latency_ms.sample(rng)
                    failed = (
                        rng.random() < config.retry_probability
                        and attempt < config.max_tool_retries
                    )
                    _add_event(
                        timeline,
                        attempt_start_ms,
                        {
                            "schema_version": SCHEMA_VERSION,
                            "session_id": session_id,
                            "task_id": task_id,
                            "node_id": tool_node,
                            "parent_ids": [parent_id],
                            "node_type": node_kind,
                            "turn": turn,
                            "branch": tool_index,
                            "attempt": attempt,
                            "duration_ms": latency_ms,
                            "status": "failed" if failed else "succeeded",
                            "side_effects": False,
                            "state_refs": [state_ref],
                            "context_delta_tokens": (
                                0 if failed else config.tool_result_tokens
                            ),
                        },
                    )
                    completion_ms = attempt_start_ms + latency_ms
                    join_timestamp = max(join_timestamp, completion_ms)
                    parent_id = tool_node
                    if not failed:
                        join_parents.append(tool_node)
                        break
                    attempt += 1
                    attempt_start_ms = completion_ms

            join_node = _node_id(config.seed, session_index, turn, "join")
            _add_event(
                timeline,
                join_timestamp,
                {
                    "schema_version": SCHEMA_VERSION,
                    "session_id": session_id,
                    "task_id": task_id,
                    "node_id": join_node,
                    "parent_ids": join_parents,
                    "node_type": "join",
                    "turn": turn,
                    "duration_ms": 0,
                    "state_refs": [state_ref],
                    "context_delta_tokens": 0,
                },
            )

            followup_node = _node_id(config.seed, session_index, turn, "followup")
            followup_prompt_tokens = (
                plan_prompt_tokens + tool_count * config.tool_result_tokens
            )
            followup_latency_ms = config.model_latency_ms.sample(rng)
            _add_model_request(
                timeline=timeline,
                timestamp=join_timestamp,
                session_id=session_id,
                task_id=task_id,
                node_id=followup_node,
                parent_ids=[join_node],
                turn=turn,
                phase="followup",
                state_ref=state_ref,
                prompt_tokens=followup_prompt_tokens,
                output_tokens=config.output_tokens,
                expected_latency_ms=followup_latency_ms,
                context_delta_tokens=0,
                prompt_factory=prompt_factory,
            )
            previous_node = followup_node

            if turn + 1 < turn_count:
                wait_node = _node_id(config.seed, session_index, turn, "wait")
                wait_ms = config.think_time_ms.sample(rng)
                wait_start_ms = join_timestamp + followup_latency_ms
                _add_event(
                    timeline,
                    wait_start_ms,
                    {
                        "schema_version": SCHEMA_VERSION,
                        "session_id": session_id,
                        "task_id": task_id,
                        "node_id": wait_node,
                        "parent_ids": [followup_node],
                        "node_type": "wait",
                        "turn": turn,
                        "duration_ms": wait_ms,
                        "state_refs": [state_ref],
                        "context_delta_tokens": 0,
                    },
                )
                turn_start_ms = wait_start_ms + wait_ms
                previous_node = wait_node

    return [timeline[timestamp] for timestamp in sorted(timeline)]


def _add_model_request(
    *,
    timeline: Dict[int, Dict[str, Any]],
    timestamp: int,
    session_id: str,
    task_id: str,
    node_id: str,
    parent_ids: List[str],
    turn: int,
    phase: str,
    state_ref: str,
    prompt_tokens: int,
    output_tokens: int,
    expected_latency_ms: int,
    context_delta_tokens: int,
    prompt_factory: PromptFactory,
) -> None:
    prompt, actual_prompt_tokens = prompt_factory(prompt_tokens, session_id, node_id)
    request = {
        "prompt": prompt,
        "prompt_length": actual_prompt_tokens,
        "output_length": output_tokens,
        "session_id": session_id,
        "agent": {
            "schema_version": SCHEMA_VERSION,
            "task_id": task_id,
            "node_id": node_id,
            "parent_ids": parent_ids,
            "node_type": "model",
            "turn": turn,
            "phase": phase,
            "expected_latency_ms": expected_latency_ms,
            "state_refs": [state_ref],
            "context_delta_tokens": context_delta_tokens,
        },
    }
    _entry(timeline, timestamp)["requests"].append(request)


def _add_event(
    timeline: Dict[int, Dict[str, Any]], timestamp: int, event: Dict[str, Any]
) -> None:
    _entry(timeline, timestamp)["events"].append(event)


def _entry(timeline: Dict[int, Dict[str, Any]], timestamp: int) -> Dict[str, Any]:
    return timeline.setdefault(
        timestamp,
        {
            "schema_version": SCHEMA_VERSION,
            "timestamp": timestamp,
            "requests": [],
            "events": [],
        },
    )


def _anonymous_id(seed: int, kind: str, index: int) -> str:
    digest = hashlib.sha256(f"{seed}:{kind}:{index}".encode()).hexdigest()[:16]
    return f"{kind}_{digest}"


def _node_id(seed: int, session: int, turn: int, label: str) -> str:
    digest = hashlib.sha256(
        f"{seed}:node:{session}:{turn}:{label}".encode()
    ).hexdigest()[:16]
    return f"node_{digest}"


def _default_prompt_factory(
    target_tokens: int, session_id: str, node_id: str
) -> Tuple[str, int]:
    prefix = (
        f"Synthetic Agent request for {session_id} and {node_id}; "
        "no production content is included. "
    )
    words = prefix.split()
    filler = ["synthetic", "agent", "context"]
    if len(words) < target_tokens:
        needed = target_tokens - len(words)
        repeats = (needed + len(filler) - 1) // len(filler)
        words.extend(filler * repeats)
    prompt = " ".join(words[:target_tokens])
    return prompt, target_tokens


def _positive_int(value: Any, field_name: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise ValueError(f"{field_name} must be a positive integer")
    return value


def _non_negative_int(value: Any, field_name: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise ValueError(f"{field_name} must be a non-negative integer")
    return value
