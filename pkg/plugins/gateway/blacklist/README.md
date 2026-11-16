# Pod blacklist

How the gateway stops routing to a pod that is broken, overloaded, or no longer the engine it was registered as.

This directory is the Go package `blacklist`: the per-pod exclusion list, its background sweeps and their tests. It does not depend on `gateway.Server`.

## Enablement

The feature is **off by default**. Set `AIBRIX_POD_BLACKLIST_ENABLED=true` (or `1`) on the gateway to turn on the whole stack together: background probe sweep, load-outlier sweep, flaky re-add and quarantine. There are no per-policy toggles. Unset, empty, `false` or `0` leaves every exported method a no-op and starts no background work, so healthy-fleet routing matches the pre-blacklist path. While it is on, the load-outlier sweep takes over the load narrowing from the per-request load-imbalance gate: `selectTargetPod` skips `ApplyLoadImbalanceGate`, so a request is narrowed by one or the other, never both (the sweep's flag can be up to one sweep interval old and would otherwise combine with the gate's live view to leave a pool routing to a single pod). See [ENV_VARS.md](../ENV_VARS.md#pod-blacklist-blacklist).

**The blacklist does not look at request outcomes.** A failing status, an ext_proc stream that closes early or a client that gives up neither blame nor exclude the pod, and none of them creates an entry. Only what the sweeps and the cache report excludes a pod (sections 3 and 4). A pod that fails requests but still passes `/health` and `/v1/models` is therefore not excluded.

| File | Role |
|---|---|
| [`pod_blacklist.go`](pod_blacklist.go) | `PodBlacklist`, the per-pod exclusion list: `Allow`, `RecordProbeResult`, `SetLoadExcluded`, `OnPodFlakyReAdd`, `probe`; master switch `AIBRIX_POD_BLACKLIST_ENABLED` and the exclusion cap `AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT` |
| [`pod_blacklist_metrics.go`](pod_blacklist_metrics.go) | The Prometheus metrics (see Metrics) |
| [`pod_ignore.go`](pod_ignore.go) | `ignoredPod`: the pods the blacklist never tracks (StormService pods, prefill, decode and encode pods, ModelClaim pods) |
| [`pod_probe_sweep.go`](pod_probe_sweep.go) | The background probe sweep (every 300 s): `StartProbeSweep` |
| [`pod_load_blacklist.go`](pod_load_blacklist.go) | The load-outlier sweep (every 30 s): `StartLoadOutlierSweep` |
| [`../gateway_blacklist.go`](../gateway_blacklist.go) | Server-side glue: starts the sweeps, supplies the cache and queue-depth function, forwards `OnPodFlakyReAdd`, registers the blacklist as the cache's quarantine checker, applies the all-excluded and over-the-cap fallback, logs it and the un-blacklist line |
| [`../gateway.go`](../gateway.go) | Filters candidates (`FilterBlacklisted`) in `selectTargetPod` |

**Wiring.** `Server` holds a `blacklist.PodBlacklist` by value; the zero value is ready to use. The sweeps take two small interfaces instead of `Server`: `blacklist.PodSource` (`ListModels`, `ListPodsByModel`; `cache.Cache` satisfies it) and `blacklist.QueueDepthFunc`. The probe hooks `HealthCheck` and `IdentityCheck` are exported fields, so tests can replace the HTTP probes. When `AIBRIX_POD_BLACKLIST_ENABLED` is on, `NewServerWithOptions` starts the sweeps and registers the server with the cache as its pod lifecycle listener and the blacklist as its quarantine checker, each only if the cache implements the matching optional registry (`cache.PodLifecycleRegistry`, `cache.PodQuarantineRegistry`). While the switch is off those steps are skipped. `Shutdown` undoes both. The quarantine checker is a single slot shared by everything using the cache, so `Shutdown` clears it only if it still holds this server's blacklist.

## Pods the blacklist ignores

Three kinds of pod are never tracked, whatever the probe verdicts say (`ignoredPod`):

- **Pods created by a StormService**, recognised by the `storm-service-name` label the StormService controller puts on every pod it renders. They are the prefill, decode and encode pods of a disaggregated deployment, which the PD router handles per role and RoleSet instead of by excluding one pod.
- **Prefill, decode and encode pods**, by the `role-name` label (`prefill`, `decode` or `encode`), whoever created them. The PD router pairs the roles within a RoleSet, so excluding every pod of one role leaves it nothing to pair while ready pods exist, which the blacklist's fallback (it only covers a model with nothing left, or too little) does not help with. The probe sweep never probed them either, and the load sweep and flaky re-add now leave them alone as well.
- **Pods that carry a ModelClaim binding**, recognised by an annotation with the prefix `route.claim.model.aibrix.ai/`. A warm pod runs one engine per claimed model, each on its own port. A verdict about the pod would mix those engines: one dead engine would take the healthy ones on the same pod out of routing.

What that means for each kind of deployment:

| Pods | Protected by the blacklist | Where the filter sees them |
|---|---|---|
| Plain engine pods (no StormService, role or ModelClaim) | Yes, by every source | Filtered on every strategy |
| Prefill and decode pods (PD) | No, never tracked | The PD strategy filters the whole ready list first and admits them; the PD router handles failures per role |
| Encode pods (E+PD) | No, never tracked | Split off before the filter on every strategy except PD, and handed to the encode stage as they are; the PD strategy keeps them in the list it filters, and admits them |
| StormService pods | No, never tracked | Filtered like any other list entry, and always admitted |
| ModelClaim pods | No, never tracked (the blacklist does not key by endpoint) | Always admitted |

A request can therefore mix both kinds. A model whose pods are all ignored behaves as if the blacklist were off. This is deliberate rather than an oversight: for a pod that is part of a RoleSet the PD router, not a per-pod verdict, decides what is still usable.

For such a pod `RecordProbeResult`, `SetLoadExcluded` and `OnPodFlakyReAdd` do nothing and create no entry, so `Allow` always admits it and it is never quarantined. The probe sweep does not probe it, and the load-outlier sweep leaves it out of both the scoring and the peer baseline (it does not count toward the 3-pod minimum either). The check is made when a verdict is recorded, so a pod that gains a ModelClaim annotation later keeps any entry it already had until the 30-minute pruning removes it; warm pods are not routed to before they hold a claim, so this does not arise in practice.

## The whole picture

A pod is excluded from routing for one of three reasons. Two of them drive the same per-pod breaker (open or closed); the load-outlier flag is separate. Routing reads both through `Allow(pod)`.

```mermaid
flowchart LR
    subgraph sources["Who excludes a pod"]
        B["Background probe sweep<br/>every 300 s"]
        D["Flaky re-add<br/>cache event"]
        C["Load-outlier sweep<br/>every 30 s"]
    end
    B --> S["Per-pod breaker state<br/>closed / open"]
    D --> S
    C --> L["loadExcluded flag"]
    S --> G{"Allow(pod)"}
    L --> G
    G -->|allowed| R["Routing candidates"]
    G -->|excluded| X["Skipped for this request"]
```

Exclusion is a soft preference. If every ready pod of a model is excluded, the gateway routes across all of them rather than fail the request.

State is **per gateway process**, held in memory. With several gateway replicas each one probes and excludes pods on its own, and a restart starts from an empty state.

## 1. Breaker states

```mermaid
stateDiagram-v2
    [*] --> Closed
    Closed --> Open: 3 degraded probe sweeps in a row, ordinary backoff
    Closed --> Open: 3 dead probe sweeps in a row, 1 hour
    Closed --> Open: flaky re-add, held 3 s
    Open --> Open: 3 more dead probe sweeps, renewed for 1 hour
    Open --> Closed: 2 healthy probe sweeps in a row, block lifted
    Open --> Closed: block ended, next sweep (the pod is routable as soon as the block ends)
```

**Backoff** doubles per consecutive trip: 1 min, 2, 4, 8, then capped at 10 min. A pod the probe sweep finds dead is blacklisted for **1 hour** instead (section 3).

**No request probes a recovering pod.** Nothing reports a request's outcome, so there is no half-open state: when a block ends the pod is a routing candidate again (`Allow` goes by the deadline), and the next sweeps decide. The breaker is moved back to closed by whichever sweep result or flaky re-add comes next (`lapseLocked`). A healthy sweep after the block has ended also resets the trip count, so a recovered pod is not punished for its earlier block. A block that has **not** ended is lifted by `podProbeRecoveryIterations` (2) healthy sweeps in a row, a quarantine included (section 3). A pod that is still degraded or dead takes traffic until the sweeps block it again, which needs another 3 failing sweeps in a row (about 10 minutes). For a degraded pod that backoff is the next, longer one.

## 2. Routing gate: `Allow`

`FilterBlacklisted` calls `Allow` on each pod it is given, before the routing algorithm runs. It does no network work. While no entry can exclude its pod (`excluded`, an exact count of the entries that are open or load-excluded, is zero) it returns the pods untouched without a lookup or an allocation, so a pod that has failed one probe, whose entry excludes nothing, does not put every request on the per-pod path. The count has no clock: it counts an open breaker even after its block has ended, so it can only over-count what `Allow` rejects, never under-count it, and `Allow` and `IsBlacklisted` answer from one deadline-aware check (`blocksLocked`). An entry whose block has ended stays counted, and keeps requests on the per-pod path, until the next probe result for the pod closes it or the 5-minute prune does; the prune is what ends it for a pod that has left the fleet and is no longer probed. When it does look, it allocates only if it finds a pod to drop. An entry is created only by a failed sweep probe, a load exclusion or a flaky re-add, never by a request.

`Allow` only reads. It takes the entry's read lock, so one request never waits for another to finish with a pod's entry, and it does not count as a touch: an entry that routing keeps looking at but nothing changes ages out after `podBlacklistEntryTTL` like any other. The read lock's counter is still shared, so many goroutines asking about the same pod at once cost on the order of 100 ns each (measured: about 130 ns per call with 12 goroutines on one pod). What keeps routing cheap is not the lock but the `excluded` gate above, and with the candidate stage looking at K pods rather than all of them.

Which pods it is given depends on the candidate stage (`AIBRIX_ROUTING_CANDIDATE_K`, see [ENV_VARS.md](../ENV_VARS.md#candidate-stage-algorithmscandidatesgo)). When the stage narrows a request to K pods, `selectTargetPod` filters those K pods after the stage picks them, so the lookups per request stay at K however many pods are ready or tracked. A few excluded candidates are dropped without replacement; if all K are excluded and other ready pods are not, the stage runs again over the ready pods that are not excluded. Everywhere else the whole ready list is filtered, ahead of the stage: for the exclusive strategies (`pd`, `slo*`), which see every ready pod, and when the request has a replica-inflight limit, whose filter reads the running count of every ready pod and must not see excluded ones. If every ready pod is excluded, or too many are, the request routes to them anyway (next).

| Pod state | `Allow` returns |
|---|---|
| No entry, or closed | true |
| `loadExcluded` set | false (checked first) |
| Open, before its deadline | false |
| Open, deadline passed | true |

`IsBlacklisted` is the read-only variant. It reports a pod only while it is load-excluded or open before its deadline.

**The exclusion cap.** The blacklist may exclude at most `AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT` (default 50) of the pods a request considers. Past that, `selectTargetPod` routes the request across all of them as if none were excluded, and counts it (`gateway_pod_blacklist_fallback_total{reason="over_cap"}`, with a throttled `too many pods are blacklisted` log line). It is the same fallback as for a model with every pod excluded, extended to most of them: the few pods left would carry all the model's traffic, and a verdict wrong for many pods at once (a network fault on the gateway's side, a probe path that something changed) is more likely than most of a fleet failing together. At the default one pod of two, two of four and three of eight may be excluded; two of three may not. A model with a single pod therefore never excludes it. `100` turns the cap off: only a model with every pod excluded falls back. With the candidate stage the cap is judged on the K candidates a request is given. When they are over it but the ready pods as a whole are not, the stage runs again over the pods that are not excluded; when the ready pods are over it too, the request goes across the candidates. Near the cap a sample can fall either side by chance, so some requests keep their exclusions and some do not.

## 3. Background probe sweep

The sweep is the only thing that looks at a pod's health. It catches a pod that went bad, a stale registration whose engine is gone, and one whose address now belongs to another process, whether or not the pod is being sent traffic.

Every **300 s** it probes every routable pod once, except the pods the blacklist ignores (see above: prefill, decode and encode pods among them), which the sweep never probes. A pod serving several models, such as a base model and LoRA adapters, is probed once, against its own served model. Pods that are already open are probed too. A probe (`probe`) gives one of three verdicts:

| Verdict | When | After 3 sweeps in a row |
|---|---|---|
| **Dead** | `GET /health` fails (transport error, or non-2xx for a `vllm`/`sglang` pod; any other engine only has to answer at all), **or** for a `vllm`/`sglang` pod with a known model, `GET /v1/models` answers 2xx with an empty or non-JSON body | Blacklists the pod for **1 hour**, whatever state its breaker is in |
| **Degraded** | `/health` is fine but `GET /v1/models` is not 2xx, is a JSON list without the model (a reload in progress looks like this), or did not arrive in time | If the breaker is closed, trips it with the ordinary backoff: 1 minute, doubling |
| **Healthy** | Everything passes | Nothing; a healthy sweep restarts both counts, and 2 in a row lift a block that is still running |

`GET /health` only proves that *some* process answers on the address. Under host networking a port can be recycled, so a stale registration may point at an unrelated listener that answers `/health` 200 and serves nothing. For engines that list their models (`vllm`, `sglang`; the engine comes from the pod's `model.aibrix.ai/engine` label), the model list is therefore an **identity check**: `GET /v1/models` must be a 2xx whose body parses as `{"data":[{"id":...}]}` with one `id` equal to the pod's model.

**Fail-open (healthy) vs fail-closed (failed):**

| Probe | Fail-open | Fail-closed |
|---|---|---|
| `/health`, `vllm` or `sglang` pod | Empty address, request construction error | Transport error, non-2xx |
| `/health`, any other engine or no engine label | Empty address, request construction error, **any HTTP response** (xLLM answers 404 on `/health` while serving and xDiT has no such route) | Transport error only |
| `/v1/models` | Empty address or model, construction error | Transport error, non-2xx, model not listed, a body that did not arrive in time or was cut off (all degraded); **empty or non-JSON 2xx** (dead) |

An engine joins the strict `/health` list, and the identity check, once its behaviour has been verified; until then a pod is only required to answer. The probe goes through an HTTP client with a transport of its own and no proxy: `http.DefaultTransport` would honour `HTTP_PROXY`, and a proxy that cannot reach the pod network answers like a dead pod.

```mermaid
flowchart TD
    T["Every 300 s"] --> L["List models, then routable pods<br/>each pod once"]
    L --> A["Resolve the address<br/>the port label or ModelClaim port,<br/>same as routing"]
    A --> P["probe(addr, model, engine)<br/>up to 16 in parallel"]
    P --> V{"Verdict"}
    V -->|healthy| Reset["Reset both counts<br/>block ended: restart the backoff<br/>2 in a row: lift a running block"]
    V -->|degraded| Dg["Dead count = 0<br/>failure count + 1 while closed"]
    V -->|dead| Dd["Dead count + 1<br/>failure count + 1 while closed"]
    Dd --> DN{"Dead 3 sweeps in a row?"}
    DN -->|yes| BL["Open for 1 hour<br/>never shortens an existing deadline<br/>renews it if already open"]
    DN -->|no| Wait["Log it and re-check next sweep"]
    Dg --> GN{"Closed and failed<br/>3 sweeps in a row?"}
    GN -->|yes| Tr["Ordinary trip:<br/>backoff 1 min, 2, 4 ..."]
    GN -->|no| Wait
    BL --> Rec["After the hour the pod is routable again;<br/>still dead, it is blacklisted anew<br/>after 3 more dead sweeps"]
```

- **Consecutive means consecutive.** A healthy sweep restarts both counts, and a degraded sweep restarts the dead count. Three failing sweeps that are not all dead still trip the ordinary breaker, so that pod gets one minute, not an hour. Detection takes about 10 minutes from the first failure (3 sweeps, 5 minutes apart). To act on the first failed sweep, change the `podProbeFailureIterations` constant to 1.
- **A dead pod stays out while it stays dead.** It keeps being probed, and every further 3 dead sweeps renew the hour.
- **A healthy pod does not serve out the hour.** A single healthy sweep only restarts the counts (and the trip count once the block has ended), but `podProbeRecoveryIterations` (2) healthy sweeps in a row lift a block that is still running, a quarantine included, so the cache scrapes the pod again. A failing sweep in between starts that count over, so a pod that flaps does not leave on one lucky probe. A load exclusion is not a sweep block: only the load sweep clears it. The stale-entry pruning counts `podBlacklistEntryTTL` from the later of the entry's last update and its `openUntil`, so a quiet, recovered pod's block is not cut short.
- **A pod blacklisted this way is quarantined.** `IsQuarantined(pod)` is true for exactly this one-hour block (not for an ordinary backoff, a load exclusion or a flaky re-add). The gateway registers the blacklist with the cache (`Server.registerPodQuarantine`), and the cache then, for that pod: does not scrape its `/metrics` and does not emit its `model_replicas` series (the existing stale-series cleanup removes the one already emitted). The pod stays in the cache, and the sweep still probes it (that is what renews the hour). When the hour ends the metrics loops pick it up again. The metrics last scraped stay in the cache's metric store, so routing across an all-excluded model still has a value to read for it. The per-pod engine gauges already exported (`num_requests_running` and the like) keep their last value until the pod is scraped again; nothing deletes them, so a dashboard shows them flat for the hour. The load-outlier sweep (section 4) does not read those frozen values: it leaves a quarantined pod out of its peer set.

## 4. Load-outlier flag and flaky re-add

- **Load outlier.** Every 30 s, each model with at least 3 ready pods is scored on engine-reported running plus waiting requests. A pod is flagged when it is more than 2 standard deviations above the mean of its *peers* (the other pods, not the group including itself) **and** at least `AIBRIX_LOAD_OUTLIER_MIN_GAP` (default 16) above it. The flag has no backoff or probe: the first sweep that finds the pod back in range clears it. It is independent of the breaker. A pod that serves several models is flagged if any of them scores it an outlier. A pod with no measurement (it joined after the last scrape, or its metrics are missing) is left out of that model's scoring: it is not scored, is not a peer, and does not count toward the 3-pod minimum, because read as load 0 it would drag its peers' mean down and flag the one pod carrying the traffic. A pod that loses its measurement is no longer flagged. A pod the blacklist ignores (see above) is left out like a quarantined one: never flagged, not a peer, and not counted toward the 3-pod minimum. A quarantined pod (section 3) is left out entirely: the cache no longer scrapes it, so its load is as old as the quarantine and would skew its peers' mean and standard deviation. It does not count toward the 3-pod minimum, and the sweep neither sets nor clears its flag until the quarantine ends. A pod that is load-excluded, or in an ordinary backoff, is still scraped and is still scored.
- **The load-imbalance gate is separate.** The gateway's per-request load-imbalance gate (`ApplyLoadImbalanceGate`, see [load_balance.md](../algorithms/load_balance.md)) narrows a request to the least-loaded pods from the gateway's live running counts. It runs whether or not the blacklist is on, and is not replaced by this sweep: the blacklist removes the pods it excludes first, and the gate then narrows the pods that are left. The sweep sees engine-side queueing and reacts only once per sweep; the gate reacts on every request.
- **Flaky re-add.** When the cache sees a pod reappear at the same IP after being absent for at least 250 ms (and less than the 60 s a deleted pod's counters are kept for), `OnPodFlakyReAdd` opens the breaker for 3 s without checking anything, and the pod is routable again after that. The in-place pod update the informer handles as a delete and re-add in one call is not a flaky re-add. The trip count is left alone. A pod that is already open keeps the later deadline.

## Constants

| Constant | Value | Meaning |
|---|---|---|
| `podBlacklistBaseDuration` / `podBlacklistMaxDuration` | 1 min / 10 min | Backoff start and cap |
| `podProbeTimeout` | 5 s | Timeout per request of a sweep probe. Nothing waits on the sweep and a slow pod is not a dead one: `/health` on the sglang pods takes 1.4-1.7 s |
| `podHealthCheckPath` / `podModelsCheckPath` | `/health` / `/v1/models` | Probe paths |
| `podProbeSweepInterval` | 300 s | Background sweep period |
| `podProbeFailureIterations` | 3 | Consecutive failed (or dead) sweeps before the sweep acts |
| `podProbeRecoveryIterations` | 2 | Consecutive healthy sweeps that lift a block that is still running |
| `podProbeDeadBlacklistDuration` | 1 h | How long the sweep blacklists a pod it found dead |
| `podProbeConcurrency` | 16 | Probes in flight during one sweep |
| `flakyReAddOpenDuration` | 3 s | Hold after a flaky re-add |
| `podBlacklistEntryTTL` / `podBlacklistSweepInterval` | 30 min / 5 min | An entry is pruned once it has been untouched this long, counted from the later of its last update and its `openUntil`. Each pass also closes a breaker whose block has ended, so it stops counting as excluded |
| `loadOutlierCheckInterval` | 30 s | Load-outlier sweep period |
| `loadOutlierStdDevThreshold` | 2.0 | Standard deviations above the peer mean |
| `loadOutlierMinPods` | 3 | Fewest ready pods for a model to be scored |
| `AIBRIX_POD_BLACKLIST_ENABLED` (env) | `false` | Master switch for the whole feature (see [Enablement](#enablement) and [ENV_VARS.md](../ENV_VARS.md#pod-blacklist-blacklist)) |
| `AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT` (env) | 50 | Most of the pods a request considers the blacklist may exclude; past it the request is routed across all of them. `100` turns the cap off (see [Routing gate](#2-routing-gate-allow) and [ENV_VARS.md](../ENV_VARS.md#pod-blacklist-blacklist)) |
| `AIBRIX_LOAD_OUTLIER_MIN_GAP` (env) | 16 | Minimum absolute running+waiting gap over the peer mean (see [ENV_VARS.md](../ENV_VARS.md#pod-blacklist-blacklist)); only used when the master switch is on |

## Operating it

**Turn it on.** Set `AIBRIX_POD_BLACKLIST_ENABLED=true` on every gateway replica that should exclude broken or overloaded pods. Restart (or roll) the gateway so the process picks up the env var. Leave it unset or `false` for the pre-blacklist routing path.

**See what is excluded.** There is no query endpoint; read the log lines below. State is per replica, so look at the log of the replica that served the request.

**Log lines** (all through the gateway log). Each one that names a pod also gives the ip:port and model it is reached on, as `pod ns/name (addr=ip:port, model=name)` (or `addr` and `model` fields in the structured ones), so a line can be matched to a curl command. The address is derived from the pod, as the sweeps and routing derive it.

| Message contains | Level | Meaning |
|---|---|---|
| `pod failed a background probe; it will be re-checked` | Info | Sweep failure 1 or 2 for a pod whose breaker is closed |
| `excluded pod failed a background probe as dead; it will be re-checked` | Info | Dead sweep 1 or 2 (`deadSweeps=`) for a pod that is already open. Without it an excluded pod is silent until the sweep that blacklists it for an hour. Degraded sweeps of such a pod are not logged. |
| `blacklisted for 1h0m0s: background probe found it dead … sweeps in a row` | Warning | The sweep found the pod dead and blacklisted (or renewed) it for an hour |
| `blacklisted for …: background probe failed … sweeps in a row` | Warning | The sweep tripped a degraded pod with the ordinary backoff |
| `pod un-blacklisted: background probes found it healthy` | Info | `podProbeRecoveryIterations` healthy sweeps in a row lifted a running block |
| `pod briefly excluded after a flaky delete/re-add` | Info | Flaky re-add |
| `all ready pods are blacklisted; routing anyway` | Info | Every pod of a model is excluded; throttled to one line per model per 10 s |
| `too many pods are blacklisted; ignoring the blacklist for this request` | Info | More than the cap of the pods are excluded; throttled like the one above |
| `pod flagged as a load outlier` | Info | Every load sweep while the pod is an outlier, with the peer mean, stddev and gap |
| `blacklisted: running+waiting request count is a statistical outlier` | Warning | The pod became load-excluded |
| `pod un-blacklisted: running+waiting request count has converged` | Info | The pod left load exclusion, with the `gateway_total` and `engine_total` it was judged on |

A block that simply ends logs nothing.

**Metrics** (Prometheus, labelled with the model, the LoRA adapter and the gateway pod, never with the pod itself):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gateway_pod_blacklist_exclusions_total` | counter | `reason`: `probe_dead`, `probe_degraded`, `load_outlier`, `flaky_readd` | A pod became excluded. Renewing a block, or a flap on a pod that is already excluded, is not counted again. |
| `gateway_pod_blacklist_recoveries_total` | counter | `reason`: `probe_healthy`, `load_recovered` | A pod's exclusion was lifted before its deadline |
| `gateway_pod_blacklist_probe_total` | counter | `verdict`: `healthy`, `degraded`, `dead` | Every probe the sweep sends. A burst of `dead` across a whole model is more likely a fault on the gateway's side than a fleet of dead pods. |
| `gateway_pod_blacklist_excluded_pods` | gauge | | Pods this gateway currently excludes, as of the end of the last sweep |
| `gateway_pod_blacklist_fallback_total` | counter | `reason`: `all_excluded`, `over_cap` | A request routed with the blacklist ignored. Counted for every request, not throttled like the log line. |

**Tests:**

```
go test ./pkg/plugins/gateway/blacklist/
go test ./pkg/plugins/gateway/ -run 'Blacklist|handleRecvError|selectTargetPod'   # the Server-side wiring
```

## Known limitations

- Exclusions are per gateway replica, so each replica has to find a bad pod on its own sweeps before it excludes it.
- A pod is found by the sweep, not by the requests it fails: one that answers `/health` and `/v1/models` correctly but fails inference is not excluded, and a pod that dies is routed to for up to about 10 minutes (3 sweeps) before it is blacklisted.
- A block that ends is not followed by a probe request: a pod that is still bad takes traffic until the sweeps block it again (3 more failing sweeps in a row).
- A probe that cannot reach the pod for a reason of the gateway's own (a network problem on the gateway's side) looks like a dead pod. After 3 sweeps every pod so affected is blacklisted, and the cache stops scraping it. The exclusion cap keeps most of the fleet routable meanwhile, the `gateway_pod_blacklist_probe_total{verdict="dead"}` burst shows what happened, and 2 healthy sweeps lift it once the fault clears.
- A `/health` that takes longer than the sweep's wait (`podProbeTimeout`, 5 s) is judged dead; the sglang pods answer in about 1.5 s, inside the limit. A `/v1/models` body that does not arrive in time is degraded, not dead.
- Only `vllm` and `sglang` pods are required to answer `/health` with a 2xx, and have their model list checked. Any other engine is only required to answer, so a pod of one that is broken but still serving HTTP is not found by the sweep.
- A pod that is excluded between the filter and the router's pick is still routed to: the replica-inflight admission (`AdmitTarget`) does not re-check exclusion. Admission fails the request when it says no, and the blacklist is a soft preference that should not fail a request that has not itself failed. The window is the time between two steps of one routing decision.
- A stale registration is only fully fixed at the source: the registration side should deregister the endpoint or verify identity. The blacklist limits the damage, and the sweep finds it without traffic.
