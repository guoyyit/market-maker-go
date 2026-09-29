package engine

import "time"

// Side 表示本方报价方向，买方向记为 +1，卖方向记为 -1。
type Side int

const (
	SideBuy  Side = +1
	SideSell Side = -1
)

// Opp 返回相反方向。
func (s Side) Opp() Side { return -s }

func (s Side) String() string {
	switch s {
	case SideBuy:
		return "buy"
	case SideSell:
		return "sell"
	default:
		return "unknown"
	}
}

// Tick 是一笔行情快照。价格与数量均为整数单位：
// 价格以最小变动价位（tick size）的整数倍表示，数量以最小交易单位表示。
// Seq 为行情源单调递增序号，用于乱序与重复检测；0 表示行情源不提供序号。
type Tick struct {
	Symbol    string
	Seq       int64
	Time      time.Time
	BidPrice  int64
	AskPrice  int64
	LastPrice int64
}

// Fill 是成交回报。Side 为本方订单方向，FillID 为成交回报的全局唯一去重键。
type Fill struct {
	FillID   string
	OrderID  string
	Symbol   string
	Side     Side
	Price    int64
	Quantity int64
	Time     time.Time
}

// Quote 是报价意图中的一档报价。
type Quote struct {
	Side     Side
	Level    int
	Price    int64
	Quantity int64
}

// Intent 是某一时刻期望挂出的全部报价集合（报价意图）。
// Seq 为意图版本号，空 Quotes 表示当前不应有任何在挂订单（如保护状态）。
type Intent struct {
	Seq       int64
	Symbol    string
	Quotes    []Quote
	Reason    string
	CreatedAt time.Time
}

// Order 是已提交到（模拟）网关的订单，ClientID 为幂等键。
type Order struct {
	ClientID  string
	Symbol    string
	Side      Side
	Price     int64
	Quantity  int64
	Level     int
	IntentSeq int64
}

// Action 是内核处理一步输入后期望网关执行的原子动作。
type Action interface{ isAction() }

// PlaceAction 提交新订单。
type PlaceAction struct{ Order Order }

// CancelAction 撤销指定 ClientID 的订单。
type CancelAction struct{ ClientID string }

func (PlaceAction) isAction()  {}
func (CancelAction) isAction() {}

// AuditEvent 是可审计事件（保护进出、重复/乱序丢弃、配置更新等）。
type AuditEvent struct {
	Time   time.Time
	Kind   string
	Detail string
}

// Protection 描述行情保护状态。
type Protection struct {
	Active        bool
	EnteredAt     time.Time
	Reason        string
	RecoveryTicks int
	LastGoodSeq   int64
}

// Snapshot 是崩溃恢复所需的持久化快照。
type Snapshot struct {
	SavedAt          time.Time
	Config           Config
	Position         int64
	LastTick         *Tick
	Intent           *Intent
	Protection       Protection
	IntentSeq        int64
	ProcessedFillIDs []string
	RNGState         uint64
}

// StepResult 是内核处理一步输入的确定性结果。
type StepResult struct {
	Accepted      bool
	IgnoredReason string
	Actions       []Action
	Audits        []AuditEvent
	Fills         []Fill
	Intent        *Intent
	// SubSteps 记录行情驱动的模拟撮合所产生的内部成交处理步骤（确定性回放轨迹用）。
	SubSteps []StepResult
}
