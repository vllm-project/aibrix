# Copyright 2024 The Aibrix Team.
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
from unittest.mock import MagicMock, patch

import pytest
from prometheus_client.core import CounterMetricFamily

from aibrix.metrics.engine_rules import get_metric_standard_rules
from aibrix.metrics.http_collector import HTTPCollector
from aibrix.metrics.standard_rules import PassthroughStandardRule, RenameStandardRule


def test_get_metric_standard_rules_ignore_case():
    # Engine str is all lowercase
    rules = get_metric_standard_rules("vllm")
    assert rules is not None

    # The function get_metric_standard_rules is case-insensitive
    rules2 = get_metric_standard_rules("vLLM")
    assert rules == rules2


def test_get_metric_standard_rules_not_support():
    # TensorRT-LLM is not supported (SGLang is now supported)
    with pytest.raises(ValueError):
        get_metric_standard_rules("TensorRT-LLM")


def test_get_metric_standard_rules_sglang_support():
    # SGLang is now supported
    rules = get_metric_standard_rules("sglang")
    assert rules is not None
    assert len(rules) > 0

    # Case insensitive
    rules2 = get_metric_standard_rules("SGLang")
    assert rules.keys() == rules2.keys()


class TestRenameStandardRule:
    @staticmethod
    def create_sample_metric(
        name: str, value: float = 1.0, metric_type: str = "counter"
    ):
        if metric_type == "counter":
            metric = CounterMetricFamily(name, f"Test {metric_type} metric for {name}")
        else:
            raise ValueError(f"Unsupported metric type: {metric_type}")
        metric.add_metric(labels=[], value=value)
        return metric

    def test_rename(self):
        metric = self.create_sample_metric("old_metric_name")
        rule = RenameStandardRule(
            original_name="old_metric_name", new_name="new_metric_name"
        )

        result = list(rule(metric))

        assert len(result) == 1
        assert len(result[0].samples) == 1
        renamed_metric = result[0]
        assert renamed_metric.name == "new_metric_name"
        assert renamed_metric.samples[0].name == "new_metric_name_total"

    def test_rename_with_prefix_suffix(self):
        metric = self.create_sample_metric("http_requests")
        rule = RenameStandardRule(
            original_name="http_requests", new_name="http_requests_renamed"
        )

        result = list(rule(metric))

        assert len(result) == 1
        assert len(result[0].samples) == 1
        renamed_metric = result[0]
        assert renamed_metric.name == "http_requests_renamed"
        assert renamed_metric.samples[0].name == "http_requests_renamed_total"

    def test_assertion_on_name_mismatch(self):
        metric = self.create_sample_metric("wrong_metric_name")
        rule = RenameStandardRule(original_name="expected_name", new_name="new_name")

        with pytest.raises(AssertionError) as exc_info:
            list(rule(metric))

        assert "does not match Rule original name" in str(exc_info.value)

    def test_multiple_samples(self):
        metric = CounterMetricFamily("old_metric", "Test multiple samples")
        metric.add_metric([], 1.0)
        metric.add_metric([], 2.0)

        rule = RenameStandardRule(original_name="old_metric", new_name="new_metric")

        result = list(rule(metric))

        assert len(result) == 1
        assert len(result[0].samples) == 2
        renamed_metric = result[0]
        assert renamed_metric.name == "new_metric"
        assert renamed_metric.samples[0].name == "new_metric_total"
        assert renamed_metric.samples[1].name == "new_metric_total"


class TestHTTPCollector:
    SAMPLE_METRICS_TEXT = """
# HELP http_requests The total number of HTTP requests.
# TYPE http_requests counter
http_requests{job="api-server",instance="localhost:9090"} 100
# HELP temperature_degrees Current temperature in degrees.
# TYPE temperature_degrees gauge
temperature_degrees{} 25.5
"""

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_collect_success(self, mock_session):
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "http_requests": RenameStandardRule(
                original_name="http_requests", new_name="http_requests_renamed"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,
            timeout=5,
        )

        results = list(collector.collect())
        # Assert
        assert len(collector.metrics_rules) == 1

        original_metric = next((m for m in results if m.name == "http_requests"), None)
        assert original_metric is None

        renamed_metric = next(
            (m for m in results if m.name == "http_requests_renamed"), None
        )
        assert renamed_metric is not None
        assert renamed_metric.samples[0].name == "http_requests_renamed_total"

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_collect_keep_original_metric(self, mock_session):
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "http_requests": RenameStandardRule(
                original_name="http_requests", new_name="http_requests_renamed"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=True,
            timeout=5,
        )

        results = list(collector.collect())
        # Assert
        assert len(collector.metrics_rules) == 1

        # Should have both original and renamed metrics
        original_metric = next((m for m in results if m.name == "http_requests"), None)
        assert original_metric is not None
        assert original_metric.samples[0].name == "http_requests_total"

        renamed_metric = next(
            (m for m in results if m.name == "http_requests_renamed"), None
        )
        assert renamed_metric is not None
        assert renamed_metric.samples[0].name == "http_requests_renamed_total"

        # Verify they are different objects (not the same reference)
        assert original_metric is not renamed_metric

        # Verify we have exactly 3 metrics (original + renamed + unmatched temperature)
        assert len(results) == 3

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_collect_deepcopy_preserves_original(self, mock_session):
        """Test that deepcopy actually preserves the original metric values."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = """# HELP test_metric A test metric
# TYPE test_metric counter
test_metric{label="original_value"} 42
"""
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "test_metric": RenameStandardRule(
                original_name="test_metric", new_name="test_metric_renamed"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=True,
        )

        # Act
        results = list(collector.collect())

        # Assert
        original_metric = next((m for m in results if m.name == "test_metric"), None)
        renamed_metric = next(
            (m for m in results if m.name == "test_metric_renamed"), None
        )

        # Both should exist
        assert original_metric is not None
        assert renamed_metric is not None

        # Both should have the original label value
        assert original_metric.samples[0].labels["label"] == "original_value"
        assert renamed_metric.samples[0].labels["label"] == "original_value"

        # But different metric names
        assert original_metric.samples[0].name == "test_metric_total"
        assert renamed_metric.samples[0].name == "test_metric_renamed_total"

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_collect_request_failure(self, mock_session):
        # Arrange
        mock_session.return_value.get.side_effect = Exception("Connection refused")

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url", metrics_rules={}, timeout=1
        )

        results = list(collector.collect())
        # Assert
        assert len(results) == 0

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_collect_non_200_response(self, mock_session):
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 500
        mock_response.text = "Internal Server Error"
        mock_session.return_value.get.return_value = mock_response

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url", metrics_rules={}, timeout=5
        )

        results = list(collector.collect())
        # Assert
        assert len(results) == 0

    def test_empty_metrics_text(self):
        class FakeHTTPCollector(HTTPCollector):
            def _collect(self):
                return ""

        collector = FakeHTTPCollector(
            endpoint="http://fake.metrics.url", metrics_rules={}
        )

        results = list(collector.collect())
        # Assert
        assert len(results) == 0


class TestPassthroughStandardRule:
    """Test PassthroughStandardRule basic functionality."""

    @staticmethod
    def create_sample_metric(name: str, value: float = 1.0):
        metric = CounterMetricFamily(name, f"Test metric for {name}")
        metric.add_metric(labels=[], value=value)
        return metric

    def test_passthrough_basic(self):
        """Test basic passthrough functionality."""
        metric = self.create_sample_metric("test_metric")
        rule = PassthroughStandardRule("test_metric")

        result = list(rule(metric))

        assert len(result) == 1
        assert result[0].name == "test_metric"
        assert result[0] is metric  # Should be the same object
