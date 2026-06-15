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

import collections.abc
import json
from enum import Enum
from typing import Any, Dict, Optional

from aibrix.batch.job_entity.aibrix_metadata import AibrixMetadata
from aibrix.batch.job_entity.batch_job import (
    BatchJobSpec,
    CompletionWindow,
)
from aibrix.logger import init_logger

# Annotation prefix for batch job specifications
JOB_ANNOTATION_PREFIX = "batch.job.aibrix.ai/"

logger = init_logger(__name__)


class JobAnnotationKey(str, Enum):
    """Valid annotation keys for job specifications."""

    SESSION_ID = f"{JOB_ANNOTATION_PREFIX}session-id"
    INPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}input-file-id"
    ENDPOINT = f"{JOB_ANNOTATION_PREFIX}endpoint"
    METADATA_PREFIX = f"{JOB_ANNOTATION_PREFIX}metadata."
    OPTS_PREFIX = f"{JOB_ANNOTATION_PREFIX}opts."
    AIBRIX = f"{JOB_ANNOTATION_PREFIX}aibrix"
    OUTPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}output-file-id"
    TEMP_OUTPUT_FILE_ID = f"{JOB_ANNOTATION_PREFIX}temp-output-file-id"
    ERROR_FILE_ID = f"{JOB_ANNOTATION_PREFIX}error-file-id"
    TEMP_ERROR_FILE_ID = f"{JOB_ANNOTATION_PREFIX}temp-error-file-id"

    MODEL_TEMPLATE_NAME = f"{JOB_ANNOTATION_PREFIX}model-template-name"
    MODEL_TEMPLATE_VERSION = f"{JOB_ANNOTATION_PREFIX}model-template-version"
    PROFILE_NAME = f"{JOB_ANNOTATION_PREFIX}profile-name"
    TEMPLATE_OVERRIDES = f"{JOB_ANNOTATION_PREFIX}template-overrides"  # JSON-encoded
    PROFILE_OVERRIDES = f"{JOB_ANNOTATION_PREFIX}profile-overrides"  # JSON-encoded


class BatchJobTransformer:
    """Parses batch-job annotations (written by JobManifestRenderer) back into a
    BatchJobSpec. Used to verify the render annotation contract round-trips."""

    @classmethod
    def _extract_batch_job_spec(
        cls, annotations: Dict[str, str], pod_spec: Any
    ) -> BatchJobSpec:
        """Extract BatchJobSpec from Kubernetes job annotations."""
        # Extract required fields
        input_file_id = annotations.get(JobAnnotationKey.INPUT_FILE_ID.value)
        if not input_file_id:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.INPUT_FILE_ID.value}' not found"
            )

        endpoint = annotations.get(JobAnnotationKey.ENDPOINT.value)
        if not endpoint:
            raise ValueError(
                f"Required annotation '{JobAnnotationKey.ENDPOINT.value}' not found"
            )

        # Extract batch metadata (key-value pairs with prefix)
        batch_metadata = {}
        batch_opts = {}
        for key, value in annotations.items():
            if key.startswith(JobAnnotationKey.METADATA_PREFIX.value):
                # Remove prefix to get the actual metadata key
                metadata_key = key[len(JobAnnotationKey.METADATA_PREFIX.value) :]
                batch_metadata[metadata_key] = value
            elif key.startswith(JobAnnotationKey.OPTS_PREFIX.value):
                # Remove prefix to get the actual opts key
                opts_key = key[len(JobAnnotationKey.OPTS_PREFIX.value) :]
                batch_opts[opts_key] = value

        # Template / profile selection. All optional;
        # absence means batch was created before the template feature
        # or via the legacy hardcoded yaml path.
        template_name = annotations.get(JobAnnotationKey.MODEL_TEMPLATE_NAME.value)
        template_version = annotations.get(
            JobAnnotationKey.MODEL_TEMPLATE_VERSION.value
        )
        profile_name = annotations.get(JobAnnotationKey.PROFILE_NAME.value)

        def _decode(key: JobAnnotationKey) -> Optional[Dict[str, Any]]:
            raw = annotations.get(key.value)
            if not raw:
                return None
            try:
                return json.loads(raw)
            except json.JSONDecodeError as e:
                logger.warning(
                    "Failed to parse overrides annotation; treating as None",
                    annotation_key=key.value,
                    error=str(e),
                    annotation_value=raw,
                )  # type: ignore[call-arg]
                return None

        template_overrides = _decode(JobAnnotationKey.TEMPLATE_OVERRIDES)
        profile_overrides = _decode(JobAnnotationKey.PROFILE_OVERRIDES)
        aibrix = (
            AibrixMetadata.model_validate_json(
                annotations[JobAnnotationKey.AIBRIX.value]
            )
            if JobAnnotationKey.AIBRIX.value in annotations
            else None
        )
        # Backward compatible logic, will be upgrade to consolidated aibrix field.
        aibrix_from_spec = AibrixMetadata.from_extension_fields(
            model_template_name=template_name,
            model_template_version=template_version,
            profile_name=profile_name,
            template_overrides=template_overrides,
            profile_overrides=profile_overrides,
        )
        if aibrix is None:
            aibrix = aibrix_from_spec
        elif aibrix_from_spec is not None:
            merged = aibrix.model_dump(exclude_none=True)
            merged.update(aibrix_from_spec.model_dump(exclude_none=True))
            aibrix = AibrixMetadata.model_validate(merged)

        return BatchJobSpec(
            input_file_id=input_file_id,
            endpoint=endpoint,
            completion_window=cls._safe_get_attr(
                pod_spec,
                "activeDeadlineSeconds",
                CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
            ),
            metadata=batch_metadata if batch_metadata else None,
            opts=batch_opts if batch_opts else None,
            aibrix=aibrix,
        )

    @classmethod
    def _safe_get_attr(cls, obj: Any, attr: str, default: Any = None) -> Any:
        """Safely get attribute from object, supporting both attr access and dict access."""
        if obj is None:
            return default

        # Try dict-like access via collections.abc.Mapping, else attribute access
        if isinstance(obj, collections.abc.Mapping):
            val = obj.get(attr, None)
        else:
            val = getattr(obj, attr, None)

        return default if val is None else val
