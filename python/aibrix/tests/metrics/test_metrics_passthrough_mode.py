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

from unittest.mock import MagicMock, patch

from aibrix.metrics.http_collector import HTTPCollector
from aibrix.metrics.standard_rules import RenameStandardRule


class TestMetricsPassthroughMode:
    """Test metrics passthrough mode functionality."""

    SAMPLE_METRICS_TEXT = """
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="test-model"} 5
# HELP vllm:prompt_tokens_total Number of prefill tokens processed.
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{model_name="test-model"} 1000
"""

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_raw_passthrough_mode_enabled(self, mock_session):
        """Test raw passthrough mode returns all metrics unchanged."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "vllm:num_requests_waiting": RenameStandardRule(
                "vllm:num_requests_waiting", "aibrix:queue_size"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,  # Would normally suppress original
            raw_passthrough_mode=True,  # Override - return raw metrics
        )

        # Act
        results = list(collector.collect())

        # Assert
        metric_names = [m.name for m in results]

        # Should have original metrics (note: prometheus client strips _total from counters)
        assert "vllm:num_requests_waiting" in metric_names
        assert "vllm:prompt_tokens" in metric_names

        # Should NOT have transformed metrics
        assert "aibrix:queue_size" not in metric_names

        # Should have exactly 2 metrics (no duplication)
        assert len(results) == 2

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_raw_passthrough_mode_disabled(self, mock_session):
        """Test normal transformation mode applies rules."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "vllm:num_requests_waiting": RenameStandardRule(
                "vllm:num_requests_waiting", "aibrix:queue_size"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,
            raw_passthrough_mode=False,
        )

        # Act
        results = list(collector.collect())

        # Assert
        metric_names = [m.name for m in results]

        # Should NOT have original vllm:num_requests_waiting
        assert "vllm:num_requests_waiting" not in metric_names

        # Should have transformed metric
        assert "aibrix:queue_size" in metric_names

        # Should have untransformed metric that has no rule
        assert "vllm:prompt_tokens" not in metric_names

    @patch("aibrix.metrics.http_collector.envs.METRICS_RAW_PASSTHROUGH_MODE", True)
    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_env_var_enables_passthrough_mode(self, mock_session):
        """Test environment variable enables raw passthrough mode."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "vllm:num_requests_waiting": RenameStandardRule(
                "vllm:num_requests_waiting", "aibrix:queue_size"
            )
        }

        # Don't specify raw_passthrough_mode - should use env var
        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,
        )

        # Act
        results = list(collector.collect())

        # Assert
        metric_names = [m.name for m in results]

        # Should have original metrics (env var enabled passthrough)
        assert "vllm:num_requests_waiting" in metric_names
        assert "vllm:prompt_tokens" in metric_names

        # Should NOT have transformed metrics
        assert "aibrix:queue_size" not in metric_names

    @patch("aibrix.metrics.http_collector.envs.METRICS_ENABLE_TRANSFORMATION", False)
    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_transformation_disabled_forces_passthrough(self, mock_session):
        """Test disabling transformation forces raw passthrough mode."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "vllm:num_requests_waiting": RenameStandardRule(
                "vllm:num_requests_waiting", "aibrix:queue_size"
            )
        }

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,
            raw_passthrough_mode=False,  # Explicitly try to disable
        )

        # Act
        results = list(collector.collect())

        # Assert - should still be in passthrough mode
        metric_names = [m.name for m in results]

        # Should have original metrics (transformation disabled)
        assert "vllm:num_requests_waiting" in metric_names
        assert "vllm:prompt_tokens" in metric_names

        # Should NOT have transformed metrics
        assert "aibrix:queue_size" not in metric_names

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_transformation_error_fallback_to_passthrough(self, mock_session):
        """Test transformation errors fall back to raw passthrough."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        # Create a rule that will raise an exception
        class FailingRule:
            def __call__(self, metric):
                raise ValueError("Simulated transformation error")

        metrics_rules = {"vllm:num_requests_waiting": FailingRule()}

        collector = HTTPCollector(
            endpoint="http://fake.metrics.url",
            metrics_rules=metrics_rules,
            keep_original_metric=False,
            raw_passthrough_mode=False,
        )

        # Act
        results = list(collector.collect())

        # Assert - should fall back to passthrough mode
        metric_names = [m.name for m in results]

        # Should have original metrics (fallback mode)
        assert "vllm:num_requests_waiting" in metric_names
        assert "vllm:prompt_tokens" in metric_names

        # Should have exactly 2 metrics
        assert len(results) == 2

    @patch("aibrix.metrics.http_collector.requests.Session")
    def test_explicit_passthrough_overrides_env_var(self, mock_session):
        """Test explicit passthrough mode parameter overrides environment variable."""
        # Arrange
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.text = self.SAMPLE_METRICS_TEXT
        mock_session.return_value.get.return_value = mock_response

        metrics_rules = {
            "vllm:num_requests_waiting": RenameStandardRule(
                "vllm:num_requests_waiting", "aibrix:queue_size"
            )
        }

        # Mock env var to be False, but explicitly enable passthrough
        with patch(
            "aibrix.metrics.http_collector.envs.METRICS_RAW_PASSTHROUGH_MODE", False
        ):
            collector = HTTPCollector(
                endpoint="http://fake.metrics.url",
                metrics_rules=metrics_rules,
                keep_original_metric=False,
                raw_passthrough_mode=True,  # Explicit override
            )

            # Act
            results = list(collector.collect())

            # Assert
            metric_names = [m.name for m in results]

            # Should have original metrics (explicit override)
            assert "vllm:num_requests_waiting" in metric_names
            assert "vllm:prompt_tokens" in metric_names

            # Should NOT have transformed metrics
            assert "aibrix:queue_size" not in metric_names

    def test_mode_detection_priority(self):
        """Test the priority of different configuration methods."""
        # Test 1: Explicit parameter overrides everything
        collector1 = HTTPCollector(
            endpoint="http://fake.url",
            raw_passthrough_mode=True,
        )
        assert collector1.raw_passthrough_mode is True

        # Test 2: Environment variable used when no explicit parameter
        with patch(
            "aibrix.metrics.http_collector.envs.METRICS_RAW_PASSTHROUGH_MODE", True
        ):
            with patch(
                "aibrix.metrics.http_collector.envs.METRICS_ENABLE_TRANSFORMATION", True
            ):
                collector2 = HTTPCollector(endpoint="http://fake.url")
                assert collector2.raw_passthrough_mode is True

        # Test 3: Transformation disabled forces passthrough regardless of other settings
        with patch(
            "aibrix.metrics.http_collector.envs.METRICS_ENABLE_TRANSFORMATION", False
        ):
            with patch(
                "aibrix.metrics.http_collector.envs.METRICS_RAW_PASSTHROUGH_MODE", False
            ):
                collector3 = HTTPCollector(
                    endpoint="http://fake.url",
                    raw_passthrough_mode=False,
                )
                assert collector3.raw_passthrough_mode is True
