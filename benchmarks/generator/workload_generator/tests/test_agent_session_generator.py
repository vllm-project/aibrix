import unittest

from generator.workload_generator.agent_session_generator import (
    SCHEMA_VERSION,
    AgentSessionConfig,
    generate_agent_session_workload,
)


def fake_prompt_factory(target_tokens, session_id, node_id):
    return f"prompt:{session_id}:{node_id}", target_tokens


class AgentSessionGeneratorTest(unittest.TestCase):
    def setUp(self):
        self.config = {
            "num_sessions": 2,
            "session_arrival_rate_rps": 2,
            "turns_per_session": 2,
            "parallel_tools": 2,
            "tool_latency_ms": [10, 20],
            "model_latency_ms": [5, 10],
            "think_time_ms": [1, 3],
            "retry_probability": 0,
            "max_tool_retries": 1,
            "initial_prompt_tokens": 100,
            "context_growth_tokens": 20,
            "tool_result_tokens": 10,
            "output_tokens": 16,
            "seed": 7,
        }

    def test_generation_is_deterministic(self):
        first = generate_agent_session_workload(
            self.config, prompt_factory=fake_prompt_factory
        )
        second = generate_agent_session_workload(
            self.config, prompt_factory=fake_prompt_factory
        )
        self.assertEqual(first, second)

    def test_output_stays_compatible_with_existing_client(self):
        workload = generate_agent_session_workload(
            self.config, prompt_factory=fake_prompt_factory
        )
        self.assertEqual(
            [entry["timestamp"] for entry in workload],
            sorted(entry["timestamp"] for entry in workload),
        )
        self.assertTrue(any(entry["events"] for entry in workload))

        requests = [request for entry in workload for request in entry["requests"]]
        self.assertEqual(len(requests), 2 * 2 * 2)
        for request in requests:
            self.assertIn("prompt", request)
            self.assertIn("prompt_length", request)
            self.assertIn("output_length", request)
            self.assertIn("session_id", request)
            self.assertEqual(request["agent"]["schema_version"], SCHEMA_VERSION)

    def test_graph_parents_exist_and_identifiers_are_anonymous(self):
        workload = generate_agent_session_workload(
            self.config, prompt_factory=fake_prompt_factory
        )
        nodes = {}
        for entry in workload:
            for request in entry["requests"]:
                nodes[request["agent"]["node_id"]] = request["agent"]
                self.assertTrue(request["session_id"].startswith("session_"))
            for event in entry["events"]:
                nodes[event["node_id"]] = event
                self.assertTrue(event["session_id"].startswith("session_"))

        for node in nodes.values():
            for parent_id in node["parent_ids"]:
                self.assertIn(parent_id, nodes)

    def test_retry_nodes_are_emitted(self):
        config = dict(self.config)
        config.update(
            {
                "num_sessions": 1,
                "turns_per_session": 1,
                "parallel_tools": 1,
                "retry_probability": 1,
                "max_tool_retries": 2,
            }
        )
        workload = generate_agent_session_workload(
            config, prompt_factory=fake_prompt_factory
        )
        events = [event for entry in workload for event in entry["events"]]
        retries = [event for event in events if event["node_type"] == "retry"]
        self.assertEqual(len(retries), 2)
        self.assertEqual(retries[-1]["status"], "succeeded")

    def test_context_grows_across_turns_and_tool_results(self):
        workload = generate_agent_session_workload(
            self.config, prompt_factory=fake_prompt_factory
        )
        requests = [
            request
            for entry in workload
            for request in entry["requests"]
            if request["session_id"]
            == next(
                candidate["session_id"]
                for candidate_entry in workload
                for candidate in candidate_entry["requests"]
            )
        ]
        lengths = [request["prompt_length"] for request in requests]
        self.assertEqual(lengths, [100, 120, 120, 140])
        tool_events = [
            event
            for entry in workload
            for event in entry["events"]
            if event["node_type"] in {"tool", "retry"}
            and event["status"] == "succeeded"
        ]
        join_events = [
            event
            for entry in workload
            for event in entry["events"]
            if event["node_type"] == "join"
        ]
        self.assertTrue(
            all(event["context_delta_tokens"] == 10 for event in tool_events)
        )
        self.assertTrue(
            all(event["context_delta_tokens"] == 0 for event in join_events)
        )

    def test_invalid_config_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "session_arrival_rate_rps"):
            AgentSessionConfig.from_dict(
                {"num_sessions": 1, "session_arrival_rate_rps": 0}
            )


if __name__ == "__main__":
    unittest.main()
