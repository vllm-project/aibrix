#!/usr/bin/env python3
"""Bridge between the Node UI server and the aiconfigurator Python SDK.

Protocol:
    stdin  : one JSON object  {"mode": "...", "params": {...}}
    stdout : any diagnostics, then a line  __AIC_UI_JSON__  followed by one
             JSON object  {"ok": true, "data": ...}  or  {"ok": false, "error": ...}
    stderr : SDK logging (surfaced on failure).
"""

import sys
import json
import math
import glob
import os
import traceback
import logging
import inspect

logging.getLogger().setLevel(logging.ERROR)
for noisy in ("aiconfigurator",):
    logging.getLogger(noisy).setLevel(logging.ERROR)


# --------------------------------------------------------------------------- #
# JSON sanitization (numpy / pandas scalars, NaN/Inf, tuples)
# --------------------------------------------------------------------------- #
def jsonable(obj):
    try:
        import numpy as np
    except Exception:  # pragma: no cover
        np = None

    def _convert(v):
        if v is None:
            return None
        if isinstance(v, bool):
            return v
        if isinstance(v, int):
            return v
        if isinstance(v, float):
            return v if math.isfinite(v) else None
        if isinstance(v, str):
            return v
        if np is not None:
            if isinstance(v, np.integer):
                return int(v)
            if isinstance(v, np.floating):
                f = float(v)
                return f if math.isfinite(f) else None
            if isinstance(v, np.bool_):
                return bool(v)
            if isinstance(v, np.ndarray):
                return [_convert(x) for x in v.tolist()]
        if isinstance(v, dict):
            return {str(k): _convert(x) for k, x in v.items()}
        if isinstance(v, (list, tuple)):
            return [_convert(x) for x in v]
        if hasattr(v, "item"):
            try:
                return _convert(v.item())
            except Exception:
                pass
        return str(v)

    return _convert(obj)


def df_records(df, cap=None):
    if df is None:
        return None
    if cap is not None and len(df) > cap:
        df = df.head(cap)
    return jsonable(df.to_dict(orient="records"))


# --------------------------------------------------------------------------- #
# Result wrappers
# --------------------------------------------------------------------------- #
def wrap_cli_result(result, pareto_cap=100):
    return {
        "type": "cli_result",
        "chosen_exp": result.chosen_exp,
        "best_throughputs": jsonable(result.best_throughputs),
        "best_latencies": jsonable(result.best_latencies),
        "best_configs": {name: df_records(df) for name, df in result.best_configs.items()},
        "pareto_fronts": {
            name: df_records(df, cap=pareto_cap)
            for name, df in getattr(result, "pareto_fronts", {}).items()
        },
        "outcomes": {
            name: (str(v) if v is not None else None)
            for name, v in getattr(result, "outcomes", {}).items()
        },
    }


def wrap_support(result):
    # repo: tuple[bool, bool]  |  installed 0.11: SupportResult dataclass
    if isinstance(result, tuple):
        agg_ok, disagg_ok = bool(result[0]), bool(result[1])
    else:
        agg_ok = bool(getattr(result, "agg_supported", False))
        disagg_ok = bool(getattr(result, "disagg_supported", False))
    return {"type": "support", "agg_supported": agg_ok, "disagg_supported": disagg_ok}


def _estimate_per_ops(est):
    """Per-op latency/source breakdown for the result table.

    afd results carry ``per_ops_data`` directly; static/agg results expose
    the same breakdown via the underlying ``InferenceSummary`` latency dicts
    (equivalent to the CLI ``--detail`` time view).
    """
    per_ops = est.per_ops_data
    per_ops_source = est.per_ops_source
    if per_ops:
        return per_ops, per_ops_source

    summary = getattr(est, "summary", None)
    if summary is None:
        return per_ops, per_ops_source

    def _pick(getter_latency, getter_source, phase):
        try:
            latency = getter_latency() or {}
        except Exception:
            latency = {}
        if not latency:
            return None, None
        source = None
        try:
            source = getter_source() or None
        except Exception:
            source = None
        return {phase: latency}, ({phase: source} if source else None)

    phases = []
    if est.mode in ("static", "static_ctx", "agg"):
        phases.append((summary.get_context_latency_dict,
                       summary.get_context_source_dict, "context"))
    if est.mode in ("static", "static_gen", "agg"):
        phases.append((summary.get_generation_latency_dict,
                       summary.get_generation_source_dict, "generation"))

    ops, sources = {}, {}
    for get_latency, get_source, phase in phases:
        phase_ops, phase_source = _pick(get_latency, get_source, phase)
        if phase_ops:
            ops.update(phase_ops)
        if phase_source:
            sources.update(phase_source)
    return (ops or None), (sources or None)


def wrap_estimate(est):
    raw = est.raw if isinstance(est.raw, dict) else {}
    per_ops, per_ops_source = _estimate_per_ops(est)
    data = {
        "type": "estimate",
        "mode": est.mode,
        "ttft_ms": est.ttft,
        "tpot_ms": est.tpot,
        "request_latency_ms": raw.get("request_latency"),
        "context_latency_ms": raw.get("context_latency"),
        "generation_latency_ms": raw.get("generation_latency"),
        "power_w": est.power_w,
        "isl": est.isl,
        "osl": est.osl,
        "batch_size": est.batch_size,
        "global_bs": raw.get("global_bs"),
        "tp": est.tp_size,
        "pp": est.pp_size,
        "num_total_gpus": raw.get("num_total_gpus"),
        "seq_per_second": raw.get("seq/s"),
        "tokens_per_second": raw.get("tokens/s"),
        "tokens_per_second_per_gpu": raw.get("tokens/s/gpu"),
        "memory_gb": raw.get("memory"),
        "kv_cache_warning": est.kv_cache_warning,
        "per_ops": per_ops,
        "per_ops_source": per_ops_source,
        "raw": raw or None,
    }
    return jsonable(data)


# --------------------------------------------------------------------------- #
# Dispatch
# --------------------------------------------------------------------------- #
def clean(params):
    """Drop None / empty-string values so SDK defaults apply."""
    return {k: v for k, v in params.items() if v is not None and v != ""}


def run(mode, params):
    params = clean(params)

    if mode == "meta":
        return run_meta()

    if mode == "support":
        from aiconfigurator.cli import cli_support
        return wrap_support(cli_support(
            params["model_path"], params["system"],
            backend=params.get("backend", "trtllm"),
            backend_version=params.get("backend_version"),
        ))

    if mode == "default":
        from aiconfigurator.cli import cli_default
        return wrap_cli_result(cli_default(**params))

    if mode == "recommend":
        from aiconfigurator.cli import cli_recommend
        return wrap_cli_result(cli_recommend(**params))

    if mode == "exp":
        import tempfile
        from aiconfigurator.cli import cli_exp
        kwargs = {}
        sig = inspect.signature(cli_exp).parameters
        tmp_path = None
        if params.get("yaml_text"):
            # UI sends raw YAML text; persist to a temp file for cli_exp.
            tf = tempfile.NamedTemporaryFile(
                mode="w", suffix=".yaml", delete=False, encoding="utf-8")
            tf.write(params["yaml_text"])
            tf.close()
            tmp_path = tf.name
            kwargs["yaml_path"] = tmp_path
        elif "yaml_path" in params:
            kwargs["yaml_path"] = params["yaml_path"]
        if "config" in params and "config" in sig:
            kwargs["config"] = params["config"]
        if "top_n" in params:
            kwargs["top_n"] = params["top_n"]
        try:
            return wrap_cli_result(cli_exp(**kwargs))
        finally:
            if tmp_path:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass

    if mode == "generate":
        try:
            from aiconfigurator.cli import cli_generate
        except ImportError:
            from aiconfigurator.generator.api import generate_naive_config as cli_generate
        result = cli_generate(
            params["model_path"], int(params["total_gpus"]), params["system"],
            backend=params.get("backend", "trtllm"),
        )
        return {"type": "generate", "config": jsonable(result)}

    if mode == "estimate":
        try:
            from aiconfigurator.cli import cli_estimate
        except ImportError:
            try:
                from aiconfigurator.cli.api import cli_estimate
            except ImportError:
                raise RuntimeError(
                    "cli_estimate is not available in the installed aiconfigurator version."
                ) from None
        if params.get("mode") == "disagg":
            # disagg requires separate prefill/decode worker specs; when the UI
            # does not provide them, derive one worker per role from the base
            # parallelism/batch size (mirrors the CLI's role-arg fallbacks).
            params.setdefault("prefill_tp_size", params.get("tp_size"))
            params.setdefault("prefill_pp_size", params.get("pp_size"))
            params.setdefault("prefill_attention_dp_size", params.get("attention_dp_size"))
            params.setdefault("prefill_batch_size", params.get("batch_size"))
            params.setdefault("prefill_num_workers", 1)
            params.setdefault("decode_tp_size", params.get("tp_size"))
            params.setdefault("decode_pp_size", params.get("pp_size"))
            params.setdefault("decode_attention_dp_size", params.get("attention_dp_size"))
            params.setdefault("decode_batch_size", params.get("batch_size"))
            params.setdefault("decode_num_workers", 1)
        return wrap_estimate(cli_estimate(**params))

    raise ValueError(f"unknown mode: {mode}")


def run_meta():
    import aiconfigurator_core
    core_dir = os.path.dirname(aiconfigurator_core.__file__)
    systems_dir = os.path.join(core_dir, "systems")
    systems = sorted(
        os.path.basename(p)[:-5]
        for p in glob.glob(os.path.join(systems_dir, "*.yaml"))
        if not os.path.basename(p).startswith("op_kernel")
    )
    versions = {}
    try:
        import aiconfigurator
        versions["aiconfigurator"] = getattr(aiconfigurator, "__version__", "unknown")
    except Exception:
        pass
    try:
        versions["aiconfigurator_core"] = getattr(aiconfigurator_core, "__version__", "unknown")
    except Exception:
        pass
    return {
        "type": "meta",
        "systems": systems,
        "versions": versions,
        "estimate_available": _estimate_available(),
    }


def _estimate_available():
    try:
        from aiconfigurator.cli import cli_estimate  # re-export probe
        return True
    except Exception:
        try:
            from aiconfigurator.cli.api import cli_estimate
            return True
        except Exception:
            return False


def main():
    try:
        payload = json.load(sys.stdin)
    except Exception as e:
        _emit({"ok": False, "error": f"invalid request JSON: {e}"})
        return
    try:
        data = run(payload.get("mode", ""), payload.get("params", {}) or {})
        _emit({"ok": True, "data": data})
    except Exception as e:
        _emit({
            "ok": False,
            "error": f"{type(e).__name__}: {e}",
            "traceback": traceback.format_exc(limit=6),
        })


def _emit(obj):
    sys.stdout.write("\n__AIC_UI_JSON__\n")
    sys.stdout.write(json.dumps(obj, ensure_ascii=False, default=str))
    sys.stdout.write("\n")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
