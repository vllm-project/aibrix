# AIBrix Issue Triage Report

- **Repository:** https://github.com/vllm-project/aibrix
- **Run date:** 2026-11-01 (window: 2026-10-31 19:11 UTC → 2026-11-01 19:11 UTC)
- **Code baseline:** `origin/main` @ `519a240` (local HEAD == `origin/main` at run time)
- **Mode:** read-only. No comments, labels, PRs or code changes were made. The only file written into the repo tree is this report.

## Scope and method

Seven issues (excluding PRs) were created in the window. Three already carry `triage/accepted` and were treated as already triaged. **Four are untriaged and in scope: #3377, #3376, #3370, #3368.**

| Excluded (already `triage/accepted`) | Note |
|---|---|
| #3372 Empty body for multi-chunk non-2xx upstream response | triaged |
| #3371 PD prefill leg never receives `x-aibrix-mock-delay-ms` | triaged |
| #3369 Session-key Redis lookup waits 3 s despite 500 ms budget | triaged |
| #3367 Refresh openai_frontend schemas | created 9 min before window start; also already accepted |

For each in-scope issue I read the full body and comments, traced the code, searched issues/PRs (open and closed) for duplicates and overlapping work, and independently reproduced the two reproducible bugs:

- **#3376** — reproduced on `main` with a Go test supplied through `go test -overlay`, so no file was added to the repo. Control case passes; the `drain.timeoutSeconds: 30` case fails (pod deleted on the first finalize pass, no `aibrix.ai/draining` annotation).
- **#3377** — reproduced with the repo's own `NativeMultipartTestStorage` double plus a paginating `list_objects`, run from a scratchpad test file. Leaked staged objects: 900 parts → **0**, 1,205 → **206**, 5,000 → **4,001**. The assembled output was correct in every case.
- **#3368** — reproduced the failing assertion, and measured the unpatched `_build_response`: 3,000 requests → 2,998 distinct ids, 50,000 → 48,828.

**Limitations (what I did not or could not verify):**

- The clone is shallow (50 commits), so I could not run `git blame`/`log` to decide whether any bug is a regression. Where I mention origin, it is a hypothesis.
- #3377: no end-to-end run against real S3/TOS or moto. The Files-API-blindness consequence is derived from code and S3's documented lexicographic key order, not executed.
- #3376: envtest/e2e suites not run.
- #3370: `docs.vllm.ai` is not reachable from this session (it would have to be added to the session's allowed network domains), so statements about what vLLM already supports are **unverified** and flagged as such.

---

## A. Executive Summary

| Metric | Value |
|---|---|
| New untriaged issues reviewed | **4** (3 already-triaged in window excluded) |
| By category | Bug 3 (one is a gap in a merged feature), Feature Request 1 |
| By subtype | Reliability 2 (#3377, #3376), Documentation/contract 1 (#3368), Feature/RFC-scale 1 (#3370) |
| Severity | P0: 0 · **P1: 1** · P2: 3 · P3: 0 |
| Duplicates / already fixed | 0 (but #3368 already has an open fix PR, #3374) |

**Needs attention now:** **#3377.** Every S3/TOS batch job with roughly 1,000 or more results permanently leaks `.multipart/` staging objects, and nothing garbage-collects them. Because those keys sort first in an S3 listing, enough leakage makes `GET /v1/files` return an empty page (derived from code, not executed end to end). The fix is small.

**Recurring themes:**

1. **Pagination assumed away.** The storage test double never paginates, so a one-page assumption survived (#3377).
2. **Opt-in feature paths not covering every deletion path** (#3376: drain exists on the PodSet and RoleSet-syncer paths but not on RoleSet teardown).
3. **Process friction:** a fix PR (#3374) was opened before `triage/accepted`, contrary to the AGENTS.md contribution gate. Separately, #3376 carries seven labels, six of them spurious.

**Issue quality:** high for #3377, #3376, #3368. Each has a runnable test, a measured result, an environment and a precise code pointer, and AI assistance is disclosed. #3370 is a design pitch without the design: unfinished table, "Area: Not sure", no security model, and no mention of the in-tree `vipe_engine`.

---

## B. Issue Triage Table (ordered by recommended engineering priority)

| Issue | Category | Severity | Priority | Confidence | Recommended Action |
|---|---|---|---|---|---|
| #3377 | Bug / Reliability | **P1** | High | High (defect) · Medium (Files-API impact) | **Fix Now** |
| #3376 | Bug / Reliability (gap in merged drain feature) | P2 | Medium-High | High (defect) · Medium (impact size) | **Accept and Prioritize** |
| #3368 | Bug (public response contract) | P2 (low end) | Medium-Low (quick win) | High | **Accept and Prioritize** (review PR #3374) |
| #3370 | Feature Request (RFC-scale) | P2 (unvalidated need) | Low-Medium until design | Medium | **Design Discussion Required** |

---

## C. Detailed Technical Assessment

### Issue: #3377 — Small-parts upload cleanup leaves staged objects after the first listing page

https://github.com/vllm-project/aibrix/issues/3377 · by `Jackmeson1` (CONTRIBUTOR) · created 2026-11-01 · labels `kind/bug`, `area/batch` · no assignee · no linked PR

**Summary:** After a small-parts multipart upload completes, cleanup lists the `.multipart/<upload_id>/` prefix once, discards the continuation token and deletes only that page. On S3/TOS a page is at most 1,000 keys, so any upload with more than about 999 parts leaves the rest behind.

**Technical Assessment**

- **Root cause (confirmed).** `python/aibrix/aibrix/storage/base2.py:251`: `objects_to_delete, _ = await self.list_objects(prefix)` ignores the token. The legacy `BaseStorage.abort_multipart_upload` has the same pattern at `storage/base.py:625`, and the issue does not mention it.
- **Backends.**
  - `s3.py:230-278` issues one `list_objects_v2` call with no `MaxKeys` unless a limit is passed, so it returns ≤1,000 keys plus `NextContinuationToken`.
  - `tos.py:212-264` behaves the same way.
  - `local.py:301-389` and Redis return everything when `limit` is `None`. This agrees with the report that those backends are unaffected.
- **Why batch hits it.** The batch adapter stages one object per request: `batch/storage/adapter.py:300` calls `upload_part(file_id, upload_id, request_index, …)`. `complete_multipart_upload` is invoked at `adapter.py:484` and `:562`, and cleanup runs at `base2.py:147/195/222/239`. Any S3/TOS batch with at least ~1,000 completed results triggers the leak. Error files do too, if they have that many records.
- **Why tests missed it.** `tests/storage/test_base2.py:132-144` returns `None` as the token and applies `limit` by truncation. The 5,000-part benchmark in `test_small_parts_benchmark.py` runs on the same non-paginating double.
- **Reproduced:** 900 parts → 0 leaked; 1,205 → 206; 5,000 → 4,001. Output bytes were correct in all runs, so this is a leak, not corruption.
- **Second-order impact (derived, not executed).** `metadata/api/v1/files.py:44,278-289` lists with `prefix=""` and a fixed ceiling of 1,000, then skips keys starting with `"."`. On S3/TOS, `.multipart/…` sorts before alphanumeric keys. Once at least 1,000 staged keys have leaked, the first 1,000 keys returned are all skipped and `GET /v1/files` shows nothing. The reporter's "consume the Files API's listing window" is correct and understated.
- **No garbage collection.** A search of `python/aibrix` for `.multipart` and orphan sweepers found none. Already-leaked objects stay forever, as the reporter notes.
- **Origin (hypothesis):** most likely arrived with the small-parts staging design (closed PR #2379, "Storage small file multi-upload optimization…"). Not verifiable here (shallow clone).
- **Fix caveats the reporter did not raise.**
  - Local and Redis tokens are offset-based. Deleting between pages while following an offset token skips entries. The safe pattern is to collect all keys first and then delete, or to re-list from the start until empty, with a progress guard so a silently failing delete cannot loop forever.
  - Fix both `base2.py` and `base.py`.
  - Add a paginating test double (the reporter's `paged_list` can be promoted into the shared helper).

**Production Impact**

- **Affected:** deployments using S3 or TOS storage for batch. This is the production configuration, since local storage is a dev default. Jobs of at least ~1,000 requests are the normal batch case.
- **Likelihood:** deterministic, every such job.
- **At scale:** unbounded storage growth (4,001 objects per 5,000-request job) and, after about one large job, degraded Files listing. No request-path outage and no data corruption, which is why this is P1 rather than P0.

**Recommended Action**

- **Fix Now.** Apply `triage/accepted` first: the reporter is a non-maintainer, so AGENTS.md requires acceptance before they implement.
- **Labels:** keep `kind/bug`, `area/batch`; add `triage/accepted`, `priority/important-soon`.
- **Estimated effort:** Small for the fix and test. A follow-up sweeper for pre-existing orphans is Medium and should be a separate issue.
- **Confidence:** High on the defect, Medium on the Files-API consequence.

**Next Steps**

1. Page through all listing results in both `abort_multipart_upload` implementations, using one of the safe patterns above.
2. Add a regression test with a paginating list (≥2,001 parts to cover multiple pages).
3. Open a follow-up for (a) an age-based sweeper keyed off the metadata object's `created_at`, and (b) making the Files API skip internal prefixes at listing time instead of consuming its 1,000-key window.
4. Ask the reporter whether TOS reproduces; only S3/moto was run.

---

### Issue: #3376 — RoleSet finalizer skips role drain, so replica-mode StormService rollouts and scale-in delete pods without draining

https://github.com/vllm-project/aibrix/issues/3376 · by `wguo7` (CONTRIBUTOR) · created 2026-11-01 · labels: `kind/bug` plus six `area/*` labels · no assignee · no linked PR

**Summary:** `RoleSetReconciler.finalize` deletes pods directly. A StormService in replica mode rolls out and scales in by deleting whole RoleSets, so `roles[].drain.timeoutSeconds` is never honored for single-pod (`podGroupSize: 1`) roles. Pods are deleted without the `aibrix.ai/draining` mark and without the wait.

**Technical Assessment**

- **Defect (confirmed, and reproduced independently on `main` @ `519a240`).**
  - `pkg/controller/roleset/sync.go:274-289` calls `r.Delete` on each pod and never calls `controllerdrain.DeletePods`.
  - Contrast `pkg/controller/podset/podset_controller.go:554-575` (`finalizePodSet`), which does call it with `podSet.Spec.Drain`. The RoleSet's own scale-in and rollout paths do too (`rolesyncer.go:128, 547, 580-592`).
  - My overlay test: `no drain configured (control)` PASS; `drain.timeoutSeconds 30` FAIL. The pod was gone and the annotation empty after the first pass.
- **Why replica-mode StormService hits it.** `stormservice/sync.go` deletes RoleSets on scale-in (`:216, :230, :247`), in `rollingUpdate` (`:309`) and in `finalize` (`:437`). All go through `rolesetoperations.go:148-158` `deleteRoleSet`, a plain `Delete(RoleSet)`. Rolling update is documented as "Designed for replica mode" (`docs/source/designs/aibrix-stormservice.rst` ~L734).
- **Gateway side (verified).** `pkg/utils/pod.go:83-85, 203-205`: `FilterReadyPod` excludes both terminating and draining pods. The drain window exists to let gateway caches converge before the engine receives SIGTERM. Without it, routing stops only when `DeletionTimestamp` is set. Whether in-flight requests are actually cut depends on engine shutdown behavior and `terminationGracePeriodSeconds`. That is a hypothesis, not measured here.
- **Known gap, never tracked.** The reviewer note on merged PR #2651 (maintainer `varungup90`, 2026-09-02, "RoleSet Teardown") flagged exactly this as non-blocking and suggested a design-doc note if it was intentionally out of scope. Neither a fix nor the doc note landed. `aibrix-stormservice.rst:65-76` presents drain as configurable on a StormService with no caveat.
- **No test coverage.** There is no unit test of `RoleSetReconciler.finalize` anywhere under `pkg/controller/roleset`.
- **Not a duplicate.**
  - #3187 (RFC) and open PR #3373 are gateway-driven, Deployment-scale-in work, and #3373 explicitly lists "StormService roles" as not included.
  - #2413 and #2651 delivered the base feature.
  - #3306 was closed unmerged.

**Review of the reporter's proposed patch (issues I found beyond their own caveats):**

1. **It would drain pods that never served traffic.** The patch passes the role's drain spec for every pod. The existing rollout helper `deletePodsForRollout` (`rolesyncer.go:580-592`) deliberately passes `nil` for not-ready pods. This was a review item on #2651. StormService scale-in also deletes not-ready RoleSets first (`sync.go:224-229`), so a naive patch adds a full drain wait to Pending or not-ready teardown. Reuse the `splitPodsByReady` split.
2. **The signature concern is overstated.** `finalize` is unexported and called from one place (`roleset_controller.go:120`). Returning the drain `RequeueAfter` instead of the fixed 15 s (`DefaultRequeueAfter`, `roleset_controller.go:46,119-126`) is a cheap, local change and removes the up-to-15 s lag.
3. **Whole-StormService deletion.** The PodSet finalizer already drains for `podGroupSize > 1` roles on that path, so draining here is consistent. It is still wasteful when no backend remains. Skipping drain when the owning StormService is being deleted is a reasonable but optional refinement.
4. **Surge interaction (verified by reading, not run).** `sync.go:316` computes surge from `len(allRoleSets)`, which includes terminating RoleSets, so a draining RoleSet holds its slot. With `maxSurge: 0` each batch waits at least the drain timeout. `readyCount` is taken from non-terminating RoleSets (`sync.go:290-292`), so `minAvailable` is still respected. This needs a docs note, not a code change.
5. **Drain reason.** The patch hard-codes `scale-in`; rollout deletions will be mislabeled. This is cosmetic (diagnostic annotation only).

**Production Impact**

- **Affected:** replica-mode StormService users who opted into `drain` on single-pod roles, which is the straightforward vLLM/SGLang shape.
- **Consequence:** the headline use case of the drain feature (rollouts and scale-in) silently does nothing for those roles.
- **Severity:** opt-in feature, no data loss, with a generic Kubernetes workaround (`preStop` sleep plus grace period, a hypothesis not tested here). So P2, but high within P2.

**Recommended Action**

- **Accept and Prioritize.** Maintainers should first settle three small questions: fix versus document; not-ready pods skip drain (recommended yes); drain on whole-StormService deletion (recommended: follow PodSet behavior, optionally skip when the owner is deleting). Then implement.
- **Labels:** `kind/bug`, `area/orchestration`, `area/stormservice`, `triage/accepted`, `priority/important-soon`. **Remove** the spurious `area/gateway`, `area/testing`, `area/website`, `area/runtime`, `area/batch` (see process notes).
- **Estimated effort:** Medium. The code change is small; tests (unit, plus envtest/e2e for the rollout and surge interaction) and the docs note are the bulk.
- **Confidence:** High on the defect, Medium on impact magnitude.

**Next Steps**

1. Decide fix versus doc; I recommend fixing, because the feature's stated purpose is controller-managed rollout/scale-in drain.
2. Implement with the not-ready split and `RequeueAfter` propagation.
3. Add the reporter's test plus a not-ready-pod case.
4. Add a docs note on drain semantics for RoleSet and StormService deletion, including the `maxSurge: 0` interaction.
5. Link to #2651, #3187 and #3373.

---

### Issue: #3368 — Batch result IDs collide because UUIDs are truncated to five hex digits

https://github.com/vllm-project/aibrix/issues/3368 · by `Jackmeson1` (CONTRIBUTOR) · created 2026-10-31 · labels `kind/bug`, `area/batch` · **open fix PR #3374** (by `AlSh007`)

**Summary:** Batch output records get `id = uuid.uuid4().hex[:5]` (about 1 M values), so ids collide at ordinary batch sizes although the docs call `id` a "Unique result identifier".

**Technical Assessment**

- **Confirmed.** `python/aibrix/aibrix/batch/job_driver/base.py:1914`.
- **Math verified.** Birthday collision probability is 37.9% at 1,000 lines and 98.6% at 3,000. Measured on the real, unpatched `_build_response`: 3,000 requests → 2,998 distinct ids; 50,000 → 48,828.
- **No AIBrix consumer of `id`.** Nothing in `python/aibrix/aibrix/batch` reads `["id"]` back. The only other writer is the synthesized `unreadable-{part_number}` at `batch/storage/adapter.py:801`, which cannot collide with hex ids. So the damage is limited to downstream clients that key on `id`. `custom_id` is the documented join key and is unaffected.
- **Docs already disagree with code.** `docs/source/features/batch-inference/batch-api.rst:739` says "Unique result identifier", and the sample output uses deterministic ids like `batch-def456-0`. Neither the old nor the new format matches that sample.
- **Reporter's format question.** Full UUID versus `job_id`-and-index. Considering both:
  - A full `uuid4().hex` is the smallest change and collision-free in practice (+27 bytes per line).
  - A deterministic id is retry-stable and matches the doc sample, but duplicates the information already in `response.request_id = f"{job_id}-{request_id}"` (`base.py:1943`).
  - Nothing depends on retry stability. Either is acceptable.
- **Other truncated UUIDs:** `base.py:274` (`_worker_token`, 8 hex) and `batch/manifest/renderer.py:664` (`batch-{hex[:8]}` job name). Neither is a practical problem and neither needs action here.

**Process note.** PR #3374 was opened roughly 15 hours after the issue, with no `triage/accepted`, and before the reporter's explicit "agree on the format first" question was answered. Both the reporter and a second contributor (`AlSh007`, "I'd like to take this one") volunteered. This is a small instance of the contribution-gate friction the AGENTS.md gate is meant to avoid; resolving it quickly prevents duplicate work.

**Production Impact:** low blast radius, silent data-merge risk for consumers who treat `id` as a primary key. Workaround: use `custom_id`.

**Recommended Action**

- **Accept and Prioritize.** Apply `triage/accepted`, answer the format question (full UUID is fine), review and merge #3374. Optionally fix the doc sample in the same or a follow-up docs PR.
- **Labels:** keep `kind/bug`, `area/batch`; add `triage/accepted`; no priority label needed beyond `priority/important-longterm` at most.
- **Estimated effort:** Small. **Confidence:** High.

**Next Steps:** confirm the format, land #3374, and close #3368 via the PR. Tell `Jackmeson1` the fix is already covered.

---

### Issue: #3370 — [Feature] Proxy Engine: Source-Agnostic Input/Output for Unmodified Inference Engines

https://github.com/vllm-project/aibrix/issues/3370 · by `DwyaneShi` (COLLABORATOR, self-assigned) · created 2026-10-31 · labels `kind/feature`, `area/batch` · Area field: "Not sure"

**Summary:** Add a proxy in front of an unmodified vLLM/SGLang. Input inspectors download remote media (`tos://…`) to a per-request local dir; preprocessors clip/transcode/resample; the engine sees only local files; postprocessors validate/package; output inspectors upload results to `output.url`.

**Technical Assessment: checking the proposal's premises against the code**

| Claim in the issue | Finding |
|---|---|
| Downloads/uploads use "AIBrix's shared storage layer … TOS, S3, GCS, HTTP, local paths" | **Partly inaccurate.** `storage/types.py:18-25`: `StorageType` = LOCAL, S3, TOS, REDIS (+AUTO). GCS exists only in the model-weights downloader (`downloader/gcs.py`). HTTP(S) is not a storage backend; `vipe_engine.py` has its own httpx download. |
| `tos://bucket/in/clip.mp4` addressed per request | **Not what the storage layer does.** `storage/factory.py:74-110` binds one bucket from env, and keys live inside it. Arbitrary-bucket URLs need a new URL-addressed layer. |
| This capability doesn't exist | **It exists once, ad hoc.** `openai_frontend/engine/vipe_engine.py` already does download (`:1209-1233`), ffmpeg frame extraction (`:1394-1442`), a per-request temp dir (`:1398`) and a TOS upload (`_build_result_dict`, `:152-165`) for one engine, with an http(s)-only URL validator (`schemas/vipe.py:28-37`) and a download size cap (`main.py:112`). The proposal never mentions it, or whether it supersedes or migrates it. |
| "Forwards its full API" | **Doesn't fit the current abstraction.** `engine/engine.py` `LLMEngine` is an in-process protocol 1:1 with chat/completion/embedding plus load/unload. A transparent pass-through proxy is a different shape. |
| Response carries `output: {url, artifacts}` | New schema field. Interacts with the schema-tracking work in #3367/PR #3366 ("track OpenAI, vLLM and SGLang"). Streaming (SSE) has no place for it, and postprocessor validation requires a buffered response. This is non-streaming only. |
| Problem is "especially batch" | **Overlaps existing/related work.** Batch already persists inputs/outputs in object storage (`batch/storage/adapter.py`), and RFC #3247 (video in Batch) designs its own content persistence. The issue doesn't say why per-pod staging beats batch-worker staging. |

**Gaps that block acceptance as written:**

- **Security model is absent.** Client-controlled `video_url` and `output.url` executed with the pod's credentials is an SSRF and confused-deputy surface: arbitrary bucket read/write, internal-endpoint fetches, and, if "local paths" is literal, arbitrary file read/write. It needs allowlists/prefix confinement, size and time limits, no credentials in requests, and a statement on decoding untrusted media with ffmpeg on a GPU node.
- **Operational semantics:** ephemeral-disk sizing per concurrent request; ffmpeg CPU contention with the engine; GPU time wasted if the upload fails after inference (idempotent output keys? retry?); cleanup on client cancel/disconnect; observability; sidecar versus in-container packaging.
- **Gateway interaction (unverified question):** whether ext_proc routing relies on inspecting media content or just the prompt; I did not trace that path.
- **No quantification:** no user, workload or measurement that shows the problem. The "Status" column of the component table is empty.
- **Alternatives to evaluate (unverified; `docs.vllm.ai` unreachable):** presigned URLs for inputs (engines already fetch http(s) URLs, per the issue's own text), vLLM media-IO and multimodal-processor options for frame rate/clip length, an init/sidecar staging container, or doing staging in the batch worker. The author should show which preprocessing truly cannot be expressed this way.

**Architectural alignment:** The need is real and the pattern is already in-tree, which supports generalizing it. But as scoped (inputs, outputs, pre- and postprocessors, all engines, all backends) it is a large, security-sensitive platform component. AGENTS.md says broad or sensitive changes need prior discussion and independent maintainer review. Postprocessors (schema validation, packaging) are the most speculative part.

**Production Impact:** none today (feature). Strategically valuable for multimodal/batch; exploratory until the design is reviewed.

**Recommended Action**

- **Design Discussion Required.** Ask for an RFC (retitle to `[RFC]`) with a security section and a narrowed v1. A sensible v1 is input-side staging only (download, local path, optional ffmpeg), scoped to offline/batch use, with `vipe_engine` as the first migration. Defer postprocessors and the response `output` schema until the first version proves out.
- **Labels:** keep `kind/feature`; add `area/multimodality`, `area/inference-engine`; reconsider `area/batch`. Do **not** apply `triage/accepted` for implementation yet.
- **Estimated effort:** Large. **Confidence:** Medium.

**Next Steps:** author to reconcile with `vipe_engine`, RFC #3247 and #3366/#3367; correct the storage-backend claim; add a threat model; and define failure semantics.

---

## D. Top Issues Worth Prioritizing

Only four issues were in scope, so this is a ranking of all four.

1. **#3377 — batch staging-object leak (P1, Small).** It affects every production S3/TOS batch of at least ~1,000 requests, grows without bound, and plausibly blinds the Files list API. The fix is a loop in two methods plus a test. It beats the rest on value-to-effort. Sequence the orphan sweeper separately.
2. **#3376 — drain skipped on RoleSet teardown (P2, Medium).** It silently disables the drain feature for the common replica-mode shape. The defect is reproduced and the fix direction is clear, but it needs the three small decisions plus tests and docs. It ranks below #3377 because it is opt-in and has no unbounded growth.
3. **#3368 — non-unique result ids (P2-low, Small).** A quick win: a one-line fix with a test, already in review. Merge to stop further effort on it.
4. **#3370 — proxy engine (P2, Large).** Strategically interesting, but the effort and risk are high and the design is missing. Resolve the design questions before spending engineering time.

---

## E. Issues Recommended for Closure

None. No duplicates, already-fixed or out-of-scope issues among the four.

- #3368 will close automatically when PR #3374 merges.
- #3370 should stay open as a design discussion. Do not close it, but do not start implementation.

---

## F. Proposed Maintainer Responses (drafts, not posted)

**#3377 (accept):**

> Thanks — reproduced independently: with a paginating `list_objects`, 900 parts leaves 0 staged objects, 1,205 leaves 206, and 5,000 leaves 4,001, while the assembled output is correct. Root cause is the discarded continuation token in `BaseStorage2.abort_multipart_upload` (`storage/base2.py`); the legacy `BaseStorage.abort_multipart_upload` (`storage/base.py`) has the same pattern, so please fix both. One caution: local/Redis tokens are offset-based, so don't delete between pages while following a token. Collect all keys then delete, or re-list from the start until empty with a progress guard. Please promote the paginating `list_objects` into the shared test double so the 5,000-part benchmark exercises it. Cleanup of already-orphaned `.multipart/` objects and making `GET /v1/files` skip internal keys at listing time (it lists `prefix=""` with a 1,000-key window and skips `.`-prefixed keys afterwards) are worth separate issues. Applying `triage/accepted` — go ahead with a PR. Did you get a chance to try TOS, or only S3/moto?

**#3376 (accept + decisions):**

> Confirmed on current main: `RoleSetReconciler.finalize` deletes pods directly, while `finalizePodSet` and the RoleSet syncer paths go through `controllerdrain.DeletePods`. This was noted as a non-blocking item in the #2651 review but never tracked or documented. We'd like to fix it rather than document it. Before you open the PR, please adjust for three points: (1) don't apply the role drain to not-ready/Pending pods; `deletePodsForRollout` in `rolesyncer.go` passes `nil` for them deliberately, and StormService scale-in deletes not-ready RoleSets first; (2) `finalize` is unexported with a single caller, so returning the drain `RequeueAfter` instead of the fixed 15 s is fine; (3) for whole-StormService deletion, follow the existing PodSet finalizer behavior (optionally skip drain when the owner is being deleted — we can decide in review). Please also add a docs note about the `maxSurge: 0` interaction, since terminating RoleSets hold a surge slot. Applying `triage/accepted`; I'm also trimming the labels to `area/orchestration` and `area/stormservice`.

**#3368 (accept):**

> Confirmed (3,000 requests → ~2,998 distinct ids). Full `uuid4().hex` is fine — we don't need a deterministic id, and it keeps the field opaque. PR #3374 covers it; please review that one and treat this issue as covered. Applying `triage/accepted`. The sample output in `batch-api.rst` still shows ids like `batch-def456-0`, which matches neither format; a small docs fix would be welcome.

**#3370 (design discussion):**

> Thanks for writing this up — the need is real, and `openai_frontend/engine/vipe_engine.py` already implements download → run → upload (with ffmpeg and TOS) for one engine, so a generic version is worth considering. Before we can accept it, could you turn this into an RFC that covers: (1) how it relates to `vipe_engine` (supersede/migrate?), RFC #3247, and the schema work in #3366/#3367; (2) a security model for client-supplied `video_url` and `output.url` (allowlists, credential scoping, SSRF, local paths, size/time limits, ffmpeg on untrusted input); (3) the storage claim: `StorageType` has only LOCAL/S3/TOS/Redis and binds a single bucket, so GCS/HTTP and arbitrary-bucket URLs aren't there today; (4) why staging belongs in a per-pod proxy rather than the batch worker, presigned URLs, or engine-native media options; (5) failure semantics (upload failure after inference, cancellation, streaming); (6) concrete workloads and measurements. I'd suggest narrowing v1 to input-side staging for offline/batch use and deferring postprocessors and the response `output` field. Keeping this open as a design discussion for now.

---

## Process observations for maintainers

- **Label hygiene.** #3376 carries `area/gateway`, `area/testing`, `area/website`, `area/runtime`, `area/orchestration` and `area/batch`; only `area/orchestration` (and `area/stormservice`) are right. The same over-labeling is visible on open issues #3333, #3297 and #3307 (`area/website`). The labeler appears to match on links or keywords in the body.
- **Contribution gate.** PR #3374 predates acceptance of #3368; consider a bot reminder when a PR references an issue lacking `triage/accepted`.
- **Test-double realism.** Storage test doubles that never paginate hide a whole class of bugs. A shared paginating fake would pay for itself.

---

## Maintainer Decision Brief

- **Immediate attention:** **#3377.** S3/TOS batch jobs of at least ~1,000 requests leak `.multipart/` staging objects (reproduced: 5,000 parts → 4,001 leaked) with no GC, and this plausibly empties `GET /v1/files`. Accept and let `Jackmeson1` fix `abort_multipart_upload` in both `base2.py` and `base.py`.
- **Highest-value work:** #3377 (Small, P1), then **#3376** (Medium, P2; reproduced on main; pass `nil` drain for not-ready pods and propagate `RequeueAfter`), then merge **#3368 / PR #3374** as a quick win.
- **Needs design review:** **#3370.** Security model, relation to `vipe_engine`/RFC #3247/#3366, and corrected storage assumptions are required before any implementation. Smaller in-thread decisions for #3376: drain on whole-StormService deletion and the `maxSurge: 0` doc note.
- **Needs clarification:** #3370 (author: RFC items above). Nothing else is blocked on information; #3377 only needs a TOS confirmation, which is optional.
- **Close or defer:** nothing to close. #3368 closes with #3374. Defer implementation of #3370 until the RFC is reviewed, and keep the orphan sweeper (follow-up to #3377) out of the first fix.
