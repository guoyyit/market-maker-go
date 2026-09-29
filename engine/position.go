package engine

import "errors"

// PositionLedger 维护持仓与已处理成交回报集合，保证成交回报幂等。
// 同 FillID 的回报只生效一次；迟到不影响去重判定（FillID 是全局唯一键）。
type PositionLedger struct {
	position int64
	fillIDs  map[string]struct{}
}

// NewPositionLedger 创建空账本。
func NewPositionLedger() *PositionLedger {
	return &PositionLedger{fillIDs: map[string]struct{}{}}
}

// Restore 用持久化的持仓与成交 ID 集合重建账本。
// 若持仓与由成交集合无法核对（成交内容缺失），调用方应另行用 ReplayFills 核对。
func (l *PositionLedger) Restore(position int64, fillIDs map[string]struct{}) {
	l.position = position
	l.fillIDs = make(map[string]struct{}, len(fillIDs))
	for id := range fillIDs {
		l.fillIDs[id] = struct{}{}
	}
}

// ApplyFill 处理一笔成交回报。
// 买方成交增加多头持仓，卖方成交减少（增加空头）持仓。
// 返回持仓变化量 delta 与是否被接受；重复 FillID 返回 (0,false)，持仓不变。
func (l *PositionLedger) ApplyFill(f Fill) (delta int64, accepted bool) {
	if f.FillID == "" {
		return 0, false
	}
	if _, ok := l.fillIDs[f.FillID]; ok {
		return 0, false
	}
	delta = int64(f.Side) * f.Quantity
	l.position += delta
	l.fillIDs[f.FillID] = struct{}{}
	return delta, true
}

// Position 返回当前净持仓。
func (l *PositionLedger) Position() int64 { return l.position }

// FillIDs 返回已处理成交 ID 的快照集合。
func (l *PositionLedger) FillIDs() map[string]struct{} {
	out := make(map[string]struct{}, len(l.fillIDs))
	for id := range l.fillIDs {
		out[id] = struct{}{}
	}
	return out
}

// ReplayFills 用成交回报序列重算持仓，是崩溃恢复时的持仓真相来源。
// 处理顺序即输入顺序；同一 FillID 仅第一次出现生效（重复/迟到均忽略）。
// 返回重算持仓与被接受的 FillID 集合。
func ReplayFills(fills []Fill) (position int64, accepted map[string]struct{}) {
	ledger := NewPositionLedger()
	for _, f := range fills {
		ledger.ApplyFill(f)
	}
	return ledger.Position(), ledger.FillIDs()
}

// ErrPositionMismatch 表示快照持仓与成交回报重算持仓不一致。
var ErrPositionMismatch = errors.New("persisted position does not match fills replay")
