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


import pytest

from aibrix.metadata.logger import init_logger


def test_init_logger_basic_functionality():
    """Test basic logger initialization and functionality."""
    logger = init_logger("test_logger")

    # Should have standard log methods
    assert hasattr(logger, "debug")
    assert hasattr(logger, "info")
    assert hasattr(logger, "warning")
    assert hasattr(logger, "error")
    assert hasattr(logger, "critical")


def test_logger_with_structured_data():
    """Test logger with additional structured data - the key feature mentioned."""
    logger = init_logger("test_structured")

    # Test the specific format mentioned: logger.info("msg", a="b", c="d")
    # This should not raise any exceptions
    try:
        logger.info("Test message", user_id=123, action="login", component="auth")
        logger.debug("Debug with data", session_id="abc123", feature="test")
        logger.warning("Warning with data", alert_level="medium", source="test")
        logger.error("Error with data", error_code=500, module="auth")

        # If we get here without exceptions, the structured logging works
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with structured data: {e}")


def test_logger_different_log_levels():
    """Test different log levels work correctly."""
    logger = init_logger("test_levels")

    # Test different levels - should not raise exceptions
    try:
        logger.debug("Debug message", level="debug")
        logger.info("Info message", level="info")
        logger.warning("Warning message", level="warning")
        logger.error("Error message", level="error")
        logger.critical("Critical message", level="critical")

        # If we get here, all log levels work
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with different log levels: {e}")


def test_logger_complex_data_types():
    """Test logger with various data types."""
    logger = init_logger("test_complex")

    # Test with various data types - should not raise exceptions
    try:
        logger.info(
            "Complex test",
            string_val="hello",
            int_val=42,
            float_val=3.14,
            bool_val=True,
            none_val=None,
            list_val=[1, 2, 3],
            dict_val={"nested": "value"},
        )

        # If we get here, complex data types work
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with complex data types: {e}")


def test_logger_multiple_calls():
    """Test multiple logger calls work correctly."""
    logger = init_logger("test_multiple")

    # Make multiple calls - should not raise exceptions
    try:
        for i in range(10):
            logger.info(f"Message {i}", iteration=i, batch="test")

        # If we get here, multiple calls work
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with multiple calls: {e}")


def test_logger_special_characters():
    """Test logger handles special characters correctly."""
    logger = init_logger("test_special")

    # Test with special characters - should not raise exceptions
    try:
        logger.info("Special chars", message="Hello 世界", emoji="🎉", quote='"test"')
        logger.info("Unicode test", symbols="αβγδε", math="∑∏∫", arrows="→←↑↓")
        logger.info(
            "Escape test", backslash="\\", newline="line1\nline2", tab="col1\tcol2"
        )

        # If we get here, special characters work
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with special characters: {e}")


def test_logger_edge_cases():
    """Test logger with edge cases and boundary conditions."""
    logger = init_logger("test_edge_cases")

    try:
        # Test empty message
        logger.info("")

        # Test very long message
        long_message = "A" * 1000
        logger.info(long_message)

        # Test many parameters
        many_params = {f"param_{i}": f"value_{i}" for i in range(50)}
        logger.info("Many params test", **many_params)

        # Test nested data structures
        logger.info(
            "Nested test",
            deep_nested={"level1": {"level2": {"level3": ["a", "b", "c"]}}},
        )

        # If we get here, edge cases work
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with edge cases: {e}")


def test_logger_format_example():
    """Test the specific logger format mentioned: logger.info('msg', a='b', c='d')."""
    logger = init_logger("test_format")

    try:
        # Test the exact format mentioned in the requirements
        logger.info("msg", a="b", c="d")
        logger.info("User login", user_id=12345, username="testuser", ip="192.168.1.1")
        logger.info(
            "API call", endpoint="/api/v1/users", method="GET", response_time=0.05
        )
        logger.error(
            "Database error",
            table="users",
            operation="SELECT",
            error_code="ER_NO_SUCH_TABLE",
        )

        # If we get here, the specific format works
        assert True

    except Exception as e:
        pytest.fail(f"Logger failed with required format: {e}")
