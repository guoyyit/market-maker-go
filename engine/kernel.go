package engine

import (
	"fmt"
	"time"
)

// Kernel 是纯确定性的状态机内核：单线程逐步消费输入事件，
// 维护持仓、行情、保护状态与报价意图，并输出动作与审计事件。
// 内核不做 IO、不读墙钟；相同初始状态与事件序列必然产生相同结果。
type Kernel struct {
	cfg        Config
	ledger     *PositionLedger
	risk       *RiskMonitor
	quoter     *Quoter
	reconciler *IntentReconciler
	rng        *RNG
	audit      *AuditLog

	lastTick  *Tick
	lastMid   int64
	haveMid   bool
	intent    *Intent
	intentSeq int64
	now       time.Time
}

// NewKernel 从配置构造空内核。
func NewKernel(cfg Config) *Kernel {
	cfg, _ = Validate(cfg)
	k := &Kernel{
		cfg:        cfg,
		ledger:     NewPositionLedger(),
		risk:       NewRiskMonitor(cfg),
		quoter:     NewQuoter(cfg),
		reconciler: NewIntentReconciler(),
		rng:        NewRNG(cfg.RandomSeed),
		audit:      NewAuditLog(),
	}
	return k
}

// RestoreKernel 从快照恢复内核，并用成交回报序列重算持仓核对。
// 核对不一致时返回 ErrPositionMismatch，调用方不得带错继续报价。
func RestoreKernel(snap Snapshot, fills []Fill) (*Kernel, error) {
	cfg, err := Validate(snap.Config)
	if err != nil {
		return nil, err
	}
	replayed, accepted := ReplayFills(fills)
	if replayed != snap.Position {
		return nil, fmt.Errorf("%w: snapshot=%d replayed=%d",
			ErrPositionMismatch, snap.Position, replayed)
	}

	k := NewKernel(cfg)
	k.ledger.Restore(snap.Position, accepted)
	var mid int64
	haveMid := false
	if snap.LastTick != nil {
		mid = MidPrice(snap.LastTick)
		haveMid = true
	}
	k.risk.Restore(snap.Protection, snap.LastTick, mid, haveMid)
	if mid, ok := k.risk.LastMid(); ok {
		k.lastMid, k.haveMid = mid, true
	}
	k.lastTick = k.risk.LastTick()
	k.intentSeq = snap.IntentSeq
	if snap.Intent != nil {
		intentCopy := *snap.Intent
		k.intent = &intentCopy
		// 从最近意图重建在挂集合：不会产生任何动作，因为网关将按同一意图恢复。
		k.reconciler.Reconcile(k.intent)
	}
	k.rng.RestoreState(snap.RNGState)
	if k.lastTick != nil {
		k.now = k.lastTick.Time
	}
	return k, nil
}

// Step 处理一步输入事件，返回确定性结果。
func (k *Kernel) Step(env Envelope) StepResult {
	if !env.WallTime.IsZero() {
		k.now = env.WallTime
	}
	switch {
	case env.Tick != nil:
		return k.stepTick(*env.Tick)
	case env.Fill != nil:
		return k.stepFill(*env.Fill)
	case env.Update != nil:
		return k.stepUpdate(*env.Update)
	case env.Timer:
		return k.stepTimer()
	default:
		return StepResult{Accepted: false, IgnoredReason: "empty event"}
	}
}

func (k *Kernel) stepTick(tick Tick) StepResult {
	accepted, changed, audits := k.risk.ObserveTick(tick)
	result := StepResult{Accepted: true, Audits: audits}
	k.recordAudits(audits)

	// 被风控判为乱序/重复而忽略的行情，不更新任何报价状态。
	if !accepted {
		return StepResult{Accepted: false, IgnoredReason: "stale tick", Audits: audits}
	}

	tickCopy := tick
	k.lastTick = &tickCopy
	if mid, ok := k.risk.LastMid(); ok {
		k.lastMid, k.haveMid = mid, true
	}

	protection := k.risk.Protection()
	if protection.Active {
		if changed {
			// enterProtection 才会返回 changed；保护期间即便尚未满足恢复条件，
			// 也必须保证在挂集合为空。
			result.Actions = append(result.Actions, k.enterProtected(tick.Time, protection.Reason)...)
		} else {
			result.Actions = append(result.Actions, k.ensureFlatWhileProtected()...)
		}
		return result
	}

	if changed {
		// 风险监控刚从保护中退出：立即重新报价。
		return k.requote(&result, "protection_recovered")
	}
	return k.requote(&result, "tick")
}

func (k *Kernel) stepFill(fill Fill) StepResult {
	delta, accepted := k.ledger.ApplyFill(fill)
	if !accepted {
		ev := AuditEvent{Time: fill.Time, Kind: AuditFillIgnored,
			Detail: fmt.Sprintf("duplicate or invalid fill id=%q", fill.FillID)}
		k.audit.Append(ev)
		return StepResult{Accepted: false, IgnoredReason: "duplicate fill", Audits: []AuditEvent{ev}}
	}

	result := StepResult{
		Accepted: true,
		Fills:    []Fill{fill},
		Audits: []AuditEvent{{
			Time: fill.Time,
			Kind: "fill_applied",
			Detail: fmt.Sprintf("fill id=%s side=%s qty=%d delta=%d position=%d",
				fill.FillID, fill.Side, fill.Quantity, delta, k.ledger.Position()),
		}},
	}
	k.recordAudits(result.Audits)

	// 成交后按新持仓立即重报（库存倾斜与仓位上限随之生效）。
	if k.risk.Protection().Active {
		result.Actions = append(result.Actions, k.ensureFlatWhileProtected()...)
	} else if k.lastTick != nil {
		k.requote(&result, "fill")
	}
	return result
}

func (k *Kernel) stepTimer() StepResult {
	changed, audits := k.risk.ObserveTime(k.now)
	result := StepResult{Accepted: true, Audits: audits}
	k.recordAudits(audits)
	if changed {
		result.Actions = append(result.Actions, k.enterProtected(k.now, k.risk.Protection().Reason)...)
	} else if k.risk.Protection().Active {
		result.Actions = append(result.Actions, k.ensureFlatWhileProtected()...)
	}
	return result
}

func (k *Kernel) stepUpdate(u ConfigUpdate) StepResult {
	next := k.cfg.Apply(u)
	validated, err := Validate(next)
	if err != nil {
		ev := AuditEvent{Time: k.now, Kind: AuditConfigUpdated,
			Detail: "rejected: " + err.Error()}
		k.audit.Append(ev)
		return StepResult{Accepted: false, IgnoredReason: err.Error(), Audits: []AuditEvent{ev}}
	}
	k.cfg = validated
	k.quoter.UpdateConfig(validated)
	k.risk.UpdateConfig(validated)
	// RandomSeed 变更不重置已持久化的 RNG 状态：热更新只影响后续抖动强度开关，
	// 以保证同输入序列下更新后的轨迹仍然确定。
	ev := AuditEvent{Time: k.now, Kind: AuditConfigUpdated, Detail: "applied"}
	k.audit.Append(ev)

	result := StepResult{Accepted: true, Audits: []AuditEvent{ev}}
	if k.risk.Protection().Active {
		result.Actions = append(result.Actions, k.ensureFlatWhileProtected()...)
	} else if k.lastTick != nil {
		k.requote(&result, "config_update")
	}
	return result
}

// enterProtected 递增意图版本、提交空意图并全量撤单。
func (k *Kernel) enterProtected(at time.Time, reason string) []Action {
	k.intentSeq++
	k.intent = &Intent{Seq: k.intentSeq, Symbol: k.cfg.Symbol, Reason: "protection:" + reason, CreatedAt: at}
	return k.reconciler.CancelAll()
}

// ensureFlatWhileProtected 保证保护期间在挂集合始终为空（重复对账返回空动作）。
func (k *Kernel) ensureFlatWhileProtected() []Action {
	if len(k.reconciler.Live()) == 0 {
		return nil
	}
	return k.enterProtected(k.now, k.risk.Protection().Reason)
}

// requote 生成下一版本意图并与在挂集合增量对账，动作写入 result。
func (k *Kernel) requote(result *StepResult, reason string) StepResult {
	if !k.haveMid || k.lastTick == nil {
		return *result
	}
	k.intentSeq++
	intent := k.quoter.Generate(k.lastMid, k.lastTick, k.ledger.Position(), k.intentSeq, k.rng)
	intent.Reason = reason
	if intent.Quotes == nil {
		// 价差过宽等情形：意图为空，全部撤单。
		intent.Quotes = []Quote{}
	}
	k.intent = intent
	result.Actions = append(result.Actions, k.reconciler.Reconcile(intent)...)
	result.Intent = intent
	return *result
}

func (k *Kernel) recordAudits(events []AuditEvent) {
	for _, ev := range events {
		k.audit.Append(ev)
	}
}

// UpdateConfig 直接应用一次热更新（等价于向 Step 投递配置事件）。
func (k *Kernel) UpdateConfig(u ConfigUpdate) []AuditEvent {
	return k.stepUpdate(u).Audits
}

// Snapshot 导出可持久化快照。
func (k *Kernel) Snapshot() Snapshot {
	fillIDs := k.ledger.FillIDs()
	ids := make([]string, 0, len(fillIDs))
	for id := range fillIDs {
		ids = append(ids, id)
	}
	var intentCopy *Intent
	if k.intent != nil {
		intent := *k.intent
		intentCopy = &intent
	}
	return Snapshot{
		SavedAt:          k.now,
		Config:           k.cfg,
		Position:         k.ledger.Position(),
		LastTick:         k.lastTick,
		Intent:           intentCopy,
		Protection:       k.risk.Protection(),
		IntentSeq:        k.intentSeq,
		ProcessedFillIDs: ids,
		RNGState:         k.rng.State(),
	}
}

// Position 返回当前净持仓。
func (k *Kernel) Position() int64 { return k.ledger.Position() }

// CurrentIntent 返回最近一次提交的报价意图。
func (k *Kernel) CurrentIntent() *Intent { return k.intent }

// Protection 返回当前保护状态。
func (k *Kernel) Protection() Protection { return k.risk.Protection() }

// Config 返回当前生效配置。
func (k *Kernel) Config() Config { return k.cfg }

// LiveOrders 返回对账器视角的在挂订单（与网关应严格一致）。
func (k *Kernel) LiveOrders() map[string]Order { return k.reconciler.Live() }

// AuditEvents 返回全部审计事件副本。
func (k *Kernel) AuditEvents() []AuditEvent { return k.audit.Events() }
