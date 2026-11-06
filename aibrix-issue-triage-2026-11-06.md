# AIBrix Issue Triage Report — #3410, #3411, #3405, #3407, #3393, #3392

- **Repository:** https://github.com/vllm-project/aibrix
- **Run date:** 2026-11-06 · **Code baseline:** `origin/main` @ `1a2edca`
- **Mode:** read-only. No comments, labels, PRs or code changes were made on GitHub or in the repo. Scratch worktrees and probe tests lived under the session scratchpad and were removed afterward (probe sources archived at `/tmp/claude-0/-home-user-aibrix/37276926-87d7-5b7a-85f8-8a268005877e/scratchpad/probe-archive/`).
- **Scope:** the six issues the requester named. #3392 and #3393 already carry `triage/accepted` and an assignee; they were still assessed, with emphasis on what the fix PRs must get right.

**Evidence legend.** **[V]** verified by me against `main` (code read and/or test run). **[A]** reported by a delegated read-only investigator and spot-checked by me where noted. **[R]** reported by the issue author and not independently verified.

**Limitations.**
- PR #3409 (the parent of #3410/#3411) and PR #3406 come from external forks. I read their diffs but did not execute their code; a run of fork test code was blocked by the sandbox classifier. All probes ran against `main`.
- No GPU, live vLLM/SGLang, multi-gateway Redis or real S3 was available. Claims that depend on them are marked unverified.
- The clone is shallow, so regression/blame questions are unanswered.

---

## A. Executive Summary

| Metric | Value |
|---|---|
| Issues reviewed | **6** |
| Category | Bug 4 (#3392, #3411, #3405, #3407) · Feature 1 (#3393) · Enhancement/observability 1 (#3410) |
| Severity | P0: 0 · P1: 0 · **P2: 4** (#3392, #3411, #3405, #3393) · P3: 2 (#3410, #3407) |
| Already `triage/accepted` | #3392, #3393 |
| Fix PR already open | #3392 → #3396 · #3405 → #3406 · #3410/#3411 are follow-ups to open PR #3409 |
| Duplicates / close candidates | none |

**Needs attention first:** #3392 — the fix (PR #3396) is written, has one maintainer approval and is waiting on a second reviewer.

**Recurring themes**

1. **The Redis "deadline that doesn't bind" family** (#3369/PR #3375 → #3408/PR #3409 → #3411 → #3410). go-redis `ContextTimeoutEnabled` is off on the shared client, so every `context.WithTimeout` on it is advisory and a stalled socket costs the 3 s default read timeout. Fixing it one call site at a time produces a fourth Redis pool and N follow-ups. #3411 should be resolved as one client-strategy decision, not eight patches.
2. **Hard-coded single-shot health probing in the runtime supervisor** (#3392, #3407): a 1 s `/health` probe feeds both "ready" and, with PR #3396, a 60 s kill rule backed by a lifetime restart budget. The two issues interact.
3. **Scheduler/lookup keys that don't match what the metrics store holds** (#3405).
4. **Process friction:** a fix PR (#3406) was opened before `triage/accepted`; #3405 carries spurious `area/gateway` and `area/website` labels; #3410/#3411 are waiting on a maintainer label before their author may implement.

**Issue quality:** high overall. #3410 and #3411 are candid about what is unmeasured (#3411's probe "has not been executed"; I ran it, see below). #3405's issue premise needs rescoping (see its section).

---

## B. Issue Triage Table (ordered by recommended engineering priority)

| Issue | Category | Severity | Priority | Confidence | Recommended Action |
|---|---|---|---|---|---|
| #3392 ModelClaim stuck Activating after EngineCore OOM | Bug / Reliability | P2 | **High** | High (logic) · Medium (real-engine behavior) | **Fix Now** — review and land #3396 |
| #3411 Redis deadlines on running-request writes/heartbeat | Bug / Reliability | P2 | Medium | High (reproduced) | **Accept and Prioritize** — decide client strategy first; after #3409 |
| #3405 leastLatency adapter scheduler fails on one unscored pod | Bug | P2 (opt-in policy) | Medium | High (defect) · Low-Medium (key-mismatch hypothesis) | **Accept and Prioritize** — rescope; hold PR #3406 for a live check |
| #3393 Respect priority when making room | Feature | P2 | Medium | High (code) · Medium (semantics) | **Accept and Prioritize** (already accepted) — after docs PR #3394 |
| #3410 Expose Redis fallback counts | Enhancement | P3 | Low-Medium | Medium-High | **Accept** counters after #3409; **Design Discussion** for the budget knob |
| #3407 SGLang never reads ready (1 s probe) | Bug (forward-looking) | P3 | Low | High (bug) · Low-Medium (fix shape) | **Accept and Prioritize** (low); design discussion on probe mechanism |

---

## C. Detailed Technical Assessment

### Issue: #3392 — ModelClaim remains Activating after EngineCore OOM during sleep

https://github.com/vllm-project/aibrix/issues/3392 · `googs1025` (COLLABORATOR) · labels `kind/bug`, `triage/accepted`, `area/runtime` · assignee `Zeyu-ZEYU` · fix PR #3396 (open)

**Summary:** After vLLM's EngineCore worker dies, the API server process stays alive with its port closed. The runtime keeps the model in `booting` forever, so the claim never leaves `Activating`.

**Technical Assessment**

- **Confirmed gap on `main` [V].** `python/aibrix/aibrix/runtime/model_runtime.py`: `_process_handle_alive` is only a `Popen.poll()` check; the supervisor path for a live process with a failed probe transitions to `booting` and `continue`s (~L1705-1719). The restart branch (`_schedule_restart(inst, "engine exited")`, ~L1728) runs only after the parent exits. `restart_count` is never reset (L1626-1632), so the restart budget is lifetime.
- **Why the controller never moves it [A].** The claim is marked Failed only when the runtime reports `failed`; `booting` is not a failure.
- **Fix design (PR #3396) [A, diff read].** Tracks `was_ready` and `unready_since`; after a continuous 60 s of failed probes following a ready state it calls `_schedule_restart`, which reuses the existing SIGTERM→SIGKILL stale-process-group stop. First boot is unchanged (no deadline). The due-restart branch is moved ahead of the liveness check so a stopping group is not clobbered back to `booting`. Four reliability tests plus a re-adoption parametrization; CI green; mergeable. Review: `googs1025` approved at `5b8ee02` (2026-11-04, "left for @Jeffwan"); Jeffwan has not reviewed.
- **Risks the review should weigh [A]:**
  - The 60 s is hard-coded and there is no corroboration signal; each false positive permanently consumes one of the 5 lifetime restarts, and a sixth leaves the claim `failed`.
  - The kill is immediate (SIGKILL branch not exercised by the tests).
  - Probe quality: with a 1 s probe (#3407), a busy-but-healthy engine could be misread. vLLM 0.30.0's `/health` was verified from its wheel to return 200 unless the engine is errored and not to touch EngineCore, so it should only miss the probe if the API server's event loop blocks >1 s. That under extreme load is plausible but unverified.
  - A follow-up for an EngineCore that dies while the engine is `sleeping` is acknowledged by the author as open.
- **Interaction with #3407 [A, confirmed in the PR hunk].** `was_ready` is only set on a passing probe, so an SGLang engine that never passes the 1 s probe is never subject to #3396. Once #3407 is fixed with a longer probe, #3396 starts applying to SGLang, where `/health` can legitimately run up to 20 s — a false-restart risk nobody has analyzed.

**Production Impact:** one model per runtime pod becomes unavailable with no automatic recovery and no explicit failure signal; co-located models are unaffected (author's A10 test). Manual claim deletion recovers it. Hence P2, high priority because the fix is nearly done.

**Recommended Action:** Fix Now — get a second review on #3396 and merge. Labels: `kind/bug`, `area/runtime`, add `area/modelclaim`, `priority/important-soon`. Effort: Small (remaining). Confidence: High on logic, Medium on real-engine behavior.

**Next Steps:** (1) Jeffwan review of #3396; (2) consider making the 60 s a documented constant/knob and logging the kill reason clearly; (3) track the sleeping-engine follow-up as its own issue; (4) cross-link #3407 so the probe change and the kill rule are reviewed together.

---

### Issue: #3411 — Honor Redis deadlines on running-request writes and heartbeats

https://github.com/vllm-project/aibrix/issues/3411 · `Jackmeson1` (CONTRIBUTOR) · labels `kind/bug`, `area/gateway` · no `triage/accepted` · follow-up B.3 to the maintainer review of PR #3409

**Summary:** Eight call sites in `pkg/cache` put a 1 s `runningRequestsWriteTimeout` context on the shared Redis client, whose `ContextTimeoutEnabled` is off, so a silent Redis costs ~3 s per call instead of 1 s. The liveness heartbeat is synchronous on its ticker, so one stalled call consumes the whole 3 s liveness window.

**Technical Assessment**

- **Site list [V].** All eight cited locations match exactly on `main` and at the PR head: `cache_running_requests.go:195, 215, 450, 491, 622, 684, 862` and `cache_decode_ledger.go:234`. PR #3409 does not touch them (its non-test changes are in `cache_init.go`, `utils/redis.go` and 2–7 lines elsewhere). The cited follow-up commit `6beec8fa` does not exist upstream or in PR #3409; the PR head is `cb6429cf`.
- **Reproduced on `main` (the issue says its probe "has not been executed") [V].** I wrote my own probes against a store built on a default-options client (what `utils.GetRedisClient` returns):

  | Scenario | Result |
  |---|---|
  | Heartbeat, listener silent from first byte (init stall) | **3.002 s** (budget 1 s) |
  | Heartbeat, warmed connection that goes silent | **3.001 s** |
  | `incrPodRunningRequests` / `decrPodRunningRequests`, warmed stall | **3.002 s** each |
  | Control: same heartbeat with `ContextTimeoutEnabled: true` | **1.000 s** |
  | `refreshInFlightPodHashTTLs` | 0 s — it only issues commands for pods with in-flight requests and my test had none, so **not exercised** |

  So the issue's expectation holds for both the init stall and the warmed-stall case it says a fix must cover.
- **Liveness mechanism [V by code, end-to-end not reproduced].** `time.Ticker` drops ticks while the handler runs, so a 3 s stall yields one heartbeat attempt per 3 s window instead of three, removing the "tolerates a missed beat" margin documented at `cache_running_requests.go:58-66`. The ZADD score is computed (`c.redisNowMillis()`, L454) before the command is sent, so a command that lands late carries an already-stale score. The multi-gateway exclusion effect itself remains unreproduced, as the issue states.
- **Request-path impact is bounded [V].** Increments/decrements run from detached goroutines (`cache_trace.go:264, 342`; `cache_impl.go:226`), so a stall holds goroutines and pooled connections for 3 s rather than adding request latency. The first heartbeat also runs inline in `initRunningRequestsLiveness` (L723) before the ticker goroutine starts; that function is called at `cache_init.go:485`. A silent Redis at that moment blocks it for ~3 s. I did not trace whether this is on the gateway's startup critical path.
- **Design considerations the issue does not settle:**
  1. **Same budget-covers-handshake trade-off as #3409's B.1.** Enabling deadlines on the heartbeat/write client makes a cold connection share the 1 s budget with TCP connect, HELLO and CLIENT SETINFO. It matters less for a 1 Hz heartbeat on a warm connection, but a design that gives writes a handshake-aware budget is needed.
  2. **Client strategy.** Honoring deadlines via a third derived client adds a third pool per gateway pod. The alternative suggested in the #3409 review — turn `redisClient` itself into the deadline-enabled copy with default retries and keep `admitRedisClient` as the no-retry variant — gives two derived clients but changes write-path behavior and deserves its own PR. Flipping the flag in `GetRedisClient` was already rejected on #3408 for blast radius.
  3. **Retry semantics are a weaker constraint than the issue implies [V, proxy experiment].** The increment/decrement Lua script is non-idempotent (`HINCRBY`) and runs on the retrying shared client. With a proxy that executes the command, drops the reply and resets the connection, one logical +1 moved the field 1→3 on the shared client (retried, reported OK) and 1→2 on a no-retry client (reported failure). After pairing with the completion decrement, both leave a net +1, which the drift reconciler corrects. So "preserve retry counts" need not constrain the design.
- **Existing coverage:** the PR's silent-TCP tests cover admission and the two routing reads only. Nothing covers the eight sibling sites.

**Production Impact:** degraded-mode quality during a Redis stall or half-open connection (goroutine/connection hold 3× longer; weaker liveness margin). Not a correctness bug and not on the request path, so P2, medium priority.

**Recommended Action:** Accept and Prioritize, sequenced after #3409 lands, and resolve the client strategy in the issue before the PR. Labels: `kind/bug`, `area/gateway`, `triage/accepted`, `priority/important-longterm`. Effort: Medium (design decision plus a regression test per site). Confidence: High.

**Next Steps:** (1) a maintainer applies `triage/accepted` (the author is waiting on it); (2) agree the strategy (derived write client vs. deadline-enabled `redisClient`); (3) include the warmed-stall probe and one test per site; (4) correct the commit reference.

---

### Issue: #3410 — Expose Redis admission and routing fallback counts

https://github.com/vllm-project/aibrix/issues/3410 · `Jackmeson1` (CONTRIBUTOR) · labels `area/gateway`, `kind/feature` · no `triage/accepted` · follow-up B.2 to the maintainer review of PR #3409

**Summary:** When Redis is slow or down, admission, running-request reads and decode-ledger reads silently fall back to local counts. Add bounded-label counters, and consider an optional override of the 100 ms budget.

**Technical Assessment**

- **Premise holds [V].** Fallbacks exist and log only at `klog.V(4)` (`cache_running_requests.go:350, 361, 367, 929, 934`; `readPodsSharedRoutingState` batch-read failure). No counter exists in `pkg/cache` or `pkg/metrics` for them. PR #3040 ("track local fallbacks") concerns the `counted` accounting flag, not metrics.
- **Fits an existing convention [V].** Counters are defined in `pkg/metrics/gateway_metrics.go` and emitted with `metrics.EmitMetricToPrometheus(routingCtx, pod, name, &SimpleMetricValue{Value: 1}, extraLabels)`. A nil context and pod are supported (`generateDefaultMetricLabelsMap` handles `pod == nil`; precedent `engine_fetcher.go:583`), and `gateway_rsp_body.go:394` shows a counter with a bounded `reason` label.
- **Design points the issue leaves open:**
  1. **A fallbacks-only counter cannot give a rate.** Operators need fallbacks ÷ attempts. Prefer a single `…_total{operation,result}` with `result` ∈ {ok, timeout, error}, or add an attempts counter.
  2. **Count per pipeline `Exec`,** not per logical read: `readPodsSharedRoutingState` serves both the running-request read (`withLedger=false`) and the ledger read (`true`) in one pipeline (`cache_decode_ledger.go:285, 332`). The label should say which.
  3. **Do not count the expected cases:** Redis unconfigured (`redisClient == nil`) and missing/empty hash (`len(fields) == 0`) are not Redis failures.
  4. **Sequencing.** On `main` a stalled read falls back after ~3 s, not 100 ms. Timeout-classified counters only mean what the issue says after #3409.
- **The budget override is a separate decision.** There is no precedent for a timeout env knob in `pkg/utils` or `pkg/cache` (only `REDIS_HOST/PORT/PASSWORD`). #3409's own text states the 100 ms budget assumes Redis RTT well below ~30 ms, including the connect handshake. An override widens the latency every request can absorb, so it needs a name, range validation and a documented trade-off. Counters should land first and show whether anyone needs it.
- **Not run [A/R]:** the author's silent-TCP tests and delay-proxy measurements live on the #3409 branch (external code); I did not execute them.

**Production Impact:** none today (observability). Useful to anyone running a remote or flaky Redis.

**Recommended Action:** Accept the counters (after #3409); split the budget override into its own issue as Design Discussion Required, or defer. Labels: `kind/enhancement` (rather than `kind/feature`), `area/gateway`, `area/observation`, `triage/accepted` for the counter half. Effort: Small–Medium. Confidence: Medium-High.

**Next Steps:** agree the metric shape (operation/result labels, per-Exec counting); implement after #3409; open a separate issue for the knob.

---

### Issue: #3405 — leastLatency adapter scheduler fails the whole selection when one ready pod has no latency metrics

https://github.com/vllm-project/aibrix/issues/3405 · `PerryLink` (CONTRIBUTOR) · labels `kind/bug`, `area/gateway`, `area/website`, `area/orchestration` · no `triage/accepted` · fix PR #3406 (open, `DsThakurRawat`)

**Summary:** `leastLatencyScheduler.SelectPod` returns on the first metric-cache miss, so one Ready pod without latency metrics blocks scheduling of the whole ModelAdapter. The sibling `leastThroughput` was fixed to skip such pods (#3299).

**Technical Assessment**

- **Defect confirmed [V].** `pkg/controller/modeladapter/scheduling/least_latency.go:44-51` returns `nil, err` on the first miss; `least_throughput.go:44-69` shows the fixed pattern (last-error, `continue`, `math.MaxFloat64` sentinel, `no ready pod has … metrics: %w`).
- **Mapping facts confirmed [A].** `request_queue_time_seconds` maps to vLLM and TRT-LLM; `request_inference_time_seconds` to vLLM only (`pkg/metrics/metrics.go:252-270`). Unmapped series are never stored (`engine_fetcher.go:266-270`). On SGLang or TRT-LLM `leastLatency` can never score any pod, and this stays true after PR #3406. The issue's doc analogy is loose: `multi-engine.rst:48-50` describes gateway routing policies, which fall back to a random pod; it is not the adapter scheduler (`:58-59` says least-latency should be used with vLLM only).
- **Failure mode [A].** `schedulePods` → `reconcileLoadOnSinglePod` returns the error; controller-runtime retries with backoff, but no status condition or event is written, so the stall is invisible on the CR.
- **Likely larger problem: the lookup key [V by code, live behavior unverified].** The controller calls `r.scheduler.SelectPod(ctx, instance.Name, …)` (`modeladapter_controller.go:767`), i.e. the **ModelAdapter's own name**. `GetMetricValueByPodModel` builds the key `<modelName>/<metric>` (`cache_impl.go:188-195`, `cache_metrics.go:331-334`), while stored keys come from the scraped Prometheus `model_name` label (`cache_metrics.go:405-416`, with a pod-metadata fallback). If the engine labels request histograms with the base served model name — which I believe vLLM does but could not verify here — then `leastLatency` finds no metrics for any adapter and the policy never schedules. The issue's own unit repro uses the same string for label and lookup, which would hide this. An investigator's unit-level test with pods scored under `base-model` and `SelectPod("my-adapter", …)` returned `metric not found` [A].
- **PR #3406 [A, diff read; PR code not executed].** Commit 1 implements the skip pattern; commit 2 changes the controller to resolve a metric model (`spec.baseModel`, else the pod's `model.aibrix.ai/name`, else the adapter name). CI is green. Reviews: `googs1025` asked for a real (non-unit) test; `varungup90` flagged the lookup key; the PR body ticks "Thorough testing completed" while the author says no live cluster was available. It was opened ~5.8 h after the issue, before any `triage/accepted` (AGENTS.md contribution gate). Pre-existing and unchanged: a histogram with `Count==0` scores 0 and outranks measured pods (`types.go:174-179`).

**Production Impact:** only clusters running `--model-adapter-scheduler-policy=leastLatency` with `replicas` set. The default is `leastAdapters` (`modeladapter_controller.go:121`), so default users are unaffected. Workarounds: use `leastAdapters`/`random`/`binPack`, or leave `replicas` unset.

**Recommended Action:** Accept and Prioritize, **rescoped** to cover both the skip behavior and the adapter-name-versus-base-model key. Hold #3406 until someone checks the `model_name` label on a live vLLM. Labels: `kind/bug`, `area/orchestration`, `triage/accepted`; **remove** `area/gateway`, `area/website`. Effort: Small. Confidence: High on the defect, Low-Medium on the key hypothesis.

**Next Steps:** (1) live vLLM check of the series' `model_name` label for a LoRA-served request; (2) emit an event or condition when scheduling fails; (3) decide whether SGLang/TRT-LLM should be rejected up front for this policy.

---

### Issue: #3393 — [Feature][ModelClaim] Respect priority when making room for a new claim

https://github.com/vllm-project/aibrix/issues/3393 · `Zeyu-ZEYU` (CONTRIBUTOR) · labels `triage/accepted`, `kind/feature`, `area/orchestration` · self-assigned · no implementation PR yet · docs PR #3394 open

**Summary:** Since #3273 a *waking* model only sleeps idle models of the same or lower priority, but a *new* claim with the default `preemptionPolicy: Never` makes room with no priority check.

**Technical Assessment**

- **Confirmed [V on the two key points].** `pkg/controller/modelclaim/makeroom.go` `planRoom` builds its victim list from `r.idleEngines(...)` with no priority filter (~L444), and its feasibility and the slept victim both come from that list. The wake path uses `idleEnginesAtOrBelowPriority` (`wake.go:536-544`, `Priority <= waker`). `SleepLowerPriority` has its own strictly-lower rule in `preemption.go` [A].
- **Feature, not regression [A].** PR #3361 documents the current behavior "as it is"; the guide says normal placement "may … sleep eligible idle engines" and "does not compare priorities"; #3171 deliberately preserved `Never` behavior. A collaborator calls it a bug (priority inversion), but it changes default-policy semantics for users who set priorities, so keep `kind/feature` and note it in release notes.
- **Semantics are sound; the matrix in the thread matches the code [A].** `<=` with everything at the default priority 0 changes nothing.
- **What the implementation PR must get right [A]:**
  - Filter **inside `planRoom`**, which is shared by `makeRoomToPlace`, `firstToMakeRoomFor` and `claimFeasibleForPlacement` (`priority.go`). Filtering only in `makeRoomToPlace` would leave a "feasible" predecessor holding the placement turn that cannot act. This is the case Zeyu raised in the thread.
  - Flip `makeroom_test.go:529` (a priority-0 `Never` claim currently sleeps priority-20 residents) and add the matrix, head-of-line and default-priority tests.
  - Update the guide's "Placement priority", "Soft preemption" and "Sleep and wake" sections and the zh_CN `.po`. #3394 rewrites exactly the paragraph this fix makes false, which is the sequencing dependency the assignee mentioned.
- **Residual risks [A]:** no aging or timeout, so starvation is bounded only if the pool sets `sleepAfterSeconds > 0`; the waiting state surfaces as the generic `NoMatchingPods` text with no hint that higher-priority idle models are the cause (a short message suffix would help); victims stay ordered longest-idle first, matching wake.

**Production Impact:** operators of pools with mixed priorities and default `Never` claims: an idle high-priority model can be slept for a low-priority newcomer and its next request waits for a wake. No in-flight request is interrupted. P2.

**Recommended Action:** Accept and Prioritize (already accepted). Merge #3394 first, then review the implementation PR against the points above. Labels: keep `triage/accepted`, `kind/feature`; add `area/modelclaim`. Effort: Small–Medium. Confidence: High on code facts, Medium on product semantics.

---

### Issue: #3407 — An SGLang engine never reads ready, because its /health takes longer than the 1 s probe

https://github.com/vllm-project/aibrix/issues/3407 · `chrikrah` (author association NONE) · labels `kind/bug`, `area/runtime`, `area/orchestration` · no `triage/accepted` · three comments including maintainer discussion

**Summary:** `engine_ready` probes `/health` with a hard-coded 1.0 s timeout; SGLang's generation-based `/health` always sleeps ~1 s first, so a healthy SGLang engine reads not-ready and stays `booting`.

**Technical Assessment**

- **Confirmed [V].** `model_runtime.py:567-576`: `engine_ready(port)` uses `timeout=1.0` and takes no engine argument. Callers reach it via `instance_ready`, used by `supervise_once`, the snapshot probe, `list_runtime_models` and re-adoption [A]; the controller routes only when `observed.Ready` [A].
- **Reproduced [V by the investigator, race script run against `main`'s `engine_ready`]:** `/health` delay ≤0.99 s → 20/20 ready; 1.0 s → 6/20 (a coin flip at the boundary; the issue saw 1/20); ≥1.005 s → 0/20.
- **SGLang claim verified from the 0.5.23 wheel (read as text only) [A]:** `http_server.py:721-785` loops with `await asyncio.sleep(1)` before its first check (:780); `SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION` defaults to true; with it off `/health` returns 200 immediately; its own failure timeout is 20 s; a cheaper `/ready` route exists (semantics not evaluated).
- **Impact is forward-looking.** The maintainer says basic SGLang serving "isn't expected to work today", the published runtime image lacks SGLang, `model-claim.rst:91-94` calls `engine: sglang` untested, and sleep/wake raises `UnsupportedModelControlError` for non-vLLM engines. This is one of several blockers.
- **Fix-shape risks [A]:**
  - A per-engine timeout means threading the engine through `instance_ready`; tests, including PR #3396's, monkeypatch `engine_ready(port)` with `lambda port:`, so a signature change must be coordinated with #3396.
  - A longer probe for SGLang pulls it under #3396's 60 s kill rule while SGLang's `/health` can legitimately take up to 20 s.
  - SGLang's `/health` injects a one-token generation on every probe — a cost to weigh against probe frequency.
  - Untested workaround: setting `SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION=0` on the runtime container (engines inherit the environment).

**Recommended Action:** Accept and Prioritize at low priority, with the probe mechanism (per-engine timeout vs. env override vs. `/ready`) settled in the thread first. The reporter is a non-maintainer and must wait for `triage/accepted` before a PR. Labels: `kind/bug`, `area/runtime`, `area/modelclaim` (drop `area/orchestration`), `triage/accepted` once decided, `priority/important-longterm`. Effort: Small. Confidence: High that the bug is real; Low-Medium on the best fix.

---

## D. Top 5 Issues Worth Prioritizing

1. **#3392** — a permanently stuck model with no automatic recovery, and the fix is written. The cost is one more review. Land #3396 and carry the two risks (hard-coded 60 s, lifetime restart budget) into follow-ups.
2. **#3411** — the only issue here that affects default-configuration production gateways with Redis. Spend the effort on the client-strategy decision once; otherwise the same deadline bug recurs at every new call site.
3. **#3405** — small to fix, but verifying the adapter-name key matters more than the skip logic: it may mean the policy has never worked for adapters.
4. **#3393** — a clean, accepted change; the value is in reviewing it against the `planRoom` placement and docs sequencing so it does not ship half-consistent.
5. **#3410** — cheap observability that makes the whole Redis fallback family measurable; ask for the attempts/result shape before the PR.

(#3407 ranks sixth: real but gated behind several other SGLang blockers.)

---

## E. Issues Recommended for Closure

None. #3405 should not be closed when #3406 merges unless the key question is resolved; #3410 should be split rather than closed.

---

## F. Proposed Maintainer Responses (drafts — not posted)

**#3411**

> Reproduced against `main` (1a2edca), since the repo's silent-listener helper only exists on the #3409 branch: on a default-options client the heartbeat takes 3.00 s for both a stall from the first byte and a warmed connection that goes silent, and increment/decrement take 3.00 s on a warmed stall, against a 1 s budget. The same heartbeat on a client with `ContextTimeoutEnabled` returns at 1.00 s, so the mechanism is as you describe. All eight sites match on `main` and at #3409's head. The follow-up commit you cite (`6beec8fa`) doesn't exist; #3409's head is `cb6429cf`.
>
> Before a PR, please settle the client strategy here: (a) a third derived write client, or (b) making `redisClient` the deadline-enabled copy with default retries and keeping `admitRedisClient` as the no-retry variant, which means two derived clients instead of three. Note the heartbeat has the same budget-covers-the-handshake trade-off as B.1 on #3409, and the ticker drops ticks while the handler runs, so a 3 s stall leaves one attempt per liveness window. On retries: I tried a reset after the script executed and the field ends at net +1 once the completion decrement lands, with or without go-redis retries, and the drift reconciler repairs it, so "preserve retry counts" doesn't need to constrain the design.
>
> This should follow #3409. Marking it for `triage/accepted` once you've picked the approach.

**#3410**

> This fits the metrics convention: define the counter in `pkg/metrics/gateway_metrics.go` and emit it with `EmitMetricToPrometheus(nil, nil, …)`, as `engine_fetcher.go` and the TPM-failure counter do. Suggestions: (1) a fallbacks-only counter can't give a rate, so prefer one `…_total{operation,result}` with `result` ∈ {ok, timeout, error}, or add an attempts counter; (2) count per pipeline `Exec`, because `readPodsSharedRoutingState` serves both the running-request and ledger reads in one pipeline, and label which; (3) don't count Redis-unconfigured or a missing pod hash as failures; (4) wait for #3409, because on `main` a stalled read falls back after ~3 s, not 100 ms.
>
> I'd split the budget override into its own issue: there is no timeout env-knob precedent in `pkg/utils` or `pkg/cache`, and #3409 documents that 100 ms assumes Redis RTT well under ~30 ms including the handshake. Let the counters show whether anyone needs it. Counters are fine to accept once #3409 is in.

**#3405**

> Confirmed that `leastLatency` returns on the first cache miss (`least_latency.go:44-51`), unlike the fixed `leastThroughput`. Two corrections: `multi-engine.rst:48-50` describes gateway routing, not this scheduler, and on SGLang or TRT-LLM every pod is unscorable, so skipping pods changes only the error text there.
>
> A bigger question: the controller passes `instance.Name` (the ModelAdapter's name) to `SelectPod` (`modeladapter_controller.go:767`), but model-scoped metrics are stored under `<model_name label>/<metric>` (`cache_metrics.go:331-334, 405-416`). If vLLM labels these histograms with the base model name, the lookup misses for every adapter and the policy never schedules, so the premise that pod A "has both histograms" needs a live check. Could someone confirm the `model_name` label on a LoRA-served request? Please rescope the issue to cover both points. #3406's second commit touches the key, so I'd hold it for that check. A failed selection also writes no event or condition, which would be worth adding.

**#3407**

> Confirmed. `engine_ready` is a hard-coded 1.0 s probe (`model_runtime.py:572`) and SGLang 0.5.23's default generation-based `/health` sleeps 1 s before its first check, so a healthy engine reads not-ready. Against `main`, a `/health` delay of 1.0 s passes only about 30% of the time and 1.005 s or more never passes.
>
> Two cautions for the fix: a per-engine timeout means passing the engine through `instance_ready`, and tests (including #3396's) monkeypatch `engine_ready(port)`, so coordinate the signature. And once SGLang reads ready, #3396's 60 s unready-after-ready kill rule applies to it, while its `/health` can take up to 20 s by its own timeout, so please check false restarts. Options: a longer SGLang timeout, `SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION=0` (untested), or its `/ready` route. Since basic SGLang serving is blocked elsewhere too, this is low priority. Please wait for `triage/accepted` before sending a PR.

**#3393 (optional, already accepted)**

> When you open the PR: apply the same-or-lower filter inside `planRoom` rather than only in `makeRoomToPlace`, because `planRoom` also feeds `firstToMakeRoomFor` and `claimFeasibleForPlacement`; otherwise a "feasible" predecessor can hold the placement turn without being able to act. Flip the existing test that has a priority-0 `Never` claim sleeping priority-20 residents (`makeroom_test.go`), cover the matrix, head-of-line and default-priority cases, and update the guide and `.po` after #3394. The wait currently surfaces only as the generic `NoMatchingPods` text; a short suffix naming higher-priority idle models would help operators, and starvation is bounded only when the pool sets `sleepAfterSeconds > 0`.

**#3392 (optional, already accepted)**

> Reviewed #3396 against the supervisor on `main`: a live process with a failed probe goes to `booting` and only restarts after exit, so the gap is real and the design matches the accepted direction. Points for the second review: the 60 s is hard-coded and each false positive permanently spends one of 5 lifetime restarts; the SIGKILL branch is not exercised by the tests; the EngineCore-dies-while-sleeping case remains open; and #3407's longer SGLang probe would bring SGLang under this rule, where `/health` can take up to 20 s.

---

## Process observations for maintainers

- **Contribution gate.** PR #3406 was prepared before `triage/accepted` on #3405 (same pattern as #3374 last week). #3410/#3411 are correctly waiting for a label before implementation.
- **Label hygiene.** #3405 has `area/gateway` and `area/website` for a controller-side scheduler; #3407 and #3393 would be better tagged `area/modelclaim` (the label exists) than `area/orchestration`.
- **Shared Redis client policy.** Four related issues/PRs now touch the same root cause. A single written decision on shared-client deadline handling would stop further piecemeal follow-ups.

---

## Maintainer Decision Brief

- **Immediate attention:** **#3392** — second review and merge of #3396.
- **Highest-value work:** #3392, then **#3411** (settle the Redis client strategy, then implement after #3409), then **#3405** (verify the adapter-name key first), then #3393 and #3410.
- **Needs design review:** **#3411** (third pool vs. deadline-enabled `redisClient`; handshake budget for writes), the **#3410 budget override** (separate issue), and the **#3407 probe mechanism** together with #3396's kill rule.
- **Needs clarification:** #3405 — a live-vLLM confirmation of the `model_name` label for LoRA requests; #3411 — the cited commit hash and the preferred client approach.
- **Close or defer:** nothing to close. Defer #3410's budget knob and #3407 (behind other SGLang blockers). Hold PR #3406 until the key question is answered.
