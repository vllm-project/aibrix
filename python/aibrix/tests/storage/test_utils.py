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

"""
Tests for storage utility functions.

Tests the utility functions used by storage implementations, particularly
focusing on filename generation and path sanitization for security.
"""

import pytest

from aibrix.storage.utils import generate_filename, _sanitize_key


class TestGenerateFilename:
    """Test the generate_filename function."""

    def test_basic_key_without_extension(self):
        """Test basic key without any extension or metadata."""
        result = generate_filename("simple_key")
        assert result == "simple_key"

    def test_key_with_content_type(self):
        """Test key with content type that maps to known extension."""
        result = generate_filename("document", content_type="application/json")
        assert result == "document.json"

    def test_key_with_mapped_content_type(self):
        """Test key with content types that have specific mappings."""
        result = generate_filename("image", content_type="image/jpeg")
        assert result == "image.jpg"
        
        result = generate_filename("archive", content_type="application/x-tar")
        assert result == "archive.tar"
        
        result = generate_filename("compressed", content_type="application/gzip")
        assert result == "compressed.gz"

    def test_key_with_generic_content_type(self):
        """Test key with generic content type that gets sanitized."""
        result = generate_filename("file", content_type="application/custom-type")
        assert result == "file.customtype"

    def test_key_with_metadata_filename(self):
        """Test key with filename in metadata takes precedence."""
        metadata = {"filename": "original.txt"}
        result = generate_filename("key", content_type="application/json", metadata=metadata)
        assert result == "key.txt"  # Extension from metadata, not content_type

    def test_key_with_complex_content_type(self):
        """Test key with complex content type gets properly sanitized."""
        result = generate_filename("file", content_type="application/vnd.api+json")
        assert result == "file.vndapijson"

    def test_path_traversal_protection(self):
        """Test that dangerous keys are sanitized."""
        dangerous_keys = [
            "../etc/passwd",
            "../../sensitive/file.txt",
            "normal/../../../etc/hosts",
            "../../../root/.ssh/id_rsa",
            "..\\..\\windows\\system32\\config\\sam",
            "....//....//etc/shadow",
            "/etc/passwd",
            "~/../../etc/passwd",
        ]
        
        expected_results = [
            "etc/passwd",
            "sensitive/file.txt",
            "normal/etc/hosts",
            "root/.ssh/id_rsa",
            "windows/system32/config/sam",
            "etc/shadow",
            "etc/passwd",
            "~/etc/passwd",
        ]
        
        for dangerous_key, expected in zip(dangerous_keys, expected_results):
            result = generate_filename(dangerous_key)
            assert result == expected, f"Expected {expected} but got {result} for key {dangerous_key}"

    def test_empty_or_dangerous_only_keys(self):
        """Test keys that are empty or contain only dangerous patterns."""
        dangerous_only_keys = [
            "",
            "../..",
            "../../..",
            "....",
            "/",
            "//",
            "\\\\",
        ]
        
        for key in dangerous_only_keys:
            result = generate_filename(key)
            assert result == "safe_key", f"Expected 'safe_key' but got '{result}' for key '{key}'"

    def test_key_with_existing_extension_no_content_type_override(self):
        """Test that keys with existing extensions don't get additional extensions."""
        result = generate_filename("document.txt", content_type="application/json")
        assert result == "document.txt"  # Should not become document.txt.json

    def test_combined_content_type_and_traversal(self):
        """Test dangerous keys with content types."""
        result = generate_filename("../etc/passwd", content_type="text/plain")
        assert result == "etc/passwd.plain"


class TestSanitizeKey:
    """Test the _sanitize_key function."""

    def test_normal_keys(self):
        """Test that normal keys pass through unchanged."""
        normal_keys = [
            "simple_key",
            "path/to/file",
            "deep/nested/path/file",
            "file_with_underscores",
            "file-with-dashes",
            "file123",
            "CamelCaseFile",
        ]
        
        for key in normal_keys:
            result = _sanitize_key(key)
            assert result == key, f"Normal key {key} should pass through unchanged"

    def test_path_traversal_removal(self):
        """Test removal of path traversal patterns."""
        test_cases = [
            ("../file", "file"),
            ("../../file", "file"),
            ("../../../file", "file"),
            ("path/../file", "path/file"),
            ("path/../../file", "path/file"),
            ("normal/../../../etc/hosts", "normal/etc/hosts"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_windows_path_traversal(self):
        """Test removal of Windows-style path traversal."""
        test_cases = [
            ("..\\file", "file"),
            ("..\\..\\file", "file"),
            ("path\\..\\file", "path/file"),
            ("..\\..\\windows\\system32\\config\\sam", "windows/system32/config/sam"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_absolute_path_removal(self):
        """Test removal of absolute path indicators."""
        test_cases = [
            ("/etc/passwd", "etc/passwd"),
            ("/absolute/path/file", "absolute/path/file"),
            ("//double/slash", "double/slash"),
            ("///triple/slash", "triple/slash"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_dot_patterns(self):
        """Test handling of various dot patterns."""
        test_cases = [
            ("./file", "file"),
            ("path/./file", "path/file"),
            ("....//file", "file"),  # Multiple dots should be filtered
            ("..../..../etc/shadow", "etc/shadow"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_empty_and_dangerous_only_inputs(self):
        """Test inputs that are empty or contain only dangerous patterns."""
        dangerous_only_inputs = [
            "",
            "../..",
            "../../..",
            "....",
            "/",
            "//",
            "\\\\",
            "../",
            "../../",
            "./",
        ]
        
        for input_key in dangerous_only_inputs:
            result = _sanitize_key(input_key)
            assert result == "safe_key", f"Expected 'safe_key' but got '{result}' for dangerous input '{input_key}'"

    def test_mixed_separators(self):
        """Test handling of mixed path separators."""
        test_cases = [
            ("path\\to/file", "path/to/file"),
            ("mixed\\..//path", "mixed/path"),
            ("complex\\../path/./file", "complex/path/file"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_special_characters_preserved(self):
        """Test that safe special characters are preserved."""
        test_cases = [
            ("file_with_underscores", "file_with_underscores"),
            ("file-with-dashes", "file-with-dashes"),
            ("file.with.dots", "file.with.dots"),
            ("file@domain.com", "file@domain.com"),
            ("file+version", "file+version"),
            ("file(1)", "file(1)"),
            ("file[1]", "file[1]"),
            ("file{1}", "file{1}"),
            ("file~backup", "file~backup"),
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"

    def test_tilde_handling(self):
        """Test handling of tilde characters in paths."""
        test_cases = [
            ("~/file", "~/file"),  # Tilde at start is preserved
            ("path/~/file", "path/~/file"),  # Tilde in middle is preserved
            ("~/../../etc/passwd", "~/etc/passwd"),  # Tilde preserved, traversal removed
        ]
        
        for input_key, expected in test_cases:
            result = _sanitize_key(input_key)
            assert result == expected, f"Expected {expected} but got {result} for {input_key}"