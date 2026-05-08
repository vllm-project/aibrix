### ⚠️ 关键 Review 发现：Multi-Strategy Routing

虽然批量 soft-scoring 架构是一个明显改进，但这个版本的 PR 在状态隔离、多端口 pod 的负载均衡正确性，以及校验一致性方面引入了若干**关键回归**。

#### 🔴 高优先级（功能与逻辑 Bug）

**1. 有状态 router 在每次请求时都会丢失状态**
当前 `Select()` 在每次请求时都会调用 `newMultiStrategyRouter`，并为每个子策略调用 provider factory。这会导致每个请求都创建新的 router 实例。

* **影响：** 像 `prefixCacheRouter`（radix trie）这样的有状态 router 每次都会从空状态开始。该策略的分数将始终为 0，使核心使用场景（`prefix-cache:6,least-request:1`）完全失效。
* **修复建议：** 在 `RouterManager` 中缓存 `multiStrategyRouter` 实例，以 config string 作为 key。

**2. H1：`prefixCacheAndLoadRouter.ScoreAll` 在打分过程中修改状态**
`ScoreAll` 调用了 `p.cache.AddPrefix` ([[prefix_cache_preble.go:631](https://www.google.com/search?q=https://github.com/vllm-project/aibrix/pull/2124/files%23diff-35)](https://www.google.com/search?q=https://github.com/vllm-project/aibrix/pull/2124/files%23diff-35))，这是一个**写操作**。它会插入 tokens、增加节点 load，并更新 `lastAccess`。

* **影响：** 即使该策略没有在 weighted aggregation 中“获胜”，cache 状态也已经被推进。这会影响未来决策，并直接违反 “ScoreAll 只读” 的契约。
* **修复建议：** 为 `ScoreAll` 使用只查询的 `MatchPrefix` 方法，并把 `AddPrefix` 保留给 `PostRouteUpdate`。

**3. H2：Multi-port / DP-aware pod routing 被绕过**
`multiStrategyRouter.Route` 路径 ([[router.go:164](https://www.google.com/search?q=https://github.com/vllm-project/aibrix/pull/2124/files%23diff-34)](https://www.google.com/search?q=https://github.com/vllm-project/aibrix/pull/2124/files%23diff-34)) 从未调用 `SetTargetPort`。它只选择了一个 pod，然后回退到 `GetModelPortForPod`（默认端口）。

* **影响：** 对于包含多个 API server port 的 Data-Parallel（DP）pod，multi-strategy routing 会忽略 least-loaded port，可能把流量压到 pod 的第一个端口。
* **修复建议：** 重新定义 `ScoreAll` 以支持 `(pod, port)` 维度，或者给 `PodScorer` 增加一个 port-selection hook。

**4. KV-sync prefix-cache 在 multi-strategy 模式下不会更新**
在 `prefixCacheRouter.PostRouteUpdate` 中，如果存在 `kvSyncRouter`，该方法会直接返回。

* **影响：** 启用 KV-sync 的 `prefix-cache` 在 multi-strategy 配置下会停止 warming / 更新 prefix ownership。

**5. Validation 接受了 `Select` 无法实例化的配置**
`Validate` 只检查策略名称是否已注册，但 `Select` 要求策略必须实现 `types.PodScorer`。

* **影响：** `random,least-request` 会通过 validation（因为 random 已注册），但 routing 时会因为 `random` 不是 scorer 而返回 400 错误。

#### 🟡 中优先级（设计与正确性）

**6. M1：README 中关于 fallback 的说明已过期**
README 仍声称系统会在 invalid strategy 时 fallback 到 `Random`。在 commits `cf5c9d5b` 和 `427ea549` 之后，系统现在会正确返回 **400 Bad Request**。

* **修复建议：** 更新 `multi_router_readme.md` Example 4，使其反映 400 错误行为。

**7. M2：长度为 1 的 fast path 跳过了 `PostRouteUpdate`**
当 `len(pods) == 1` 时，`multiStrategyRouter.Route` 会提前返回。

* **影响：** 在单 pod 集群中，`PostRouteUpdate`（session-affinity headers、prefix-cache indexing）不会执行，状态不会继续演进。
* **修复建议：** 即使只有一个可用 pod，也仍然遍历并执行 `PostRouteUpdaters`。

**8. M3：`PostRouteUpdate` 执行顺序不确定**
当前循环使用 Go `map`，遍历顺序是随机的。

* **修复建议：** 按 `m.config.Items` 遍历，以保留用户定义的执行顺序。

**9. M4：`PostRouteUpdate` 失败会中断请求**
某个次要策略 update 发生瞬时失败（例如 tokenizer error）时，即使 pod 已经被正确选中，也会向用户返回 503。

* **修复建议：** 对非关键 update error 只记录日志并继续执行，不中断请求。

**10. M5：Session-affinity 会“跟随” winner**
如果 `least-request` 覆盖了 `session-affinity` 并选择了不同 pod，`PostRouteUpdate` 会把 session header 重写为新的 pod。

* **说明：** 这是一个语义变化（soft affinity）。请在 README 中记录该行为。

#### 🔵 低优先级 / Nits

* **L1：重构范围：** PR 描述声称 `least_util` 等已经重构为复用 `ScoreAll`，但它们仍然包含各自独立的循环。请同步代码或描述。
* **L2：Hot-path allocations：** diagnostic 使用的 `logBuilder` 和 `fmt.Sprintf` 每次请求都会执行。建议包在 `if klog.V(4).Enabled()` 中。
* **L3：Tie-break 注释：** “original order” 实际上已经被 `CryptoShuffle` 打乱。请更新注释以反映这一点。
* **L5：Polarity zero-value：** `PolarityLeast` 是 zero-value。如果某个策略忘记实现该方法，会静默默认成 “Lower is better”。

---

## 修复记录与当前状态

> 更新时间：2026-05-08。结论：High #1-#5 已修复并补充单测；Medium #7-#9 已修复并补充单测；#6、#10、L1、L2、L3 已处理；L5 不作为必修项。

### 已修复

| # | 状态 | 修复方式 | 验证 |
|---|---|---|---|
| 1. Stateful routers lose state | ✅ 已修复 | 在 `RouterManager` 中增加 `multiRouterCache`，通过 `getOrCreateMultiStrategyRouter()` 按 config string 缓存并复用 multi router，避免每次请求重复构造 sub-strategy router。 | 新增 `TestSelectCachesMultiStrategyRouter` 验证相同 multi config 复用同一个 multi router。 |
| 2. `prefixCacheAndLoadRouter.ScoreAll` 写状态 | ✅ 已修复 | 新增只读 prefix match 路径 `MatchPrefixNodeReadOnly()` / `matchPrefixHelperReadOnly()`，`ScoreAll()` 改为只读查询，不再调用会插入/更新状态的 `AddPrefix()`。 | 新增 `TestPrefixCacheAndLoadRouterScoreAllDoesNotMutateCache` 验证 `ScoreAll()` 不改变 prefix cache。 |
| 3. Multi-port / DP-aware routing bypassed | ✅ 已修复 | multi router 选中 winner pod 后调用 `setTargetPortIfNeeded()`；当 ready pods 是 multi-port pods 且配置里包含 `least-request` 时，复用 least-request cache，通过 `selectTargetPortForPodWithLeastRequestCount()` 选择该 pod 的 least-loaded port。 | 新增 `TestMultiStrategyRouterRoute_SelectsLeastLoadedPortForMultiPortPod` 验证 multi-strategy 路径会设置 least-loaded target port。 |
| 4. KV-sync prefix-cache 在 multi-strategy 不更新 | ✅ 已修复 | `prefixCacheRouter.PostRouteUpdate()` 在存在 `kvSyncRouter` 时委托给 `kvSyncPrefixCacheRouter.PostRouteUpdate()`；KV-sync updater 会 tokenize 请求、计算 prefix hashes，并对最终选中的 pod 调用 `syncIndexer.AddPrefix()`。 | 新增 `TestPrefixCachePostRouteUpdateUpdatesKVSyncIndexer` 验证 multi-strategy `PostRouteUpdate()` 会更新 KV-sync indexer。 |
| 5. Validate 接受 Select 无法实例化的配置 | ✅ 已修复 | `Validate()` 对 multi-strategy config 额外校验 provider 非 nil，并实例化 router 做 `types.PodScorer` type assertion；不实现 `PodScorer` 的策略会在 validate 阶段被拒绝。 | 新增 `TestValidateRejectsNonScorerInMultiStrategy` 和 `TestValidateRejectsNilProviderInMultiStrategy`。 |
| 7. 单 pod fast path 跳过 `PostRouteUpdate` | ✅ 已修复 | `multiStrategyRouter.Route()` 的 `len(pods)==1` fast path 现在会先 `SetTargetPod()`，再设置 target port，并调用 `runPostRouteUpdates()`。 | 新增 `TestMultiStrategyRouterRoute_PostRouteUpdateForSinglePod`。 |
| 8. `PostRouteUpdate` 顺序非确定 | ✅ 已修复 | 新增 `runPostRouteUpdates()`，按 `m.config.Items` 的用户配置顺序执行 updater，不再遍历 Go map。 | 新增 `TestMultiStrategyRouterRoute_PostRouteUpdateOrderAndErrorHandling` 验证执行顺序。 |
| 9. `PostRouteUpdate` 失败会 abort 请求 | ✅ 已修复 | `runPostRouteUpdates()` 对 updater error 只记录 `klog.Warningf`，不再向上返回错误，不影响已经完成的 pod 选择和请求路由。 | `TestMultiStrategyRouterRoute_PostRouteUpdateOrderAndErrorHandling` 同时验证 updater 失败不会中断路由。 |

### 额外修复

| 项 | 状态 | 修复方式 | 验证 |
|---|---|---|---|
| `RoutingContext` target port 状态泄漏 | ✅ 已修复 | `RoutingContext.reset()` 中增加 `r.targetPort.Store(0)`，避免 pooled context 复用时继承上一次请求的 target port。 | 新增 router context 复用测试，验证 `TargetPort()` 会被重置为 `0`。 |
| `TestModelRPSLimit` fixed-window flake | ✅ 已修复 | e2e 测试在发送请求前等待进入新的 Redis fixed-window 秒级窗口，避免第一个请求发生在窗口末尾导致第二个请求落入下一个窗口。 | 更新 `TestModelRPSLimit` 的 window 等待逻辑。 |
| goconst: repeated `"test-model"` | ✅ 已修复 | 在 `router_test.go` 中抽取 `const testModelName = "test-model"`，复用测试模型名。 | goconst linter 不再因重复字符串报错。 |

### 已解决的小问题

| # / Nit | 状态 | 当前判断 | 修复与验证 |
|---|---|---|---|
| 6. README fallback 说法过期 | ✅ 已修复 | Example 4 已说明 invalid strategy 会返回 `400 Bad Request`，但 README 顶部配置说明仍写 “fall back to default random router”，前后不一致。 | 已修改 `multi_router_readme.md` 顶部配置说明，统一为 invalid strategy 返回 `400 Bad Request`，不 silent fallback。纯文档修复，无需单测。 |
| 10. session-affinity “跟随 winner” | ✅ 已修复 | multi-strategy 下 session-affinity 是 soft score；如果 weighted aggregation 最终选择了不同 pod，`PostRouteUpdate()` 会把 session header 更新为最终 winner pod。 | 已在 README 明确说明 multi-strategy 下 `session-affinity` 是 soft-scoring 策略，response session header 会更新为最终 winner pod；新增 `TestSessionAffinityPostRouteUpdateFollowsFinalTargetPod` 验证 header 跟随最终 winner。 |
| L1 Refactor scope | ✅ 已修复 | 如果 PR 描述仍声称 `least_util` 等单策略已复用 `ScoreAll()`，则描述与代码实现不一致。 | 已让 `leastUtilRouter.Route()` 复用 `ScoreAll()` 获取批量分数，避免单策略路径和 batch scoring 逻辑继续分叉；新增 `TestLeastUtilRouteMatchesScoreAllMinimum` 验证 `Route()` 与 `ScoreAll()` 的最小 utilization 选择一致。 |
| L2 Hot-path allocations | ✅ 已修复 | `scoreAndRank()` 中 diagnostics 的 `fmt.Sprintf` / `strings.Builder` 仍会在每次请求执行，即使 V(4) 日志关闭。 | 已把诊断字符串构造整体包进 `if klog.V(4).Enabled()`，避免 hot path 非必要分配；新增 `TestScoreAndRankWithDiagnosticLoggingDisabled` 覆盖 V(4) 关闭时的无 diagnostics 路径。 |
| L3 Tie-break comment | ✅ 已修复 | 注释仍写 “original order”，但 ready pod list 进入 router 前已被 shuffle。 | 已将注释改为 “first pod in shuffled ready list”。纯注释修复，无需单测；tie-break 行为已有 `TestScoreAndRank` 覆盖。 |
| L5 Polarity zero-value | ✅ 不作为必修 | `PodScorer` 接口强制实现 `Polarity()`，不存在“忘记实现导致自动用 zero-value”的编译层面风险；真正风险是策略返回了错误 polarity。 | 不作为 blocker；依赖各策略单测覆盖 polarity 语义即可。 |

### 后续建议

1. 当前 comments.md 中列出的 High / Medium / Low 小问题均已处理或明确不作为必修项。
2. 后续只需根据 CI / reviewer 新反馈继续跟进。
