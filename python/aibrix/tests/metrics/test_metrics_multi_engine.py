# Copyright 2025 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from typing import Union

import pytest
from prometheus_client.core import CounterMetricFamily, GaugeMetricFamily

from aibrix.metrics.engine_rules import get_metric_standard_rules
from aibrix.metrics.standard_rules import PassthroughStandardRule, RenameStandardRule


class TestMultiEngineMetricsSupport:
    """Test multi-engine metrics support with hybrid mapping strategy."""

    def test_get_metric_rules_vllm(self):
        """Test vLLM metrics rules are returned correctly."""
        rules = get_metric_standard_rules("vllm")

        # Check core performance metrics exist
        assert "vllm:num_requests_waiting" in rules
        assert isinstance(rules["vllm:num_requests_waiting"], RenameStandardRule)
        assert rules["vllm:num_requests_waiting"].new_name == "aibrix:queue_size"

        assert "vllm:gpu_cache_usage_perc" in rules
        assert (
            rules["vllm:gpu_cache_usage_perc"].new_name == "aibrix:gpu_cache_usage_perc"
        )

        # Check token processing metrics exist
        assert "vllm:prompt_tokens_total" in rules
        assert (
            rules["vllm:prompt_tokens_total"].new_name == "aibrix:prompt_tokens_total"
        )

        assert "vllm:generation_tokens_total" in rules
        assert (
            rules["vllm:generation_tokens_total"].new_name
            == "aibrix:generation_tokens_total"
        )

        # Check latency metrics exist
        assert "vllm:time_to_first_token_seconds" in rules
        assert (
            rules["vllm:time_to_first_token_seconds"].new_name
            == "aibrix:time_to_first_token_seconds"
        )

        assert "vllm:time_per_output_token_seconds" in rules
        assert (
            rules["vllm:time_per_output_token_seconds"].new_name
            == "aibrix:time_per_output_token_seconds"
        )

        assert "vllm:e2e_request_latency_seconds" in rules
        assert (
            rules["vllm:e2e_request_latency_seconds"].new_name
            == "aibrix:e2e_request_latency_seconds"
        )

        # Check success metrics exist
        assert "vllm:request_success_total" in rules
        assert (
            rules["vllm:request_success_total"].new_name
            == "aibrix:request_success_total"
        )

        # Check pass-through metrics exist
        assert "vllm:num_requests_running" in rules
        assert isinstance(rules["vllm:num_requests_running"], PassthroughStandardRule)

    def test_get_metric_rules_sglang(self):
        """Test SGLang metrics rules are returned correctly."""
        rules = get_metric_standard_rules("sglang")

        # Check core performance metrics exist
        assert "sglang:num_queue_reqs" in rules
        assert isinstance(rules["sglang:num_queue_reqs"], RenameStandardRule)
        assert rules["sglang:num_queue_reqs"].new_name == "aibrix:queue_size"

        assert "sglang:gen_throughput" in rules
        assert rules["sglang:gen_throughput"].new_name == "aibrix:generation_throughput"

        # Check token processing metrics exist
        assert "sglang:prompt_tokens_total" in rules
        assert (
            rules["sglang:prompt_tokens_total"].new_name == "aibrix:prompt_tokens_total"
        )

        assert "sglang:generation_tokens_total" in rules
        assert (
            rules["sglang:generation_tokens_total"].new_name
            == "aibrix:generation_tokens_total"
        )

        # Check latency metrics exist
        assert "sglang:time_to_first_token_seconds" in rules
        assert (
            rules["sglang:time_to_first_token_seconds"].new_name
            == "aibrix:time_to_first_token_seconds"
        )

        assert "sglang:time_per_output_token_seconds" in rules
        assert (
            rules["sglang:time_per_output_token_seconds"].new_name
            == "aibrix:time_per_output_token_seconds"
        )

        assert "sglang:e2e_request_latency_seconds" in rules
        assert (
            rules["sglang:e2e_request_latency_seconds"].new_name
            == "aibrix:e2e_request_latency_seconds"
        )

        # Check cache and utilization metrics exist
        assert "sglang:cache_hit_rate" in rules
        assert rules["sglang:cache_hit_rate"].new_name == "aibrix:cache_hit_rate"

        assert "sglang:token_usage" in rules
        assert rules["sglang:token_usage"].new_name == "aibrix:token_usage"

        # Check pass-through metrics exist
        assert "sglang:num_running_reqs" in rules
        assert isinstance(rules["sglang:num_running_reqs"], PassthroughStandardRule)

        assert "sglang:func_latency_seconds" in rules
        assert isinstance(rules["sglang:func_latency_seconds"], PassthroughStandardRule)

    def test_get_metric_rules_case_insensitive(self):
        """Test engine names are case insensitive."""
        vllm_lower = get_metric_standard_rules("vllm")
        vllm_upper = get_metric_standard_rules("VLLM")
        vllm_mixed = get_metric_standard_rules("vLLM")

        assert vllm_lower.keys() == vllm_upper.keys() == vllm_mixed.keys()

        sglang_lower = get_metric_standard_rules("sglang")
        sglang_upper = get_metric_standard_rules("SGLANG")
        sglang_mixed = get_metric_standard_rules("SGLang")

        assert sglang_lower.keys() == sglang_upper.keys() == sglang_mixed.keys()

    def test_get_metric_rules_unsupported_engine(self):
        """Test unsupported engines raise ValueError with helpful message."""
        with pytest.raises(ValueError) as exc_info:
            get_metric_standard_rules("tensorrt-llm")

        assert "tensorrt-llm" in str(exc_info.value)
        assert "Supported engines: vllm, sglang" in str(exc_info.value)

    def test_core_metrics_standardization(self):
        """Test that core metrics from both engines map to same aibrix namespace."""
        vllm_rules = get_metric_standard_rules("vllm")
        sglang_rules = get_metric_standard_rules("sglang")

        # Both should map queue metrics to same target
        vllm_queue_rule = vllm_rules["vllm:num_requests_waiting"]
        sglang_queue_rule = sglang_rules["sglang:num_queue_reqs"]
        assert (
            vllm_queue_rule.new_name
            == sglang_queue_rule.new_name
            == "aibrix:queue_size"
        )

        # Both should map token processing metrics to same targets
        vllm_prompt_rule = vllm_rules["vllm:prompt_tokens_total"]
        sglang_prompt_rule = sglang_rules["sglang:prompt_tokens_total"]
        assert (
            vllm_prompt_rule.new_name
            == sglang_prompt_rule.new_name
            == "aibrix:prompt_tokens_total"
        )

        vllm_gen_rule = vllm_rules["vllm:generation_tokens_total"]
        sglang_gen_rule = sglang_rules["sglang:generation_tokens_total"]
        assert (
            vllm_gen_rule.new_name
            == sglang_gen_rule.new_name
            == "aibrix:generation_tokens_total"
        )

        # Both should map latency metrics to same targets
        vllm_ttft_rule = vllm_rules["vllm:time_to_first_token_seconds"]
        sglang_ttft_rule = sglang_rules["sglang:time_to_first_token_seconds"]
        assert (
            vllm_ttft_rule.new_name
            == sglang_ttft_rule.new_name
            == "aibrix:time_to_first_token_seconds"
        )

        vllm_tpot_rule = vllm_rules["vllm:time_per_output_token_seconds"]
        sglang_tpot_rule = sglang_rules["sglang:time_per_output_token_seconds"]
        assert (
            vllm_tpot_rule.new_name
            == sglang_tpot_rule.new_name
            == "aibrix:time_per_output_token_seconds"
        )

        vllm_e2e_rule = vllm_rules["vllm:e2e_request_latency_seconds"]
        sglang_e2e_rule = sglang_rules["sglang:e2e_request_latency_seconds"]
        assert (
            vllm_e2e_rule.new_name
            == sglang_e2e_rule.new_name
            == "aibrix:e2e_request_latency_seconds"
        )


class TestPassthroughStandardRule:
    """Test PassthroughStandardRule implementation."""

    @staticmethod
    def create_sample_metric(
        name: str, value: float = 1.0, metric_type: str = "counter"
    ) -> Union[CounterMetricFamily, GaugeMetricFamily]:
        """Create sample metric for testing."""
        metric: Union[CounterMetricFamily, GaugeMetricFamily]
        if metric_type == "counter":
            metric = CounterMetricFamily(name, f"Test counter metric for {name}")
        elif metric_type == "gauge":
            metric = GaugeMetricFamily(name, f"Test gauge metric for {name}")
        else:
            raise ValueError(f"Unsupported metric type: {metric_type}")
        metric.add_metric(labels=[], value=value)
        return metric

    def test_passthrough_unchanged(self):
        """Test PassthroughStandardRule returns metric unchanged."""
        original_metric = self.create_sample_metric("test_metric", 42.0, "gauge")
        rule = PassthroughStandardRule("test_metric")

        result = list(rule(original_metric))

        assert len(result) == 1
        returned_metric = result[0]

        # Metric should be identical to original
        assert returned_metric.name == original_metric.name == "test_metric"
        assert returned_metric.documentation == original_metric.documentation
        assert len(returned_metric.samples) == len(original_metric.samples) == 1
        assert (
            returned_metric.samples[0].value == original_metric.samples[0].value == 42.0
        )

    def test_passthrough_name_mismatch_assertion(self):
        """Test PassthroughStandardRule raises assertion on name mismatch."""
        metric = self.create_sample_metric("wrong_name")
        rule = PassthroughStandardRule("expected_name")

        with pytest.raises(AssertionError) as exc_info:
            list(rule(metric))

        assert "does not match Rule metric name" in str(exc_info.value)

    def test_passthrough_with_multiple_samples(self):
        """Test PassthroughStandardRule handles multiple samples correctly."""
        metric = CounterMetricFamily("multi_sample", "Test multiple samples")
        metric.add_metric({"label": "value1"}, 10.0)
        metric.add_metric({"label": "value2"}, 20.0)

        rule = PassthroughStandardRule("multi_sample")
        result = list(rule(metric))

        assert len(result) == 1
        returned_metric = result[0]
        assert len(returned_metric.samples) == 2
        assert returned_metric.samples[0].value == 10.0
        assert returned_metric.samples[1].value == 20.0

    def test_passthrough_rule_type(self):
        """Test PassthroughStandardRule has correct rule type."""
        rule = PassthroughStandardRule("test")
        assert rule.rule_type == "PASSTHROUGH"


class TestHybridMappingStrategy:
    """Test the hybrid mapping strategy works as intended."""

    def test_vllm_hybrid_coverage(self):
        """Test vLLM rules cover both mapping and pass-through appropriately."""
        rules = get_metric_standard_rules("vllm")

        # Count mapping vs pass-through rules
        rename_rules = sum(
            1 for r in rules.values() if isinstance(r, RenameStandardRule)
        )
        passthrough_rules = sum(
            1 for r in rules.values() if isinstance(r, PassthroughStandardRule)
        )

        # Should have both types of rules
        assert rename_rules > 0, "Should have some renamed metrics for standardization"
        assert (
            passthrough_rules > 0
        ), "Should have some pass-through metrics for debugging"

        # Essential metrics should be renamed
        essential_mapped = [
            "vllm:num_requests_waiting",
            "vllm:gpu_cache_usage_perc",
            "vllm:prompt_tokens_total",
            "vllm:generation_tokens_total",
            "vllm:time_to_first_token_seconds",
            "vllm:time_per_output_token_seconds",
            "vllm:e2e_request_latency_seconds",
        ]
        for metric in essential_mapped:
            assert metric in rules
            assert isinstance(rules[metric], RenameStandardRule)

    def test_sglang_hybrid_coverage(self):
        """Test SGLang rules cover both mapping and pass-through appropriately."""
        rules = get_metric_standard_rules("sglang")

        # Count mapping vs pass-through rules
        rename_rules = sum(
            1 for r in rules.values() if isinstance(r, RenameStandardRule)
        )
        passthrough_rules = sum(
            1 for r in rules.values() if isinstance(r, PassthroughStandardRule)
        )

        # Should have both types of rules
        assert rename_rules > 0, "Should have some renamed metrics for standardization"
        assert (
            passthrough_rules > 0
        ), "Should have some pass-through metrics for debugging"

        # Essential metrics should be renamed
        essential_mapped = [
            "sglang:num_queue_reqs",
            "sglang:gen_throughput",
            "sglang:prompt_tokens_total",
            "sglang:generation_tokens_total",
            "sglang:time_to_first_token_seconds",
            "sglang:time_per_output_token_seconds",
            "sglang:e2e_request_latency_seconds",
            "sglang:cache_hit_rate",
            "sglang:token_usage",
        ]
        for metric in essential_mapped:
            assert metric in rules
            assert isinstance(rules[metric], RenameStandardRule)
