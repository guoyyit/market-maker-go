// Package risk 实现行情异常保护：心跳停更与单笔价格异常跳变。
//
// 保护语义：
//   - Normal 状态下持续更新报价；
//   - 一旦心跳超时或中间价跳变超过阈值，立即转入 Protected 状态，
//     引擎收到该信号后必须全量撤单；
//   - Protected 状态下不得挂新单；只有恢复条件满足（收到正常行情且
//     与上一有效帧的跳变在阈值内）才回到 Normal；
//   - 每次进入/退出保护都产生一条 Transition 审计记录，全程可追溯。
package risk

import (
	"fmt"
	"sync"
	"time"

	"github.com/guoyyit/market-maker-go/internal/types"
)

// State 是风控状态。
type State int

const (
	// Normal 正常报价中。
	Normal State = iota
	// Protected 保护状态：已全量撤单，暂停一切新报价。
	Protected
)

func (s State) String() string {
	switch s {
	case Normal:
		return "NORMAL"
	case Protected:
		return "PROTECTED"
	default:
		return "UNKNOWN"
	}
}

// Reason 标识进入/退出保护的原因。
type Reason string

const (
	ReasonStartup       Reason = "startup"
	ReasonHeartbeat     Reason = "heartbeat_timeout"
	ReasonPriceJump     Reason = "price_jump"
	ReasonInvalidSpread Reason = "invalid_spread"
	ReasonRecovered     Reason = "recovered"
)

// Transition 是一条保护状态流转的审计记录。
type Transition struct {
	From    State  `json:"from"`
	To      State  `json:"to"`
	Reason  Reason `json:"reason"`
	TickSeq uint64 `json:"tick_seq"`
	Time    time.Time
	Detail  string `json:"detail"`
}

// Params 是风控判定参数（由配置快照提供）。
type Params struct {
	HeartbeatTimeout time.Duration
	MaxPriceJump     int64
	MaxSpread        int64
}

// Monitor 是单标的的行情风控监视器。
type Monitor struct {
	mu sync.Mutex

	params Params

	state      State
	lastSeq    uint64
	lastMid    int64
	lastTickAt time.Time
	haveTick   bool

	transitions []Transition
}

// NewMonitor 创建监视器，初始状态为 Normal（尚无行情帧）。
func NewMonitor(params Params) *Monitor {
	return &Monitor{
		params: params,
		state:  Normal,
	}
}

// SetParams 热更新判定阈值，不改变当前状态与行情基准。
func (m *Monitor) SetParams(params Params) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.params = params
}

// OnTick 处理一帧新行情（序号去重/乱序过滤由调用方保证）。
// 返回最新状态；若发生状态流转，同时返回一条 Transition 记录。
//
// 注意：引发跳变/非法价差的那一帧不会成为新的价格基准，
// 防止异常帧把后续正常行情"锚定"成新的跳变。
func (m *Monitor) OnTick(tick types.Tick) (State, *Transition) {
	m.mu.Lock()
	defer m.mu.Unlock()

	mid := tick.Mid()

	// 行情自身非法：价差 <= 0 或超过最大允许价差，进入保护。
	spread := tick.AskPrice - tick.BidPrice
	if spread <= 0 || spread > m.params.MaxSpread {
		tr := m.transitionLocked(Protected, ReasonInvalidSpread, tick.Seq, tick.Time,
			fmt.Sprintf("spread=%d out of (0,%d]", spread, m.params.MaxSpread))
		return Protected, tr
	}

	// 与最近一帧有效行情比较，检测单笔异常跳变。
	if m.haveTick {
		jump := mid - m.lastMid
		if jump < 0 {
			jump = -jump
		}
		if jump > m.params.MaxPriceJump {
			detail := fmt.Sprintf("mid jump %d -> %d (abs=%d) exceeds %d",
				m.lastMid, mid, jump, m.params.MaxPriceJump)
			tr := m.transitionLocked(Protected, ReasonPriceJump, tick.Seq, tick.Time, detail)
			return Protected, tr
		}
	}

	// 正常帧：更新基准；若此前处于保护状态，则满足恢复条件，回到 Normal。
	var tr *Transition
	if m.state == Protected {
		tr = m.transitionLocked(Normal, ReasonRecovered, tick.Seq, tick.Time,
			fmt.Sprintf("normal tick seq=%d mid=%d", tick.Seq, mid))
	}
	m.state = Normal
	m.lastSeq = tick.Seq
	m.lastMid = mid
	m.lastTickAt = tick.Time
	m.haveTick = true
	return m.state, tr
}

// CheckHeartbeat 在定时心跳点检查行情是否停更。
// 发生超时进入保护时返回非空 Transition。
func (m *Monitor) CheckHeartbeat(now time.Time) (State, *Transition) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != Normal || !m.haveTick {
		return m.state, nil
	}
	if now.Sub(m.lastTickAt) <= m.params.HeartbeatTimeout {
		return m.state, nil
	}

	tr := m.transitionLocked(Protected, ReasonHeartbeat, m.lastSeq, now,
		fmt.Sprintf("no tick for %s (last at %s)",
			now.Sub(m.lastTickAt), m.lastTickAt.Format(time.RFC3339Nano)))
	return Protected, tr
}

// State 返回当前风控状态。
func (m *Monitor) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Transitions 返回进入/退出保护的全部审计记录副本（按时间顺序）。
func (m *Monitor) Transitions() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transition, len(m.transitions))
	copy(out, m.transitions)
	return out
}

// transitionLocked 追加一条状态流转记录并切换状态，调用方必须持有 m.mu。
func (m *Monitor) transitionLocked(to State, reason Reason, seq uint64, at time.Time, detail string) *Transition {
	from := m.state
	tr := Transition{
		From:    from,
		To:      to,
		Reason:  reason,
		TickSeq: seq,
		Time:    at,
		Detail:  detail,
	}
	m.transitions = append(m.transitions, tr)
	m.state = to
	return &tr
}

// Snapshot 是可持久化的风控状态。
type Snapshot struct {
	State       State     `json:"state"`
	LastTickSeq uint64    `json:"last_tick_seq"`
	LastMid     int64     `json:"last_mid"`
	LastTickAt  time.Time `json:"last_tick_at"`
	HaveTick    bool      `json:"have_tick"`
}

// Export 返回当前状态快照用于持久化。
func (m *Monitor) Export() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		State:       m.state,
		LastTickSeq: m.lastSeq,
		LastMid:     m.lastMid,
		LastTickAt:  m.lastTickAt,
		HaveTick:    m.haveTick,
	}
}

// Restore 从快照恢复风控状态（重启时调用），审计历史从持久化日志另行加载。
// 恢复后监视器按快照中的最后有效行情作为跳变基准。
func (m *Monitor) Restore(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s.State
	m.lastSeq = s.LastTickSeq
	m.lastMid = s.LastMid
	m.lastTickAt = s.LastTickAt
	m.haveTick = s.HaveTick
}

// LoadTransitions 用持久化的审计记录重建流转历史（不改变当前状态）。
func (m *Monitor) LoadTransitions(ts []Transition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transitions = make([]Transition, len(ts))
	copy(m.transitions, ts)
}
