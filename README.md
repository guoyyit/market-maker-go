# market-maker-go

本地做市报价引擎：消费本地模拟/回放的行情与成交数据，持续维护两侧报价，
按持仓与风险状态调整报价，行情异常时先全量撤单进入保护。

**不依赖任何外部服务**：行情、成交通过进程内通道注入，订单在内存网关中撮合，
状态持久化到本地目录。

## 快速开始

```bash
go run ./cmd/mm -dir ./run-state
go test ./...
```

演示会按固定序列注入行情、成交、心跳与配置事件，逐条打印报价、撤单、
持仓与保护状态变化；同一种子下输出可重复。

## 文档

- [报价规则](docs/quoting.md)：双边档位、库存倾斜、仓位上限、订单代号。
- [风险语义](docs/risk.md)：心跳停更、异常跳变、非法价差的保护与恢复。
- [配置项与热更新](docs/configuration.md)：参数含义、校验、在途报价生效方式。
- [崩溃恢复](docs/recovery.md)：持久化内容、重启对账、持仓重算。
- [确定性回放](docs/replay.md)：固定种子下逐条一致的保证与使用方式。
- [架构与数据流](docs/architecture.md)：包结构、单循环状态机、容错顺序。

## 目录结构

| 路径 | 职责 |
| --- | --- |
| `internal/types` | 行情、成交、报价、意图等领域类型 |
| `internal/config` | 配置快照、校验与原子热更新 |
| `internal/gateway` | 本地内存订单网关（幂等挂单/撤单/模拟成交） |
| `internal/ledger` | 持仓账本（成交去重、按成交集合重算） |
| `internal/risk` | 心跳/跳变/价差保护监视器与审计记录 |
| `internal/pricing` | 双边报价生成、库存倾斜、确定性抖动、代号管理 |
| `internal/book` | 报价意图与在挂集合的增量对账 |
| `internal/store` | 原子状态快照与 JSONL 审计日志 |
| `internal/engine` | 单循环状态机，串联全部组件 |
| `internal/replay` | 确定性回放与逐条输出比对 |
| `cmd/mm` | 端到端演示入口 |
