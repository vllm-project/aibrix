# Copyright 2026 The Aibrix Team.
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

"""Registries for ModelDeploymentTemplate and BatchProfile.

A registry caches a parsed, validated set of templates / profiles in
memory. The actual loading is delegated to a TemplateSource (K8s
ConfigMap, local file, or future DB), so registries are testable
without a live cluster.

Lookups (get / all) are synchronous and lock-protected. Reload is
synchronous; an optional async watch task triggers reload on
ConfigMap change events.

See docs/source/features/batch-templates.rst for the admin-facing
description of how these are exposed.
"""

from __future__ import annotations

import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Generic, List, Optional, Tuple, TypeVar

import yaml
from pydantic import ValidationError

from aibrix.logger import init_logger

from .schema import (
    BatchProfile,
    BatchProfileList,
    CompletionWindowOption,
    DeploymentMode,
    ModelDeploymentTemplate,
    ModelDeploymentTemplateList,
    Priority,
    ProviderType,
    TemplateStatus,
)

logger = init_logger(__name__)

# Fixed Kubernetes resource references (v5 design §8). Hardcoded so that
# the metadata service can find them without configuration. If/when
# multi-tenant isolation requires per-namespace registries, these become
# constructor parameters.
DEFAULT_NAMESPACE = "aibrix-system"
TEMPLATES_CONFIGMAP_NAME = "aibrix-model-deployment-templates"
TEMPLATES_DATA_KEY = "templates.yaml"
PROFILES_CONFIGMAP_NAME = "aibrix-batch-profiles"
PROFILES_DATA_KEY = "profiles.yaml"


# ─────────────────────────────────────────────────────────────────────────────
# Per-item load errors
# ─────────────────────────────────────────────────────────────────────────────


@dataclass
class LoadError:
    """A single item that failed to load.

    Errors do not abort the registry load; valid items are still
    cached and the failures are surfaced in the return value of
    reload() so callers can log / alert.
    """

    item_kind: str  # "ModelDeploymentTemplate" or "BatchProfile"
    item_identifier: str  # name or name@version (best effort, may be "<unknown>")
    error: str

    def __str__(self) -> str:
        return f"{self.item_kind} '{self.item_identifier}': {self.error}"


# ─────────────────────────────────────────────────────────────────────────────
# Source abstraction
# ─────────────────────────────────────────────────────────────────────────────


L = TypeVar("L", ModelDeploymentTemplateList, BatchProfileList)


class TemplateSource(ABC, Generic[L]):
    """Abstract source returning the raw YAML for a typed list document.

    Implementations decide where the YAML comes from (K8s ConfigMap,
    local file, future DB). They return the raw YAML string; parsing,
    validation, and warnings happen in the registry.
    """

    @abstractmethod
    def load_raw(self) -> Optional[str]:
        """Returns the raw YAML string, or None if the source is empty.

        Raises any source-specific exception (e.g. K8s API failure)
        without catching; the caller decides retry / alert behavior.
        """
        ...

    @abstractmethod
    def describe(self) -> str:
        """Human-readable identifier for log messages."""
        ...


class K8sConfigMapSource(TemplateSource):
    """Loads YAML from a fixed-name ConfigMap data key."""

    def __init__(
        self,
        name: str,
        namespace: str,
        data_key: str,
        core_v1_api: Any,
    ) -> None:
        self._name = name
        self._namespace = namespace
        self._data_key = data_key
        self._api = core_v1_api

    def load_raw(self) -> Optional[str]:
        from kubernetes.client.rest import ApiException

        try:
            cm = self._api.read_namespaced_config_map(
                name=self._name, namespace=self._namespace
            )
        except ApiException as e:
            if e.status == 404:
                logger.warning(
                    "ConfigMap not found; treating as empty registry",
                    name=self._name,
                    namespace=self._namespace,
                )  # type: ignore[call-arg]
                return None
            raise

        data = getattr(cm, "data", None) or {}
        return data.get(self._data_key)

    def describe(self) -> str:
        return f"configmap/{self._namespace}/{self._name}#{self._data_key}"


class LocalFileSource(TemplateSource):
    """Loads YAML from a local file. Used for tests and standalone deployments.

    The file may be either:
      - the typed list document directly (apiVersion/kind/items at top), or
      - a Kubernetes ConfigMap with the typed list under data[data_key]
      - a multi-document YAML containing one of the above

    The loader auto-detects which form is present.
    """

    def __init__(self, path: Path, data_key: Optional[str] = None) -> None:
        self._path = Path(path)
        self._data_key = data_key

    def load_raw(self) -> Optional[str]:
        if not self._path.exists():
            logger.warning(
                "Template source file does not exist; treating as empty",
                path=str(self._path),
            )  # type: ignore[call-arg]
            return None

        text = self._path.read_text()
        # Multi-doc YAML support: prefer a ConfigMap whose data carries
        # the expected key. Otherwise fall back to the first document
        # treated as the raw data shape (a list for templates, a dict
        # for profiles).
        first_non_cm: Optional[str] = None
        for doc in yaml.safe_load_all(text):
            if isinstance(doc, dict) and doc.get("kind") == "ConfigMap":
                data = doc.get("data", {}) or {}
                if self._data_key and (value := data.get(self._data_key)):
                    return value
                continue
            if first_non_cm is None and doc is not None:
                first_non_cm = yaml.safe_dump(doc)
        return first_non_cm

    def describe(self) -> str:
        return f"file://{self._path}"


# ─────────────────────────────────────────────────────────────────────────────
# Deferred-field warnings
# ─────────────────────────────────────────────────────────────────────────────


def _warn_deferred_template_fields(t: ModelDeploymentTemplate) -> List[str]:
    """Inspect a template for fields whose runtime is not yet active.

    Returns a list of human-readable warnings; caller logs them.
    Empty list means nothing deferred. The schema still accepts these
    values for forward compatibility; this function exists so admins
    are not surprised when their configuration silently lacks effect.
    """
    warnings: List[str] = []
    spec = t.spec

    if spec.deployment_mode != DeploymentMode.DEDICATED:
        warnings.append(
            f"deployment_mode='{spec.deployment_mode.value}' is accepted but "
            f"only 'dedicated' is honored at runtime today (shared/external "
            f"depend on the worker/engine decoupling that is not yet "
            f"implemented)"
        )

    if spec.provider_config.type != ProviderType.K8S:
        warnings.append(
            f"provider_config.type='{spec.provider_config.type.value}' is "
            f"accepted but only 'k8s' is implemented today"
        )

    return warnings


def _warn_deferred_profile_fields(p: BatchProfile) -> List[str]:
    warnings: List[str] = []
    spec = p.spec
    sched = spec.scheduling

    if sched.completion_window != CompletionWindowOption.TWENTY_FOUR_HOURS:
        warnings.append(
            f"scheduling.completion_window='{sched.completion_window.value}' "
            f"is accepted but treated as 24h until the deadline-aware "
            f"scheduler ships"
        )
    if sched.priority != Priority.NORMAL:
        warnings.append(
            f"scheduling.priority='{sched.priority.value}' is accepted but "
            f"not yet honored at runtime"
        )
    if sched.provider_preference:
        warnings.append(
            "scheduling.provider_preference is accepted but not yet honored "
            "(multi-cloud routing is not yet implemented)"
        )
    if sched.allow_preempt:
        warnings.append("scheduling.allow_preempt=true is accepted but not yet honored")
    if sched.allow_spot:
        warnings.append("scheduling.allow_spot=true is accepted but not yet honored")
    if sched.retry_policy is not None:
        warnings.append(
            "scheduling.retry_policy is accepted but not yet honored "
            "(smart-client retry is not yet implemented)"
        )

    return warnings


# ─────────────────────────────────────────────────────────────────────────────
# Registries
# ─────────────────────────────────────────────────────────────────────────────


class TemplateRegistry:
    """In-memory cache of ModelDeploymentTemplate, indexed by name.

    Selects the unique 'active' version for each name. If multiple
    active versions exist for a single name, the load fails with an
    explicit error rather than picking arbitrarily.
    """

    def __init__(self, source: TemplateSource) -> None:
        self._source = source
        self._lock = threading.RLock()
        self._active: Dict[str, ModelDeploymentTemplate] = {}
        self._all: Dict[Tuple[str, str], ModelDeploymentTemplate] = {}

    def reload(self) -> List[LoadError]:
        """Synchronously reload from source. Returns per-item errors.

        A successful return with non-empty errors means: registry has
        been replaced with the valid subset; failed items are absent
        from the cache.
        """
        errors: List[LoadError] = []
        raw = self._source.load_raw()
        if raw is None:
            with self._lock:
                self._active.clear()
                self._all.clear()
            logger.info(
                "Template registry reloaded as empty (source returned None)",
                source=self._source.describe(),
            )  # type: ignore[call-arg]
            return errors

        new_active, new_all, errors = self._parse(raw)

        with self._lock:
            self._active = new_active
            self._all = new_all

        logger.info(
            "Template registry reloaded",
            source=self._source.describe(),
            active_count=len(new_active),
            total_count=len(new_all),
            error_count=len(errors),
        )  # type: ignore[call-arg]
        for e in errors:
            logger.error("Template load error", item=e.item_identifier, error=e.error)  # type: ignore[call-arg]
        return errors

    def _parse(
        self, raw: str
    ) -> Tuple[
        Dict[str, ModelDeploymentTemplate],
        Dict[Tuple[str, str], ModelDeploymentTemplate],
        List[LoadError],
    ]:
        errors: List[LoadError] = []
        new_active: Dict[str, ModelDeploymentTemplate] = {}
        new_all: Dict[Tuple[str, str], ModelDeploymentTemplate] = {}

        try:
            parsed = yaml.safe_load(raw)
        except yaml.YAMLError as e:
            errors.append(
                LoadError(
                    item_kind="ModelDeploymentTemplateList",
                    item_identifier="<root>",
                    error=f"YAML parse failed: {e}",
                )
            )
            return new_active, new_all, errors

        # Templates YAML is a plain top-level list. Empty file is
        # acceptable (no templates yet).
        if parsed is None:
            return new_active, new_all, errors
        if not isinstance(parsed, list):
            errors.append(
                LoadError(
                    item_kind="ModelDeploymentTemplateList",
                    item_identifier="<root>",
                    error=(
                        "templates YAML must be a top-level list; got "
                        f"{type(parsed).__name__}"
                    ),
                )
            )
            return new_active, new_all, errors
        items_raw = parsed

        for raw_item in items_raw:
            ident = "<unknown>"
            if isinstance(raw_item, dict):
                name = raw_item.get("name", "<unknown>")
                version = raw_item.get("version", "<unknown>")
                ident = f"{name}@{version}"

            try:
                t = ModelDeploymentTemplate.model_validate(raw_item)
            except ValidationError as e:
                errors.append(
                    LoadError(
                        item_kind="ModelDeploymentTemplate",
                        item_identifier=ident,
                        error=str(e),
                    )
                )
                continue

            key = (t.name, t.version)
            if key in new_all:
                errors.append(
                    LoadError(
                        item_kind="ModelDeploymentTemplate",
                        item_identifier=f"{t.name}@{t.version}",
                        error="duplicate (name, version)",
                    )
                )
                continue
            new_all[key] = t

            if t.status == TemplateStatus.ACTIVE:
                if t.name in new_active:
                    other = new_active[t.name]
                    errors.append(
                        LoadError(
                            item_kind="ModelDeploymentTemplate",
                            item_identifier=t.name,
                            error=f"multiple active versions: '{other.version}' "
                            f"and '{t.version}'; mark one deprecated/draft",
                        )
                    )
                    new_active.pop(t.name, None)
                    continue
                new_active[t.name] = t

                for w in _warn_deferred_template_fields(t):
                    logger.warning(
                        "Deferred template field detected",
                        template=f"{t.name}@{t.version}",
                        warning=w,
                    )  # type: ignore[call-arg]

        return new_active, new_all, errors

    # ── Lookup API ────────────────────────────────────────────────────────

    def get(self, name: str) -> Optional[ModelDeploymentTemplate]:
        """Return the active template for the given name, or None."""
        with self._lock:
            return self._active.get(name)

    def get_by_version(
        self, name: str, version: str
    ) -> Optional[ModelDeploymentTemplate]:
        """Return a specific version regardless of status. None if absent."""
        with self._lock:
            return self._all.get((name, version))

    def all_active(self) -> List[ModelDeploymentTemplate]:
        with self._lock:
            return list(self._active.values())

    def all(self) -> List[ModelDeploymentTemplate]:
        with self._lock:
            return list(self._all.values())

    def names(self) -> List[str]:
        """Active template names. Useful for UI / validation."""
        with self._lock:
            return sorted(self._active.keys())


class ProfileRegistry:
    """In-memory cache of BatchProfile, indexed by name.

    Profiles do not have versions in v1alpha1. The 'default' profile
    name from the source list is honored: get_default() returns the
    default profile when set.
    """

    def __init__(self, source: TemplateSource) -> None:
        self._source = source
        self._lock = threading.RLock()
        self._profiles: Dict[str, BatchProfile] = {}
        self._default_name: Optional[str] = None

    def reload(self) -> List[LoadError]:
        errors: List[LoadError] = []
        raw = self._source.load_raw()
        if raw is None:
            with self._lock:
                self._profiles.clear()
                self._default_name = None
            logger.info(
                "Profile registry reloaded as empty",
                source=self._source.describe(),
            )  # type: ignore[call-arg]
            return errors

        new_profiles, new_default, errors = self._parse(raw)

        with self._lock:
            self._profiles = new_profiles
            self._default_name = new_default

        logger.info(
            "Profile registry reloaded",
            source=self._source.describe(),
            count=len(new_profiles),
            default=new_default,
            error_count=len(errors),
        )  # type: ignore[call-arg]
        for e in errors:
            logger.error("Profile load error", item=e.item_identifier, error=e.error)  # type: ignore[call-arg]
        return errors

    def _parse(
        self, raw: str
    ) -> Tuple[Dict[str, BatchProfile], Optional[str], List[LoadError]]:
        errors: List[LoadError] = []
        new_profiles: Dict[str, BatchProfile] = {}

        try:
            parsed = yaml.safe_load(raw) or {}
        except yaml.YAMLError as e:
            errors.append(
                LoadError(
                    item_kind="BatchProfileList",
                    item_identifier="<root>",
                    error=f"YAML parse failed: {e}",
                )
            )
            return new_profiles, None, errors

        default = parsed.get("default")
        items_raw = parsed.get("items") or []
        if not isinstance(items_raw, list):
            errors.append(
                LoadError(
                    item_kind="BatchProfileList",
                    item_identifier="<root>",
                    error="'items' is not a list",
                )
            )
            return new_profiles, None, errors

        for raw_item in items_raw:
            ident = (
                raw_item.get("name", "<unknown>")
                if isinstance(raw_item, dict)
                else "<unknown>"
            )
            try:
                p = BatchProfile.model_validate(raw_item)
            except ValidationError as e:
                errors.append(
                    LoadError(
                        item_kind="BatchProfile",
                        item_identifier=ident,
                        error=str(e),
                    )
                )
                continue

            if p.name in new_profiles:
                errors.append(
                    LoadError(
                        item_kind="BatchProfile",
                        item_identifier=p.name,
                        error="duplicate name",
                    )
                )
                continue
            new_profiles[p.name] = p

            for w in _warn_deferred_profile_fields(p):
                logger.warning(
                    "Deferred profile field detected",
                    profile=p.name,
                    warning=w,
                )  # type: ignore[call-arg]

        # Validate 'default' refers to a loaded profile
        if default is not None and default not in new_profiles:
            errors.append(
                LoadError(
                    item_kind="BatchProfileList",
                    item_identifier="<default>",
                    error=f"default '{default}' not found in items",
                )
            )
            default = None

        return new_profiles, default, errors

    # ── Lookup API ────────────────────────────────────────────────────────

    def get(self, name: str) -> Optional[BatchProfile]:
        with self._lock:
            return self._profiles.get(name)

    def get_default(self) -> Optional[BatchProfile]:
        with self._lock:
            if self._default_name is None:
                return None
            return self._profiles.get(self._default_name)

    def default_name(self) -> Optional[str]:
        with self._lock:
            return self._default_name

    def all(self) -> List[BatchProfile]:
        with self._lock:
            return list(self._profiles.values())

    def names(self) -> List[str]:
        with self._lock:
            return sorted(self._profiles.keys())


# ─────────────────────────────────────────────────────────────────────────────
# Convenience factories
# ─────────────────────────────────────────────────────────────────────────────


def k8s_template_registry(
    core_v1_api: Any,
    namespace: str = DEFAULT_NAMESPACE,
    name: str = TEMPLATES_CONFIGMAP_NAME,
) -> TemplateRegistry:
    """Build a TemplateRegistry backed by a Kubernetes ConfigMap."""
    source = K8sConfigMapSource(
        name=name,
        namespace=namespace,
        data_key=TEMPLATES_DATA_KEY,
        core_v1_api=core_v1_api,
    )
    return TemplateRegistry(source)


def k8s_profile_registry(
    core_v1_api: Any,
    namespace: str = DEFAULT_NAMESPACE,
    name: str = PROFILES_CONFIGMAP_NAME,
) -> ProfileRegistry:
    """Build a ProfileRegistry backed by a Kubernetes ConfigMap."""
    source = K8sConfigMapSource(
        name=name,
        namespace=namespace,
        data_key=PROFILES_DATA_KEY,
        core_v1_api=core_v1_api,
    )
    return ProfileRegistry(source)


def local_template_registry(path: Path) -> TemplateRegistry:
    """Build a TemplateRegistry backed by a local file (tests / standalone)."""
    return TemplateRegistry(LocalFileSource(path, data_key=TEMPLATES_DATA_KEY))


def local_profile_registry(path: Path) -> ProfileRegistry:
    """Build a ProfileRegistry backed by a local file (tests / standalone)."""
    return ProfileRegistry(LocalFileSource(path, data_key=PROFILES_DATA_KEY))
