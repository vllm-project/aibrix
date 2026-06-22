"""Unit tests for BatchJobError and related job entity classes"""

import json
from pathlib import Path

import pytest

from aibrix.batch.job_entity import (
    AibrixMetadata,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchProfileRef,
    ClientConfig,
    CompletionWindow,
    JobAnnotationKey,
    ModelTemplateRef,
    ResourceAllocation,
    ResourceDetail,
    RuntimeSpec,
)
from aibrix.batch.manifest.renderer import JobManifestRenderer, RenderError
from aibrix.batch.template import local_profile_registry, local_template_registry

_FIXTURE = Path(__file__).parent / "testdata" / "template_configmaps_unittest.yaml"


class TestBatchJobError:
    """Test BatchJobError creation and handling."""

    def test_batch_job_error_with_standard_exception(self):
        """Test creating BatchJobError with a standard Exception converted to string."""
        # Create a standard exception
        fe = Exception("Something went wrong during processing")

        # This should not throw an error
        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        # Verify the error was created correctly
        assert error.code == BatchJobErrorCode.FINALIZING_ERROR.value
        assert error.message == "Something went wrong during processing"
        assert error.param is None
        assert error.line is None
        assert str(error) == "Something went wrong during processing"

    def test_batch_job_error_with_nested_exception(self):
        """Test creating BatchJobError with a nested exception chain."""
        try:
            try:
                raise ValueError("Inner error")
            except ValueError as inner:
                raise RuntimeError("Outer error") from inner
        except RuntimeError as fe:
            error = BatchJobError(
                code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe)
            )

            assert error.code == BatchJobErrorCode.FINALIZING_ERROR.value
            assert error.message == "Outer error"

    def test_batch_job_error_with_all_parameters(self):
        """Test creating BatchJobError with all optional parameters."""
        fe = Exception("Processing failed at specific location")

        error = BatchJobError(
            code=BatchJobErrorCode.FINALIZING_ERROR,
            message=str(fe),
            param="output_file_id",
            line=42,
        )

        assert error.code == BatchJobErrorCode.FINALIZING_ERROR.value
        assert error.message == "Processing failed at specific location"
        assert error.param == "output_file_id"
        assert error.line == 42

    def test_batch_job_error_inheritance(self):
        """Test that BatchJobError is properly an Exception subclass."""
        fe = Exception("Test exception")

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        # Should be an instance of Exception
        assert isinstance(error, Exception)
        assert isinstance(error, BatchJobError)

        # Should be raisable
        with pytest.raises(BatchJobError) as exc_info:
            raise error

        assert exc_info.value.code == BatchJobErrorCode.FINALIZING_ERROR.value
        assert exc_info.value.message == "Test exception"

    def test_batch_job_error_with_unicode_exception(self):
        """Test creating BatchJobError with unicode characters in exception message."""
        fe = Exception("Processing failed: 文件不存在 (file not found)")

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        assert error.code == BatchJobErrorCode.FINALIZING_ERROR.value
        assert error.message == "Processing failed: 文件不存在 (file not found)"

    def test_batch_job_error_with_json_serialization(self):
        """Test that BatchJobError can be represented in JSON-like structure."""
        fe = Exception("JSON serialization test")

        error = BatchJobError(
            code=BatchJobErrorCode.FINALIZING_ERROR,
            message=str(fe),
            param="test_param",
            line=100,
        )

        # Test that error attributes can be serialized
        error_dict = {
            "code": error.code,
            "message": error.message,
            "param": error.param,
            "line": error.line,
        }

        # Should be able to serialize to JSON
        json_str = json.dumps(error_dict)
        assert json_str is not None

        # Should be able to deserialize
        parsed = json.loads(json_str)
        assert parsed["code"] == BatchJobErrorCode.FINALIZING_ERROR.value
        assert parsed["message"] == "JSON serialization test"
        assert parsed["param"] == "test_param"
        assert parsed["line"] == 100


class TestBatchJobEntityCreation:
    """Test creation of various batch job entities."""

    def test_batch_job_spec_creation(self):
        """Test creating BatchJobSpec."""
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            metadata={"priority": "high"},
        )

        assert spec.input_file_id == "test-input-123"
        assert spec.endpoint == "/v1/chat/completions"
        assert spec.completion_window == 86400
        assert spec.metadata == {"priority": "high"}

    def test_batch_job_spec_supported_completion_windows(self):
        windows = {
            "1h": 3600,
            "2h": 7200,
            "6h": 21600,
            "12h": 43200,
            "24h": 86400,
        }
        for window, seconds in windows.items():
            spec = BatchJobSpec.from_strings(
                input_file_id="test-input-123",
                endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
                completion_window=window,
            )
            assert spec.completion_window == seconds

    def test_batch_job_spec_creation_with_aibrix_metadata(self):
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            aibrix=AibrixMetadata(
                job_id="job-123",
                runtime=RuntimeSpec(target="Kubernetes"),
                resource_allocation=ResourceAllocation(
                    provision_id="reservation-1",
                    provision_resource_deadline=3600,
                    resource_details=[
                        ResourceDetail(
                            provider="deployment",
                            endpoint_cluster="cluster-a",
                            gpu_type="H100",
                            replica=2,
                        )
                    ],
                ),
                model_template=ModelTemplateRef(
                    name="mock-vllm",
                    version="v0.0.1",
                    overrides={"engine_args": {"max_num_seqs": 128}},
                ),
                profile=BatchProfileRef(
                    name="unittest",
                    overrides={"scheduling": {"max_concurrency": 4}},
                ),
            ),
        )

        assert spec.aibrix is not None
        assert spec.aibrix.job_id == "job-123"
        assert spec.aibrix.runtime is not None
        assert spec.aibrix.runtime.target == "Kubernetes"
        assert spec.aibrix.runtime_target == "Kubernetes"
        assert spec.aibrix.resource_allocation is not None
        assert spec.aibrix.resource_allocation.provision_id == "reservation-1"
        assert spec.aibrix.resource_allocation.resource_details is not None
        assert spec.aibrix.resource_allocation.resource_details[0].gpu_type == "H100"
        assert spec.aibrix.model_template is not None
        assert spec.aibrix.model_template.name == "mock-vllm"
        assert spec.aibrix.profile is not None
        assert spec.aibrix.profile.name == "unittest"

    def test_resource_allocation_allows_extra_fields(self):
        allocation = ResourceAllocation.model_validate(
            {
                "provision_id": "reservation-1",
                "future_field": {"phase": "queued"},
            }
        )

        assert allocation.provision_id == "reservation-1"
        assert getattr(allocation, "future_field") == {"phase": "queued"}

    def test_aibrix_metadata_extension_fields_include_client(self):
        metadata = AibrixMetadata(
            client=ClientConfig.model_validate(
                {
                    "max_concurrency": 256,
                    "adaptive_concurrency": True,
                    "adaptive_max_factor": 16,
                    "retry_policy": {
                        "max_retries": 5,
                        "base_delay_seconds": 2,
                        "max_delay_seconds": 10,
                        "no_endpoint_max_retries": 5,
                    },
                }
            )
        )

        fields = metadata.to_extension_fields()
        restored = AibrixMetadata.from_extension_fields(**fields)

        assert fields["client"]["max_concurrency"] == 256
        assert restored is not None
        assert restored.client is not None
        assert restored.client.retry_policy is not None
        assert restored.client.retry_policy.base_delay_seconds == 2

    def test_resource_detail_allows_extra_fields(self):
        detail = ResourceDetail.model_validate(
            {
                "provider": "deployment",
                "gpu_type": "H100",
                "future_field": ["zone-a", "zone-b"],
            }
        )

        assert detail.provider == "deployment"
        assert detail.gpu_type == "H100"
        assert getattr(detail, "future_field") == ["zone-a", "zone-b"]

    def test_batch_job_error_codes_coverage(self):
        """Test that all BatchJobErrorCode values work with exceptions."""
        test_exception = Exception("Test exception message")

        # Test each error code
        error_codes_to_test = [
            BatchJobErrorCode.INVALID_INPUT_FILE,
            BatchJobErrorCode.INVALID_ENDPOINT,
            BatchJobErrorCode.INVALID_COMPLETION_WINDOW,
            BatchJobErrorCode.INVALID_METADATA,
            BatchJobErrorCode.AUTHENTICATION_ERROR,
            BatchJobErrorCode.INFERENCE_FAILED,
            BatchJobErrorCode.PREPARE_OUTPUT_ERROR,
            BatchJobErrorCode.FINALIZING_ERROR,
            BatchJobErrorCode.UNKNOWN_ERROR,
        ]

        for error_code in error_codes_to_test:
            # This should not throw any errors
            error = BatchJobError(code=error_code, message=str(test_exception))

            assert error.code == error_code.value
            assert error.message == "Test exception message"
            assert isinstance(error, Exception)
            assert isinstance(error, BatchJobError)

    def test_render_job_manifest_with_template_and_profile(self):
        template_registry = local_template_registry(_FIXTURE)
        profile_registry = local_profile_registry(_FIXTURE)
        template_registry.reload()
        profile_registry.reload()

        renderer = JobManifestRenderer(template_registry, profile_registry)
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            aibrix=AibrixMetadata(
                model_template=ModelTemplateRef(name="mock-vllm"),
                profile=BatchProfileRef(name="unittest"),
            ),
        )

        manifest = renderer.render("session-1", spec, job_name="batch-test")

        pod_annotations = manifest["spec"]["template"]["metadata"]["annotations"]
        assert manifest["metadata"]["name"] == "batch-test"
        assert (
            pod_annotations[JobAnnotationKey.MODEL_TEMPLATE_NAME.value] == "mock-vllm"
        )
        assert pod_annotations[JobAnnotationKey.PROFILE_NAME.value] == "unittest"
        assert manifest["spec"]["suspend"] is True
        assert (
            manifest["spec"]["template"]["spec"]["containers"][1]["image"]
            == "aibrix/vllm-mock:nightly"
        )

    def test_render_with_inline_template_and_profile_without_registries(self):
        # Prepare template and profile from fixture, so inline spec is available.
        template_registry = local_template_registry(_FIXTURE)
        profile_registry = local_profile_registry(_FIXTURE)
        template_registry.reload()
        profile_registry.reload()
        template = template_registry.get("mock-vllm")
        profile = profile_registry.get("unittest")
        assert template is not None
        assert profile is not None

        # Renderer without registries: the inline specs carry the resolved data.
        renderer = JobManifestRenderer()
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            aibrix=AibrixMetadata(
                model_template=ModelTemplateRef(
                    name=template.name,
                    version=template.version,
                    spec=template.spec.model_dump(exclude_none=True),
                ),
                profile=BatchProfileRef(
                    name=profile.name,
                    spec=profile.spec.model_dump(exclude_none=True),
                ),
            ),
        )

        manifest = renderer.render("session-1", spec, job_name="batch-test")

        pod_annotations = manifest["spec"]["template"]["metadata"]["annotations"]
        assert (
            pod_annotations[JobAnnotationKey.MODEL_TEMPLATE_NAME.value] == template.name
        )
        assert pod_annotations[JobAnnotationKey.PROFILE_NAME.value] == profile.name

    def test_render_without_template_registry_reports_render_error(self):
        renderer = JobManifestRenderer()
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            aibrix=AibrixMetadata(model_template=ModelTemplateRef(name="mock-vllm")),
        )

        with pytest.raises(RenderError, match="template registry is not configured"):
            renderer.render("session-1", spec, job_name="batch-test")

    def test_render_without_profile_registry_reports_render_error(self):
        # Inline template spec is available; the profile is referenced by name
        # with no registry configured -> render error.
        template_registry = local_template_registry(_FIXTURE)
        template_registry.reload()
        template = template_registry.get("mock-vllm")
        assert template is not None

        renderer = JobManifestRenderer()
        spec = BatchJobSpec(
            input_file_id="test-input-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            aibrix=AibrixMetadata(
                model_template=ModelTemplateRef(
                    name=template.name,
                    version=template.version,
                    spec=template.spec.model_dump(exclude_none=True),
                ),
                profile=BatchProfileRef(name="unittest"),
            ),
        )

        with pytest.raises(RenderError, match="profile registry is not configured"):
            renderer.render("session-1", spec, job_name="batch-test")


class TestExceptionMessageConversion:
    """Test various ways exceptions can be converted to messages."""

    def test_exception_with_args(self):
        """Test exception with multiple arguments."""
        fe = Exception("Primary message", "Secondary info", 42)

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        # str() should convert the first argument
        assert "Primary message" in error.message

    def test_exception_with_no_args(self):
        """Test exception with no arguments."""
        fe = Exception()

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        # Should handle empty exception gracefully
        assert error.message == ""

    def test_custom_exception_class(self):
        """Test with custom exception class."""

        class CustomFinalizationError(Exception):
            def __init__(self, operation, details):
                self.operation = operation
                self.details = details
                super().__init__(f"Operation '{operation}' failed: {details}")

        fe = CustomFinalizationError("file_upload", "network timeout")

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        assert error.message == "Operation 'file_upload' failed: network timeout"

    def test_exception_with_special_characters(self):
        """Test exception message with special characters."""
        fe = Exception("Error with special chars: !@#$%^&*()_+-={}[]|\\:;\"'<>?,./")

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        assert (
            error.message
            == "Error with special chars: !@#$%^&*()_+-={}[]|\\:;\"'<>?,./"
        )

    def test_exception_with_newlines_and_tabs(self):
        """Test exception message with newlines and tabs."""
        fe = Exception("Multi-line\nerror\tmessage\nwith\ttabs")

        error = BatchJobError(code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe))

        assert error.message == "Multi-line\nerror\tmessage\nwith\ttabs"


class TestBatchJobErrorFastAPICompatibility:
    """Test BatchJobError compatibility with FastAPI serialization."""

    def test_batch_job_error_pydantic_type_adapter_compatibility(self):
        """Test BatchJobError with Pydantic TypeAdapter (FastAPI requirement)."""
        from pydantic import TypeAdapter

        # Create a BatchJobError instance
        fe = Exception("Pydantic TypeAdapter test")
        error = BatchJobError(
            code=BatchJobErrorCode.FINALIZING_ERROR,
            message=str(fe),
            param="test_param",
            line=100,
        )

        # Create TypeAdapter for BatchJobError
        adapter = TypeAdapter(BatchJobError)

        # Test serialization (this is what was failing)
        serialized = adapter.dump_python(error)

        assert isinstance(serialized, dict)
        assert serialized["code"] == BatchJobErrorCode.FINALIZING_ERROR.value
        assert serialized["message"] == "Pydantic TypeAdapter test"
        assert serialized["param"] == "test_param"
        assert serialized["line"] == 100

    def test_batch_job_error_pydantic_json_serialization(self):
        """Test BatchJobError JSON serialization through Pydantic TypeAdapter."""
        from pydantic import TypeAdapter

        fe = Exception("JSON TypeAdapter test")
        error = BatchJobError(
            code=BatchJobErrorCode.AUTHENTICATION_ERROR,
            message=str(fe),
            param=None,
            line=None,
        )

        adapter = TypeAdapter(BatchJobError)

        # Test JSON serialization
        json_str = adapter.dump_json(error)
        assert json_str is not None

        # Test the JSON content
        parsed = json.loads(json_str)
        assert parsed["code"] == BatchJobErrorCode.AUTHENTICATION_ERROR.value
        assert parsed["message"] == "JSON TypeAdapter test"
        assert "param" not in parsed
        assert "line" not in parsed

    def test_batch_job_error_pydantic_validation_and_serialization_roundtrip(self):
        """Test BatchJobError validation and serialization roundtrip through Pydantic."""
        from pydantic import TypeAdapter

        # Original error
        fe = Exception("Roundtrip test")
        original_error = BatchJobError(
            code=BatchJobErrorCode.PREPARE_OUTPUT_ERROR,
            message=str(fe),
            param="roundtrip_param",
            line=50,
        )

        adapter = TypeAdapter(BatchJobError)

        # Serialize to dict
        error_dict = adapter.dump_python(original_error)

        # Validate back to BatchJobError (roundtrip)
        validated_error = adapter.validate_python(error_dict)

        # Verify the roundtrip worked
        assert isinstance(validated_error, BatchJobError)
        assert validated_error.code == original_error.code
        assert validated_error.message == original_error.message
        assert validated_error.param == original_error.param
        assert validated_error.line == original_error.line

    def test_batch_job_error_list_serialization_for_fastapi(self):
        """Test BatchJobError list serialization for FastAPI responses with errors list."""
        from typing import List

        from pydantic import TypeAdapter

        # Create multiple errors
        exceptions = [
            ValueError("First validation error"),
            FileNotFoundError("Second file error"),
            RuntimeError("Third runtime error"),
        ]

        errors = []
        error_codes = [
            BatchJobErrorCode.INVALID_INPUT_FILE,
            BatchJobErrorCode.AUTHENTICATION_ERROR,
            BatchJobErrorCode.FINALIZING_ERROR,
        ]

        for exc, code in zip(exceptions, error_codes):
            error = BatchJobError(
                code=code, message=str(exc), param=f"param_{code.value}", line=None
            )
            errors.append(error)

        # Test list serialization
        adapter = TypeAdapter(List[BatchJobError])

        # Serialize list of errors
        serialized_errors = adapter.dump_python(errors)

        assert isinstance(serialized_errors, list)
        assert len(serialized_errors) == 3

        # Verify each error was serialized correctly
        for i, serialized_error in enumerate(serialized_errors):
            assert isinstance(serialized_error, dict)
            assert serialized_error["code"] == error_codes[i].value
            assert serialized_error["message"] == str(exceptions[i])
            assert serialized_error["param"] == f"param_{error_codes[i].value}"
            assert "line" not in serialized_error
