// Package ledger 维护持仓状态，负责成交回报的去重与持仓重算。
//
// 持仓是全部"已接受"成交回报 PositionDelta 的累加：
//   - 买入成交 +Qty，卖出成交 -Qty；
//   - 相同 Trade.ID 的回报只生效一次（重复回报幂等）；
//   - 成交回报允许乱序到达，账本不依赖到达顺序，结果仅由成交集合决定；
//   - 崩溃恢复时按持久化的成交集合重算，持仓与重启前一致。
package ledger

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/guoyyit/market-maker-go/internal/types"
)

// Ledger 是线程安全的持仓账本。
type Ledger struct {
	mu       sync.Mutex
	position int64
	// applied 记录已接受的成交 ID 与其序号，用于去重与审计。
	applied map[string]types.Trade
}

// New 创建空账本。
func New() *Ledger {
	return &Ledger{applied: make(map[string]types.Trade)}
}

// Apply 接受一笔成交回报并更新持仓。
// 返回更新后的持仓以及该回报是否真正生效：
// 重复 ID 返回 (当前持仓, false, nil)，不产生任何副作用。
func (l *Ledger) Apply(tr types.Trade) (position int64, applied bool, err error) {
	if tr.ID == "" {
		return 0, false, errors.New("ledger: trade id must not be empty")
	}
	if tr.Qty <= 0 {
		return 0, false, fmt.Errorf("ledger: trade %s has non-positive qty %d", tr.ID, tr.Qty)
	}
	if tr.MakerSide != types.SideBuy && tr.MakerSide != types.SideSell {
		return 0, false, fmt.Errorf("ledger: trade %s has unknown side", tr.ID)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.applied[tr.ID]; dup {
		// 重复成交回报：幂等忽略，持仓不变。
		return l.position, false, nil
	}
	l.applied[tr.ID] = tr
	l.position += tr.PositionDelta()
	return l.position, true, nil
}

// Position 返回当前持仓。
func (l *Ledger) Position() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.position
}

// Has 报告指定成交 ID 是否已经生效。
func (l *Ledger) Has(tradeID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.applied[tradeID]
	return ok
}

// AppliedCount 返回已接受成交回报的数量。
func (l *Ledger) AppliedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.applied)
}

// Rebuild 清空账本并按给定成交序列依次重放。
// 序列内的重复 ID 只生效一次；返回重算后的持仓。
func (l *Ledger) Rebuild(trades []types.Trade) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.applied = make(map[string]types.Trade, len(trades))
	l.position = 0
	for _, tr := range trades {
		if tr.ID == "" {
			return 0, errors.New("ledger: trade id must not be empty")
		}
		if tr.Qty <= 0 {
			return 0, fmt.Errorf("ledger: trade %s has non-positive qty %d", tr.ID, tr.Qty)
		}
		if tr.MakerSide != types.SideBuy && tr.MakerSide != types.SideSell {
			return 0, fmt.Errorf("ledger: trade %s has unknown side", tr.ID)
		}
		// 同一集合内重复 ID 只生效一次（先到为准，与 Apply 的在线语义一致）。
		if _, dup := l.applied[tr.ID]; dup {
			continue
		}
		l.applied[tr.ID] = tr
		l.position += tr.PositionDelta()
	}
	return l.position, nil
}

// Trades 返回已接受成交的确定性副本（按成交序号升序），供持久化与审计。
func (l *Ledger) Trades() []types.Trade {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]types.Trade, 0, len(l.applied))
	for _, tr := range l.applied {
		out = append(out, tr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].ID < out[j].ID
	})
	return out
}
