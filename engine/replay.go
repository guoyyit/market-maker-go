package engine

import "context"

// TraceEntry 是确定性回放中一步输入对应的逐条输出记录。
type TraceEntry struct {
	Index  int
	Env    Envelope
	Result StepResult
	// PositionAfter 是本步处理后的持仓快照，便于逐条核对。
	PositionAfter int64
}

// Replayer 用固定随机种子与相同输入序列逐条重放，
// 用于复现历史场景并校验报价、撤单与持仓变化序列完全一致。
// 重放不做任何文件 IO：使用 MemoryStore 与内存模拟网关。
type Replayer struct {
	cfg Config
	gw  Gateway
}

// NewReplayer 构造回放器。
func NewReplayer(cfg Config) *Replayer {
	return &Replayer{cfg: cfg, gw: NewSimGateway()}
}

// WithGateway 指定回放使用的网关（例如不做自动撮合的桩网关）。
func (r *Replayer) WithGateway(gw Gateway) *Replayer {
	r.gw = gw
	return r
}

// Run 对事件序列做确定性重放，返回逐条轨迹。
func (r *Replayer) Run(ctx context.Context, events []Envelope) ([]TraceEntry, error) {
	engine, err := NewEngine(r.cfg, r.gw, NewMemoryStore())
	if err != nil {
		return nil, err
	}
	defer engine.Close()

	trace := make([]TraceEntry, 0, len(events))
	for i, env := range events {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		result := engine.ProcessOne(env)
		trace = append(trace, TraceEntry{
			Index:         i,
			Env:           env,
			Result:        result,
			PositionAfter: engine.Kernel().Position(),
		})
	}
	return trace, nil
}

// CompareTraces 比较两次回放轨迹是否逐条一致（含动作、审计、成交、持仓）。
// 不一致时返回首个不一致位置（从 0 起）。
func CompareTraces(a, b []TraceEntry) (mismatchAt int, equal bool) {
	if len(a) != len(b) {
		index := len(a)
		if len(b) < index {
			index = len(b)
		}
		return index, false
	}
	for i := range a {
		if !traceEntryEqual(a[i], b[i]) {
			return i, false
		}
	}
	return 0, true
}

func traceEntryEqual(a, b TraceEntry) bool {
	if a.Index != b.Index || a.PositionAfter != b.PositionAfter {
		return false
	}
	if a.Result.Accepted != b.Result.Accepted ||
		a.Result.IgnoredReason != b.Result.IgnoredReason ||
		len(a.Result.Actions) != len(b.Result.Actions) ||
		len(a.Result.Audits) != len(b.Result.Audits) ||
		len(a.Result.Fills) != len(b.Result.Fills) ||
		len(a.Result.SubSteps) != len(b.Result.SubSteps) {
		return false
	}
	for i := range a.Result.Actions {
		if !actionEqual(a.Result.Actions[i], b.Result.Actions[i]) {
			return false
		}
	}
	for i := range a.Result.Audits {
		if a.Result.Audits[i] != b.Result.Audits[i] {
			return false
		}
	}
	for i := range a.Result.Fills {
		if a.Result.Fills[i] != b.Result.Fills[i] {
			return false
		}
	}
	return true
}

func actionEqual(a, b Action) bool {
	switch av := a.(type) {
	case PlaceAction:
		bv, ok := b.(PlaceAction)
		return ok && av.Order == bv.Order
	case CancelAction:
		bv, ok := b.(CancelAction)
		return ok && av.ClientID == bv.ClientID
	default:
		return false
	}
}
