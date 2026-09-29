package types

import "time"

// Side 表示报价或成交的方向。
type Side int8

const (
	// SideBuy 买入侧（挂买单；买入成交使持仓增加）。
	SideBuy Side = 1
	// SideSell 卖出侧（挂卖单；卖出成交使持仓减少）。
	SideSell Side = -1
)

// String 返回方向的可读名称。
func (s Side) String() string {
	switch s {
	case SideBuy:
		return "BUY"
	case SideSell:
		return "SELL"
	default:
		return "UNKNOWN"
	}
}

// Tick 是一帧行情快照，价格与数量均为整数单位（最小价位/最小数量），
// 避免浮点带来的不确定性。Seq 为单调递增序号，用于乱序/重复容错。
type Tick struct {
	Symbol   string
	BidPrice int64
	AskPrice int64
	Seq      uint64
	Time     time.Time
}

// Mid 返回中间价（买一卖一取整均值，向下取整）。
func (t Tick) Mid() int64 {
	return (t.BidPrice + t.AskPrice) / 2
}

// Trade 是一笔本方挂单被成交的回报。
// MakerSide 为被成交挂单的方向：买入成交使持仓 +Qty，卖出成交使持仓 -Qty。
type Trade struct {
	ID     string
	Symbol string
	// MakerOrderID 是被成交挂单的 ClientOrderID，用于崩溃恢复核对
	// 与同档位订单在成交后的代号（generation）轮换。
	MakerOrderID string
	MakerSide    Side
	Price        int64
	Qty          int64
	Seq          uint64
	Time         time.Time
}

// PositionDelta 返回该成交对持仓造成的变化量。
func (tr Trade) PositionDelta() int64 {
	return int64(tr.MakerSide) * tr.Qty
}

// Quote 是一条期望挂在网关的报价。
// ClientOrderID 由 标的+方向+档位 确定性生成，用于幂等去重与崩溃恢复。
type Quote struct {
	Symbol        string
	Side          Side
	Level         int
	Price         int64
	Qty           int64
	ClientOrderID string
}

// Intent 是最近一次提交的报价意图（全部期望在挂的报价集合）。
type Intent struct {
	Symbol string
	Quotes []Quote
	Seq    uint64
	Time   time.Time
}

// EventKind 标识注入引擎的输入事件类型。
type EventKind int

const (
	EventTick EventKind = iota + 1
	EventTrade
	EventConfig
	EventTimer
)

// Event 是回放/实时统一的输入信封，具体负载由 Kind 决定。
// 具体负载类型在 engine 包实现阶段再补充定义。
type Event struct {
	Kind EventKind
	Time time.Time
}
