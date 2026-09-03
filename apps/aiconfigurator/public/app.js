/* aiconfigurator UI frontend — vanilla JS, no dependencies. */

const $ = (id) => document.getElementById(id);
let currentMode = "default";
let meta = { systems: [], estimate_available: false };
let lastResult = null;

const MODE_LABELS = {
  default: "default", recommend: "recommend", estimate: "estimate",
  support: "support", generate: "generate", exp: "exp",
};

const EXP_YAML_EXAMPLE = `# exps controls execution order; each top-level key is one experiment
exps:
  - agg_demo
  - disagg_demo

agg_demo:
  serving_mode: agg
  model_path: Qwen/Qwen3-32B-FP8
  system_name: h200_sxm
  total_gpus: 8
  isl: 4000
  osl: 1000
  ttft: 2000
  tpot: 30

disagg_demo:
  serving_mode: disagg
  model_path: Qwen/Qwen3-32B-FP8
  system_name: h200_sxm
  total_gpus: 16
  isl: 4000
  osl: 1000
  ttft: 2000
  tpot: 30
`;

/* ---------------- mode tabs ---------------- */
function switchMode(mode) {
  currentMode = mode;
  document.querySelectorAll(".tab").forEach((t) =>
    t.classList.toggle("active", t.dataset.mode === mode));
  document.querySelectorAll(".group[data-group]").forEach((fs) => {
    const groups = fs.dataset.group.split(/\s+/);
    fs.style.display = (groups.includes("common") || groups.includes(mode)) ? "" : "none";
  });
  updateCmdPreview();
}

document.querySelectorAll(".tab").forEach((t) =>
  t.addEventListener("click", () => switchMode(t.dataset.mode)));

document.querySelectorAll("input, select, textarea").forEach((el) =>
  el.addEventListener("input", updateCmdPreview));

/* ---------------- presets ---------------- */
const PRESETS = {
  default8: () => {
    switchMode("default");
    setV({ model_path: "Qwen/Qwen3-32B-FP8", system: "h200_sxm", backend: "trtllm",
      database_mode: "SILICON", isl: 4000, osl: 1000, prefix: 0, ttft: 2000, tpot: 30,
      total_gpus: 8, top_n: 5 });
  },
  recommendRate: () => {
    switchMode("recommend");
    setV({ model_path: "Qwen/Qwen3-32B-FP8", system: "h200_sxm", backend: "trtllm",
      database_mode: "HYBRID", isl: 4000, osl: 1000, ttft: 2000, tpot: 30,
      target_type: "target_request_rate", target_value: 50, top_n: 5 });
  },
  disaggCompare: () => {
    switchMode("default");
    setV({ model_path: "deepseek-ai/DeepSeek-V3", system: "h200_sxm", backend: "trtllm",
      database_mode: "SILICON", isl: 4000, osl: 1000, ttft: 5000, tpot: 60,
      total_gpus: 64, top_n: 5 });
  },
  estimatePoint: () => {
    switchMode("estimate");
    setV({ model_path: "Qwen/Qwen3-32B-FP8", system: "h200_sxm", backend: "trtllm",
      database_mode: "HYBRID", isl: 4000, osl: 1000, prefix: 0,
      est_mode: "static", est_bs: 128, est_tp: 2, est_pp: 1, est_dp: 1 });
  },
};
document.querySelectorAll(".preset").forEach((b) =>
  b.addEventListener("click", () => PRESETS[b.dataset.preset]()));

function setV(map) {
  for (const [k, v] of Object.entries(map)) {
    const el = $(k);
    if (el) el.type === "checkbox" ? (el.checked = !!v) : (el.value = v);
  }
  updateCmdPreview();
}

/* ---------------- param gathering ---------------- */
function num(id) { const v = $(id).value; return v === "" ? null : Number(v); }

function gatherParams() {
  const common = {
    model_path: $("model_path").value.trim(),
    system: $("system").value,
    backend: $("backend").value,
    database_mode: $("database_mode").value,
    isl: num("isl"), osl: num("osl"), prefix: num("prefix"),
    ttft: num("ttft"), tpot: num("tpot"),
  };
  const strict = $("strict_sla").checked;

  switch (currentMode) {
    case "default":
      return { ...common, strict_sla: strict,
        total_gpus: num("total_gpus"), top_n: num("top_n") };
    case "recommend": {
      const t = {};
      t[$("target_type").value] = num("target_value");
      return { ...common, strict_sla: strict, top_n: num("top_n"), ...t };
    }
    case "estimate":
      return {
        model_path: common.model_path,
        system_name: common.system,
        backend_name: common.backend,
        database_mode: common.database_mode,
        isl: common.isl, osl: common.osl, prefix: common.prefix,
        mode: $("est_mode").value,
        batch_size: num("est_bs"),
        tp_size: num("est_tp"), pp_size: num("est_pp"),
        attention_dp_size: num("est_dp"),
      };
    case "support":
      return { model_path: common.model_path, system: common.system, backend: common.backend };
    case "generate":
      return { model_path: common.model_path, system: common.system,
        backend: common.backend, total_gpus: num("total_gpus") };
    case "exp":
      return { yaml_text: $("exp_yaml").value };
  }
}

/* ---------------- CLI command preview ---------------- */
function updateCmdPreview() {
  const p = gatherParams();
  const parts = [`aiconfigurator cli ${MODE_LABELS[currentMode]}`];
  const flag = (k, v) => { if (v !== null && v !== undefined && v !== "") parts.push(`--${k} ${v}`); };

  if (currentMode === "exp") {
    parts[0] = "aiconfigurator cli exp --yaml-path <your-experiment.yaml>";
    $("cmd-preview").value = parts[0];
    return;
  }
  flag("model-path", p.model_path);
  if (currentMode !== "estimate") {
    flag("system", p.system);
    flag("backend", p.backend);
    flag("database-mode", p.database_mode);
    flag("isl", p.isl); flag("osl", p.osl); flag("prefix", p.prefix);
    flag("ttft", p.ttft); flag("tpot", p.tpot);
    if (p.strict_sla) parts.push("--strict-sla");
  } else {
    flag("system", p.system_name);
    flag("backend", p.backend_name);
    flag("database-mode", p.database_mode);
    flag("isl", p.isl); flag("osl", p.osl);
    parts.push(`--estimate-mode ${p.mode}`);
    flag("tp", p.tp_size); flag("pp", p.pp_size); flag("batch-size", p.batch_size);
  }
  if (currentMode === "default") { flag("total-gpus", p.total_gpus); flag("top-n", p.top_n); }
  if (currentMode === "generate") flag("total-gpus", p.total_gpus);
  if (currentMode === "recommend") {
    if (p.target_request_rate) flag("target-request-rate", p.target_request_rate);
    if (p.target_concurrency) flag("target-concurrency", p.target_concurrency);
    flag("top-n", p.top_n);
  }
  $("cmd-preview").value = parts.join(" \\\n  ");
}

/* ---------------- run ---------------- */
$("run-btn").addEventListener("click", async () => {
  const params = gatherParams();
  if (!params.model_path && currentMode !== "exp") {
    showError("Please fill in model_path first (HuggingFace ID or local model directory).", "");
    return;
  }
  const btn = $("run-btn");
  btn.disabled = true;
  $("run-status").textContent = "Running (sweeps typically take 5 seconds to a few minutes)…";
  hideError();
  try {
    const resp = await fetch("/api/run", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: currentMode, params }),
    });
    const payload = await resp.json();
    if (!payload.ok) {
      showError(payload.error || "Run failed", payload.traceback || payload.stderr || "");
    } else {
      lastResult = payload.data;
      renderResult(payload.data);
    }
  } catch (e) {
    showError(`Request failed: ${e.message}`, "");
  } finally {
    btn.disabled = false;
    $("run-status").textContent = "";
  }
});

/* ---------------- result rendering ---------------- */
$("tab-table").addEventListener("click", () => toggleView("table"));
$("tab-json").addEventListener("click", () => toggleView("json"));
function toggleView(which) {
  $("tab-table").classList.toggle("active", which === "table");
  $("tab-json").classList.toggle("active", which === "json");
  $("result-content").style.display = which === "table" ? "" : "none";
  $("result-json").style.display = which === "json" ? "" : "none";
}

function showError(title, detail) {
  const box = $("result-error");
  box.style.display = "";
  $("result-placeholder").style.display = "none";
  $("result-content").style.display = "none";
  box.innerHTML = `<div class="err-title">✗ ${title}</div>` +
    (detail ? `<pre style="white-space:pre-wrap;margin:8px 0 0;font-size:12px;overflow:auto;max-height:300px">${escapeHtml(detail)}</pre>` : "");
}
function hideError() { $("result-error").style.display = "none"; }

function renderResult(data) {
  $("result-placeholder").style.display = "none";
  $("result-content").style.display = "";
  $("result-json").textContent = JSON.stringify(data, null, 2);
  const c = $("result-content");
  c.innerHTML = "";

  if (data.type === "support") return renderSupport(c, data);
  if (data.type === "estimate") return renderEstimate(c, data);
  if (data.type === "generate") return renderGenerate(c, data);
  if (data.type === "cli_result") return renderCliResult(c, data);
  c.textContent = JSON.stringify(data, null, 2);
}

function card(k, v, unit, cls) {
  return `<div class="card"><div class="k">${k}</div><div class="v ${cls || ""}">${v} <span class="u">${unit || ""}</span></div></div>`;
}

function renderSupport(c, d) {
  c.innerHTML = `<div class="verdict">
    <div class="card"><div class="k">Aggregated support</div><div class="v ${d.agg_supported ? "good" : "bad"}">${d.agg_supported ? "✓ Supported" : "✗ Not supported"}</div></div>
    <div class="card"><div class="k">Disaggregated support</div><div class="v ${d.disagg_supported ? "good" : "bad"}">${d.disagg_supported ? "✓ Supported" : "✗ Not supported"}</div></div>
  </div>
  <p class="hint">Note: support is a lightweight check based on a majority vote over the support matrix; the actual default/exp run results are authoritative.</p>`;
}

function renderGenerate(c, d) {
  const cfg = d.config || {};
  const rows = Object.entries(flatten(cfg)).map(([k, v]) =>
    `<div><span class="k">${escapeHtml(k)}</span><span class="v">${escapeHtml(String(v))}</span></div>`).join("");
  c.innerHTML = `<div class="kv-list">${rows}</div>`;
}

function renderEstimate(c, d) {
  const cards = [
    card("TTFT / prefill", fmt(d.ttft_ms), "ms"),
    card("TPOT", fmt(d.tpot_ms), "ms"),
    card("End-to-end request latency", fmt(d.request_latency_ms), "ms"),
    card("Request throughput", fmt(d.seq_per_second), "req/s"),
    card("Total throughput", fmt(d.tokens_per_second), "tok/s"),
    card("Per-GPU throughput", fmt(d.tokens_per_second_per_gpu), "tok/s/gpu"),
    card("GPUs per replica", fmt(d.num_total_gpus), "GPU/replica"),
    card("Memory usage", fmt(d.memory_gb), "GB/GPU"),
    card("Power", d.power_w ? fmt(d.power_w) : "N/A", "W/GPU"),
    card("Mode", d.mode, ""),
  ].join("");
  let html = `<div class="summary-cards">${cards}</div>`;
  if (d.context_latency_ms || d.generation_latency_ms) {
    html += `<p class="hint">Single-round breakdown: prefill ${fmt(d.context_latency_ms)} ms + decode ${fmt(d.generation_latency_ms)} ms` +
      (d.global_bs ? `; ${d.global_bs} requests complete per round, total offline job duration ≈ total requests ÷ (${fmt(d.seq_per_second)} req/s × replica count)` : "") +
      `.</p>`;
  }
  if (d.kv_cache_warning) html += `<p class="hint">⚠ ${escapeHtml(d.kv_cache_warning)}</p>`;
  if (d.per_ops && Object.keys(d.per_ops).length) {
    html += `<div class="exp-block"><h3 class="exp-title">Per-op latency breakdown (ms)</h3>` +
      renderOpsTable(d.per_ops, d.per_ops_source) + `</div>`;
  }
  c.innerHTML = html;
}

function renderOpsTable(perOps, source) {
  const rows = [];
  for (const [step, ops] of Object.entries(perOps)) {
    if (typeof ops !== "object" || ops === null) continue;
    for (const [op, val] of Object.entries(ops)) {
      if (typeof val !== "number") continue;
      const src = source && source[step] ? source[step][op] : "";
      rows.push({ step, op, val, src });
    }
  }
  rows.sort((a, b) => b.val - a.val);
  const body = rows.slice(0, 40).map((r) =>
    `<tr><td>${escapeHtml(r.step)}</td><td style="text-align:left">${escapeHtml(r.op)}</td><td class="hl">${fmt(r.val)}</td><td>${escapeHtml(r.src || "")}</td></tr>`
  ).join("");
  return `<div class="table-wrap"><table>
    <thead><tr><th>Phase</th><th style="text-align:left">Op</th><th>Latency (ms)</th><th>Data source</th></tr></thead>
    <tbody>${body}</tbody></table></div>`;
}

const PRIORITY_COLS_AGG = [
  "parallel", "num_total_gpus", "tp", "pp", "dp", "moe_ep", "bs", "global_bs",
  "total_gpus_needed", "replicas_needed", "load_served_pct",
  "concurrency", "tokens/s", "tokens/s/gpu", "tokens/s/user", "seq/s",
  "ttft", "tpot", "request_latency", "memory", "power_w",
];
const PRIORITY_COLS_DISAGG = [
  "num_total_gpus", "total_gpus_needed", "replicas_needed", "load_served_pct",
  "(p)workers", "(p)parallel", "(p)bs", "(d)workers", "(d)parallel", "(d)bs",
  "tokens/s", "tokens/s/gpu", "tokens/s/user",
  "ttft", "tpot", "request_latency", "(p)memory", "(d)memory", "power_w",
];
const HIDE_COLS = new Set([
  "model", "isl", "osl", "prefix", "backend", "version", "system",
  "(p)backend", "(p)version", "(p)system", "(d)backend", "(d)version", "(d)system",
  "gemm", "kvcache", "fmha", "moe", "comm",
  "(p)gemm", "(p)kvcache", "(p)fmha", "(p)moe", "(p)comm",
  "(d)gemm", "(d)kvcache", "(d)fmha", "(d)moe", "(d)comm",
  "encoder_latency", "encoder_memory", "balance_score",
  "num_ctx_reqs", "num_gen_reqs", "num_tokens", "ctx_tokens", "gen_tokens",
  "moe_tp", "cp", "(p)moe_tp", "(p)cp", "(d)moe_tp", "seq/s/gpu",
  "(p)seq/s/worker", "(d)seq/s/worker", "(e)workers", "(e)tp", "(e)pp", "(e)parallel", "(e)memory",
  "_per_ops_source",
]);

function renderCliResult(c, d) {
  const exps = Object.keys(d.best_configs || {});
  const bestTp = d.best_throughputs && d.chosen_exp ? d.best_throughputs[d.chosen_exp] : null;
  let html = `<div class="summary-cards">
    ${card("Best experiment", d.chosen_exp || "-", "")}
    ${card("Best cluster throughput", bestTp != null ? fmt(bestTp) : "-", "tok/s/gpu")}
    ${card("Number of experiments", exps.length, "")}
  </div>`;
  for (const name of exps) {
    const rows = d.best_configs[name] || [];
    const isBest = name === d.chosen_exp;
    html += `<div class="exp-block">
      <h3 class="exp-title">${escapeHtml(name)} · Top ${rows.length} configs${isBest ? '<span class="tag-best">Best</span>' : ""}</h3>
      ${rows.length ? renderConfigTable(rows, name) : "<p class='hint'>No feasible configs for this experiment (SLO may be too tight or no performance data available).</p>"}
    </div>`;
  }
  c.innerHTML = html;
}

function renderConfigTable(rows, expName) {
  if (!rows.length) return "";
  const disagg = expName.includes("disagg") || "(p)workers" in rows[0] || "(p)tp" in rows[0];
  const priority = disagg ? PRIORITY_COLS_DISAGG : PRIORITY_COLS_AGG;
  const allCols = Object.keys(rows[0]);
  const cols = [
    ...priority.filter((k) => k in rows[0]),
    ...allCols.filter((k) => !priority.includes(k) && !HIDE_COLS.has(k)),
  ];
  const head = "<th>#</th>" + cols.map((k) => `<th>${escapeHtml(k)}</th>`).join("");
  const body = rows.map((r, i) => {
    const tds = cols.map((k) => {
      let v = r[k];
      if (v === null || v === undefined) return "<td>-</td>";
      const cls = ["tokens/s", "tokens/s/gpu", "tokens/s/user", "parallel",
                   "(p)parallel", "(d)parallel", "total_gpus_needed"].includes(k) ? "hl" : "";
      return `<td class="${cls}">${typeof v === "number" ? fmt(v) : escapeHtml(String(v))}</td>`;
    }).join("");
    return `<tr><td>${i + 1}</td>${tds}</tr>`;
  }).join("");
  return `<div class="table-wrap"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}

/* ---------------- utils ---------------- */
function fmt(v) {
  if (v === null || v === undefined) return "-";
  if (typeof v !== "number") return escapeHtml(String(v));
  if (!isFinite(v)) return "-";
  if (Number.isInteger(v)) return String(v);
  if (Math.abs(v) >= 1000) return v.toFixed(0);
  if (Math.abs(v) >= 100) return v.toFixed(1);
  return v.toFixed(2);
}
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (ch) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
}

/* ---------------- boot: meta + init ---------------- */
async function boot() {
  $("exp_yaml").value = EXP_YAML_EXAMPLE;
  try {
    const resp = await fetch("/api/health");
    const health = await resp.json();
    const badge = $("env-badge");
    if (health.python) {
      badge.className = "badge badge-ok";
      badge.textContent = "Python bridge: " + health.python.split("/").slice(-3, -1).join("/");
    } else {
      badge.className = "badge badge-bad";
      badge.textContent = "No Python with aiconfigurator found (set AIC_PYTHON)";
    }
  } catch (e) { /* server up but health failed */ }

  try {
    const resp = await fetch("/api/run", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: "meta", params: {} }),
    });
    const payload = await resp.json();
    if (payload.ok) {
      meta = payload.data;
      const sel = $("system");
      sel.innerHTML = meta.systems.map((s) => `<option value="${s}">${s}</option>`).join("");
      sel.value = meta.systems.includes("h200_sxm") ? "h200_sxm" : meta.systems[0];
      if (!meta.estimate_available) {
        $("est-hint").style.display = "";
        document.querySelector('.tab[data-mode="estimate"]').style.opacity = ".55";
      }
    }
  } catch (e) { /* ignore */ }
  switchMode("default");
}
boot();
