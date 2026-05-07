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

"""Source-level asserts that the Type2 Connector correctly inherits
from Type1 and that the shared ``__init__`` wires the right
Scheduler/Worker subclass through the ``SCHEDULER_CLASS`` /
``WORKER_CLASS`` class attributes.

We parse the two connector source files with ``ast`` instead of
importing them at runtime. This deliberately avoids pulling in vLLM
(and its CUDA/torch dependencies) at test time: the module can't be
imported cleanly under the ``vllm==0.10.2`` pin in
``requirements/test.txt`` because the connector depends on symbols
(``KVConnectorWorkerMetadata``, ``vllm.utils.math_utils``) that only
appear in newer vLLM releases. The tests still catch the regression
pattern the reviewer asked for — namely, a future change that shadows
``SCHEDULER_CLASS`` / ``WORKER_CLASS`` with the wrong class, or breaks
the Connector / Scheduler / Worker hierarchy — because the only runtime
magic involved is plain Python class inheritance, which is visible at
the source level.
"""

import ast
from pathlib import Path
from typing import Optional

CONNECTOR_DIR = (
    Path(__file__).resolve().parents[1]
    / "aibrix_kvcache"
    / "integration"
    / "vllm"
    / "kv_connector"
)
TYPE1_SRC = CONNECTOR_DIR / "aibrix_offloading_connector_type1.py"
TYPE2_SRC = CONNECTOR_DIR / "aibrix_offloading_connector_type2.py"


def _parse(path: Path) -> ast.Module:
    return ast.parse(path.read_text(encoding="utf-8"))


def _find_class(tree: ast.Module, name: str) -> Optional[ast.ClassDef]:
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and node.name == name:
            return node
    return None


def _base_names(cls: ast.ClassDef) -> list[str]:
    """Return the textual names of the class's base classes.

    Handles both plain names (``Foo``) and dotted attribute access
    (``module.Foo`` -> ``Foo``). Subscripted bases (``Generic[T]``) are
    resolved to the base name (``Generic``).
    """
    out = []
    for base in cls.bases:
        if isinstance(base, ast.Name):
            out.append(base.id)
        elif isinstance(base, ast.Attribute):
            out.append(base.attr)
        elif isinstance(base, ast.Subscript) and isinstance(
            base.value, ast.Name
        ):
            out.append(base.value.id)
    return out


def _class_attr_rhs_name(cls: ast.ClassDef, attr_name: str) -> Optional[str]:
    """Return the identifier on the RHS of ``attr_name = X`` or
    ``attr_name: T = X`` if the RHS is a plain Name; ``None`` otherwise.
    """
    for node in cls.body:
        if isinstance(node, ast.AnnAssign) and isinstance(
            node.target, ast.Name
        ):
            if node.target.id == attr_name and isinstance(node.value, ast.Name):
                return node.value.id
        elif isinstance(node, ast.Assign):
            for target in node.targets:
                if (
                    isinstance(target, ast.Name)
                    and target.id == attr_name
                    and isinstance(node.value, ast.Name)
                ):
                    return node.value.id
    return None


def test_type1_connector_declares_scheduler_and_worker_class_attrs():
    """Type1's Connector must declare SCHEDULER_CLASS / WORKER_CLASS
    pointing at its own Scheduler / Worker subclasses. If these go
    missing or get pointed at the wrong class, the shared __init__
    would instantiate the wrong Scheduler/Worker everywhere.
    """
    tree = _parse(TYPE1_SRC)
    cls = _find_class(tree, "AIBrixOffloadingConnector")
    assert cls is not None, (
        "AIBrixOffloadingConnector class not found in type1 source"
    )
    assert (
        _class_attr_rhs_name(cls, "SCHEDULER_CLASS")
        == "AIBrixOffloadingConnectorScheduler"
    )
    assert (
        _class_attr_rhs_name(cls, "WORKER_CLASS")
        == "AIBrixOffloadingConnectorWorker"
    )


def test_type2_connector_inherits_from_type1_connector():
    """Type2's Connector must inherit from Type1's Connector so it
    picks up the full connector contract (get_num_new_matched_tokens,
    build_connector_worker_meta, update_state_after_alloc, etc.) via
    inheritance rather than copy-paste.
    """
    tree = _parse(TYPE2_SRC)
    cls = _find_class(tree, "AIBrixOffloadingConnector")
    assert cls is not None, (
        "AIBrixOffloadingConnector class not found in type2 source"
    )
    # Type2 imports Type1's Connector as AIBrixOffloadingConnectorType1
    # and inherits from it under that alias.
    assert "AIBrixOffloadingConnectorType1" in _base_names(cls), (
        f"Type2 Connector bases are {_base_names(cls)}, expected to "
        "include AIBrixOffloadingConnectorType1"
    )


def test_type1_scheduler_init_assigns_tracker_enabled():
    """The Scheduler must compute ``_tracker_enabled`` in ``__init__``
    by checking the ``AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND`` env. If a
    future change drops or renames that gate, the scheduler tracker
    could be enabled in L2-backed deployments and clobber the
    worker-reconciliation contract.
    """
    tree = _parse(TYPE1_SRC)
    scheduler = _find_class(tree, "AIBrixOffloadingConnectorScheduler")
    assert scheduler is not None, (
        "AIBrixOffloadingConnectorScheduler not found in type1 source"
    )

    # Find the __init__ method and inspect its body.
    init_fn = None
    for node in scheduler.body:
        if isinstance(node, ast.FunctionDef) and node.name == "__init__":
            init_fn = node
            break
    assert init_fn is not None, "Scheduler.__init__ not found"

    # Walk the init body looking for ``self._tracker_enabled = ...``
    # whose RHS references AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND.
    init_src = ast.unparse(init_fn)
    assert "self._tracker_enabled" in init_src, (
        "Scheduler.__init__ does not assign self._tracker_enabled — "
        "the L2 gate is missing"
    )
    assert "AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND" in init_src, (
        "Scheduler.__init__ does not reference "
        "AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND — the gate is no longer "
        "driven by the documented env variable"
    )


def test_aibrix_worker_meta_uses_block_offset_schema():
    """``AIBrixWorkerMeta`` must carry ``(start_block_idx, num_blocks)``
    tuples per request, not just token counts. Without the offset, the
    scheduler can't tell which slice of ``request.block_hashes`` to
    mark as cached or to invalidate when the worker reports a partial
    save / failed load. This is the schema the Gemini review on
    PR #2146 flagged (Bugs 1 and 2 of the saved/failed_load reporting).
    """
    tree = _parse(TYPE1_SRC)
    cls = _find_class(tree, "AIBrixWorkerMeta")
    assert cls is not None, "AIBrixWorkerMeta not found in type1 source"
    src = ast.unparse(cls)
    assert "saved_blocks" in src, (
        "AIBrixWorkerMeta missing saved_blocks field — the scheduler "
        "can't map worker reports to block_hashes without an offset"
    )
    assert "failed_load_blocks" in src, (
        "AIBrixWorkerMeta missing failed_load_blocks field — the "
        "scheduler can't invalidate the correct slice of block_hashes"
    )
    # The old token-count schema must be gone.
    assert "saved_tokens:" not in src, (
        "AIBrixWorkerMeta still carries the old saved_tokens field — "
        "the offset bug from #2146 review is back"
    )
    assert "failed_load_tokens:" not in src, (
        "AIBrixWorkerMeta still carries the old failed_load_tokens "
        "field — the tail-invalidation bug from #2146 review is back"
    )


def test_type2_scheduler_and_worker_inherit_from_type1():
    """Type2's Scheduler / Worker must inherit from their Type1
    counterparts (aliased as Type1Scheduler / Type1Worker in type2).
    """
    tree = _parse(TYPE2_SRC)
    scheduler = _find_class(tree, "AIBrixOffloadingConnectorScheduler")
    worker = _find_class(tree, "AIBrixOffloadingConnectorWorker")
    assert scheduler is not None, (
        "AIBrixOffloadingConnectorScheduler not found in type2 source"
    )
    assert worker is not None, (
        "AIBrixOffloadingConnectorWorker not found in type2 source"
    )
    assert "Type1Scheduler" in _base_names(scheduler), (
        f"Type2 Scheduler bases are {_base_names(scheduler)}, expected "
        "to include Type1Scheduler"
    )
    assert "Type1Worker" in _base_names(worker), (
        f"Type2 Worker bases are {_base_names(worker)}, expected to "
        "include Type1Worker"
    )
