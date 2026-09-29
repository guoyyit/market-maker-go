# 报价规则

所有价格以**最小价位**为整数单位，数量以**最小数量单位**为整数单位，
全程不使用浮点数，保证跨平台、跨重启的确定性。

## 输入

- 行情帧 `Tick{BidPrice, AskPrice, Seq, Time}`，中间价
  `Mid = (Bid + Ask) / 2`（整数向下取整）。
- 成交回报 `Trade{ID, MakerOrderID, MakerSide, Price, Qty, Seq}`：
  买入成交使持仓 `+Qty`，卖出成交使持仓 `-Qty`。

## 两侧档位

给定配置 `HalfSpread`（最优档半价差）、`LevelStep`（相邻档间距）、
`Levels`（每侧档位数），对档位 `l = 0 .. Levels-1`：

```
fairMid = Mid - skewShift
offset  = HalfSpread + l * LevelStep
bid[l]  = fairMid - offset + jitter
ask[l]  = fairMid + offset + jitter
```

返回顺序固定为「买侧档位 0..N-1，随后卖侧档位 0..N-1」。

## 库存倾斜

```
deviation = position - InventoryTarget
skewShift = SkewCoeff * trunc(deviation / SkewDenom)
```

- 整除向 0 截断，正负偏离语义对称。
- 持仓**高于**目标时 `skewShift > 0`，`fairMid` 下移：买价与卖价同时下移，
  使卖出更容易被成交、买入更难，引导库存回落。
- 持仓低于目标时对称地上移。

## 仓位上限

- `position >= +MaxPosition`：**停止挂买单**，只保留卖侧报价，
  不再增加多头敞口。
- `position <= -MaxPosition`：**停止挂卖单**，只保留买侧报价。
- 回落到上限以内后，下一次报价自动恢复缺失的一侧。

## 确定性抖动

- `JitterTicks > 0` 时，每个**新行情帧**从 PCG 随机流取一个值
  `jitter ∈ [-JitterTicks, JitterTicks]`，同一帧买卖两侧共享。
- 抖动只平移价位水平，不改变两侧价差。
- 同一行情帧内因**成交补挂**或**配置热更新**触发的重新报价**复用**该抖动，
  不额外消耗随机数。因此同帧补挂价格与该帧首次报价一致。
- 随机流位置（抽取计数）随快照持久化，重启后通过空转对齐，
  恢复后下一帧抖动与未崩溃时一致。

## 订单代号（ClientOrderID）

格式：`SYMBOL-SIDE-LEVEL-GENERATION`，例如 `DEMO-BUY-0-2`。

- 同一档位在挂期间 `GENERATION` 不变。
- 两种情况使代号加 1（下一节解释原因）：
  1. 该档报价**被成交**；
  2. 该档仍在挂但**价格或数量变化**（行情移动、库存倾斜、热更新）。
- 若档位因减档或触限被撤销、之后又恢复挂出，也会换用新代号。

## 增量撤改与"在挂集合 == 最近意图"

本地网关规定：已成交或已撤销的订单 ID 进入**终态、不得复用**。
因此价格变化不能在同一 ID 上原地改单，而是确定地表达为
「撤销旧代号单 → 挂出新代号单」。

每次生成意图后，`book` 对账器比较「网关在挂集合」与「新意图」：

1. 意图中消失、或价格/数量不一致的档位 → `Cancel`；
2. 意图中存在但在挂缺失（含刚撤销的变价档）→ `Place`；
3. 完全一致的档位 → 无动作。

动作顺序固定：**先全部撤销（按 ID 升序），再全部挂单（按 ID 升序）**。
对账成功后固化"最近意图"。因此任何时刻对账完成后：

> 网关在挂报价集合与最近一次提交的报价意图逐单一致
> （相同 ID、相同价格、相同数量）。
