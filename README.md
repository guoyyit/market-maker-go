# 本地做市报价引擎（market-maker-go）

一个完全本地运行的做市报价引擎：消费内存管道注入的行情与成交数据，持续维护双侧报价，
根据持仓与风险状态调整报价，行情异常时先全量撤单保护；支持热更新、崩溃恢复与确定性回放。
**不依赖任何外部服务**：行情/成交来自本地内存通道或文件回放，撮合在进程内模拟完成。

## 包结构

- `engine/config.go`：配置项、校验与热更新合并。
- `engine/types.go`：领域类型（行情、成交、报价、意图、订单、动作、快照等）。
- `engine/quote.go`：双侧报价生成、库存价格/数量倾斜、仓位上限、确定性抖动 RNG。
- `engine/risk.go`：心跳停更与价格跳变保护、恢复条件、乱序行情隔离。
- `engine/position.go`：持仓账本与成交回报幂等去重。
- `engine/intent.go`：报价意图与在挂订单的增量对账（撤/改/挂）。
- `engine/gateway.go`：内存模拟订单网关（幂等提交）与确定性撮合。
- `engine/kernel.go`：纯确定性状态机内核，单线程逐步处理输入事件。
- `engine/engine.go`：内核 + 网关 + 管道 + 持久化的装配与事件循环。
- `engine/store.go`：本地文件持久化（原子快照 + 只追加事件日志）与内存实现。
- `engine/events.go`：统一输入事件与内存管道 `Feed`。
- `engine/replay.go`：确定性回放与轨迹逐条比较。
- `cmd/mmdemo`：最小演示程序。

## 报价规则

价格以“最小变动价位”的整数倍表示，数量以最小交易单位表示，均用 `int64`，
不使用浮点，避免回放时出现不确定性。

每笔被接受的有效行情驱动一次重报：

1. 计算中间价 `mid = floor((BidPrice + AskPrice) / 2)`；买/卖盘缺失时取 `LastPrice`。
2. 库存价格倾斜：`adjustedMid = mid - (position - TargetPosition) * SkewPerUnit`。
   多头偏离把报价整体下压（促进卖出、抑制买入），空头反之。
3. 各档报价：
   - 买 `price = adjustedMid - QuoteOffset - (level-1)*TickStep + jitter`
   - 卖 `price = adjustedMid + QuoteOffset + (level-1)*TickStep + jitter`
4. 库存数量倾斜：
   - 买量百分比 = `100 - deviation * QuantitySkewPctPerUnit`
   - 卖量百分比 = `100 + deviation * QuantitySkewPctPerUnit`
   - 百分比裁剪到 `[0,200]`，数量 = `BaseQuantity * pct / 100`（为正时至少 1 单位）。
5. **仓位上限**：`position >= MaxPosition` 时不再产生任何买单（停止同向增加多头敞口），
   卖单保留以便减仓；`position <= -MaxPosition` 时对称处理。
6. 最优买卖价差超过 `MaxSpread` 时产生空意图并全撤（`reason=spread_too_wide`）。
7. 可选确定性抖动：`RandomSeed != 0` 时每档价格附加 `{-1,0,+1}` 价位，
   随机数消费顺序固定为“每档先买后卖”；RNG 状态进入快照，跨重启轨迹保持一致。

### 报价生命周期与一致性

每次重报生成版本号单调递增的 `Intent`。`IntentReconciler` 以 `(方向, 档位)` 槽位为身份对账：

- 意图中消失的槽位 → 撤单；
- 槽位价格或数量变化 → **先撤旧单再挂新单**（撤改）；
- 未变化槽位 → 保留（增量更新）；
- 动作序列固定排序（撤单按 ClientID 升序，挂单先买后卖、档位升序）。

订单 `ClientID = Symbol-IntentSeq-side-Llevel`，是提交幂等键。
**任何时刻，内核记录的在挂订单集合与网关实际在挂订单集合、最近一次提交的报价意图严格一致。**

## 风险语义（行情保护）

- **价格跳变**：新中间价相对最近有效中间价的绝对变化超过 `MaxJumpTicks`（>0 时启用）
  → 立即生成空意图、全量撤单并进入保护。
- **心跳停更**：事件时钟相对最近有效行情的时间差超过 `StaleTimeout` → 同样全撤并进入保护。
  判定只使用输入事件携带的时间戳，不读墙钟。
- **恢复条件**：进入保护后，第一笔行情作为“恢复基准”（不做跳变检查），
  其后连续 `RecoveryTicks` 笔行情相对基准都不跳变才退出保护并重新报价；
  期间再次跳变则重置计数并等待新基准。
- 保护进入与退出、行情丢弃、配置更新等全部写入审计流（`AuditEvent`，见内核 `AuditEvents()`）。

## 乱序与重复输入

- 行情按 `Tick.Seq` 判序：`Seq <= 最近已接受 Seq` 的行情（迟到或重复）直接忽略，
  只记审计，不更新中间价、不触发重报、不触发模拟撮合。
- 成交回报按 `FillID` 全局幂等：同一 ID 只第一次生效，重复或迟到的同 ID 回报不改变持仓。
  持仓重算函数 `ReplayFills` 以“首次出现为准”，结果确定。
- 同版本、同内容的报价意图重复对账不产生动作；网关对 `ClientID` 幂等，重复提交被拒绝。

## 配置热更新

通过 `Engine.UpdateConfig(ConfigUpdate)` 或向管道投递 `ConfigUpdate` 事件完成。
`ConfigUpdate` 的 nil 字段保持原值；合并后做整体校验，**非法更新被整体拒绝**，
在挂订单与生效配置均不变。

对在途报价的生效方式是**确定且立即**的：

1. 更新一旦接受，立即生成新版本报价意图（版本号 +1）；
2. 与当前在挂订单做增量对账，仅变化的档位被撤改；
3. 不撤不重的档位保持原订单（含原挂单时间）；
4. 若更新时正处于保护状态，不重新报价，恢复后第一次报价直接使用新配置；
5. `RandomSeed` 热更新只切换抖动开关，不重置已持久化的 RNG 状态，保证轨迹仍可复现。

## 崩溃恢复

持久化由两部分组成（全部在本地目录，无外部服务）：

- `snapshot.json`：原子快照（写临时文件 + fsync + rename），记录配置、持仓、
  最近行情、最近报价意图、保护状态、意图版本号、已处理成交 ID 集合、RNG 状态；
  每个事件处理完成后刷新。
- `events.jsonl`：只追加的输入事件日志；**事件先 fsync 落盘，内核再生效**。

`RecoverEngine` 流程：

1. 读取最近快照；读取事件日志，提取其中全部成交回报；
2. 用成交回报序列重算持仓，与快照持仓核对；不一致返回 `ErrPositionMismatch`，拒绝带错报价；
3. 从快照重建保护状态、最近行情、意图版本号与 RNG 状态；
4. 以最近报价意图为权威集合对账恢复网关：缺失订单补挂（同 `ClientID` 幂等，
   不会重复挂单），多余订单撤销——因此**重启后不重复挂单、不丢失报价意图**；
5. 恢复后再次收到的重复成交回报仍被 `FillID` 去重。

## 确定性回放

`Replayer` 用固定种子与相同 `[]Envelope` 序列逐条重放（内存网关 + `MemoryStore`，无文件 IO），
输出包含每步动作、审计、成交、级联内部成交步骤与处理后持仓的 `TraceEntry` 轨迹；
`CompareTraces` 比较两次轨迹是否逐条一致。注入的网关可替换为不撮合的桩网关，
以单独验证输入→报价/撤单/持仓的确定性。

## 配置项一览（`engine.Config`）

| 字段 | 含义 |
| --- | --- |
| `Symbol` | 合约标识 |
| `Levels` | 每侧报价档位数 |
| `BaseQuantity` | 每档基础报价数量 |
| `TickStep` | 相邻档位价格间距 |
| `QuoteOffset` | 第一档相对中间价的偏移 |
| `MaxSpread` | 允许挂出的最大买卖价差 |
| `TargetPosition` | 库存目标持仓 |
| `MaxPosition` | 持仓硬上限（绝对值） |
| `SkewPerUnit` | 每偏离目标 1 单位的价格倾斜价位数 |
| `QuantitySkewPctPerUnit` | 每偏离 1 单位的数量百分比调整（0–100） |
| `MaxJumpTicks` | 单笔中间价最大允许跳变（0 关闭） |
| `StaleTimeout` | 行情心跳超时（0 关闭） |
| `RecoveryTicks` | 退出保护所需连续健康行情数 |
| `RandomSeed` | 确定性抖动种子（0 为不抖动） |

## 快速开始

```go
cfg := engine.DefaultConfig()
store := engine.NewMemoryStore()
e, _ := engine.NewEngine(cfg, engine.NewSimGateway(), store)

feed := engine.NewFeed(16)
go e.Run(context.Background(), feed)
feed.PostTick(engine.Tick{Symbol: cfg.Symbol, Seq: 1, BidPrice: 99, AskPrice: 101, /*...*/})
feed.PostFill(engine.Fill{FillID: "f1", Side: engine.SideBuy, Quantity: 10, /*...*/})
```

文件持久化与崩溃恢复演示：

```bash
go build -o mmdemo ./cmd/mmdemo
./mmdemo          # 首次运行，数据写入 ./demo-data
./mmdemo          # 再次运行自动 RecoverEngine
```

## 测试

```bash
go test ./...
go test -race ./...
```

测试覆盖：乱序/重复行情与成交、价格跳变与心跳停更保护、恢复条件、
报价增量对账、仓位上限、数量/价格倾斜、配置热更新（含非法更新拒绝）、
确定性回放一致性、文件持久化崩溃恢复（含持仓篡改检测与保护状态恢复）。
