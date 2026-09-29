package engine

import (
	"context"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func quoteIDSet(eng *Engine, t *testing.T) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for id := range eng.gw.OpenOrders() {
		out[id] = struct{}{}
	}
	return out
}

func equalIDSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// restart 在同一存储目录、同一网关对象上重建引擎并执行恢复，
// 模拟"进程重启但本地网关订单记录仍在"的崩溃恢复。
func (h *harness) restart(t *testing.T) *Engine {
	t.Helper()
	h.cancel()
	<-h.done

	eng, err := New(Options{
		Symbol:   testSymbol,
		Initial:  testConfig(),
		Gateway:  h.gw,
		StoreDir: h.storeDir,
		Now:      func() time.Time { return *h.now },
	})
	if err != nil {
		t.Fatalf("new engine after crash: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := eng.Recover(ctx); err != nil {
		t.Fatalf("recover after crash: %v", err)
	}
	done := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- eng.Run(ctx)
		close(done)
	}()
	h.eng = eng
	h.cancel = cancel
	h.done = done
	h.runErr = runErr
	return eng
}

func TestCrashRecoveryRepostsIntentWithoutDuplicates(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 100, 102, base.Add(10*time.Millisecond))}})
	beforeIDs := quoteIDSet(h.eng, t)
	if h.openCount() != 4 {
		t.Fatalf("open before crash = %d, want 4", h.openCount())
	}

	eng := h.restart(t)

	// 恢复后持仓为 0、在挂集合与崩溃前一致，且没有重复挂单。
	if eng.Snapshot().Position != 0 {
		t.Fatalf("position after recover = %d, want 0", eng.Snapshot().Position)
	}
	if h.openCount() != 4 {
		t.Fatalf("open after recover = %d, want 4", h.openCount())
	}
	afterIDs := quoteIDSet(eng, t)
	if !equalIDSet(beforeIDs, afterIDs) {
		t.Fatalf("order ids differ after recovery:\nbefore %v\nafter  %v",
			beforeIDs, afterIDs)
	}

	// 再发一帧行情，引擎应正常增量更新且不报终态错误。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 101, 103, base.Add(20*time.Millisecond))}})
	if obs.Risk.String() != "NORMAL" || h.openCount() != 4 {
		t.Fatalf("post-recover quoting broken: risk=%v open=%d",
			obs.Risk, h.openCount())
	}
}

func TestCrashRecoveryRecomputesPositionAndRefillsFilledLevel(t *testing.T) {
	h := newHarness(t, skewOrPlainCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	h.fillOpenBuy(0, base.Add(2*time.Millisecond)) // 持仓 +10

	filledID := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 0)
	gen1ID := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 1)

	eng := h.restart(t)

	if pos := eng.Snapshot().Position; pos != 10 {
		t.Fatalf("recomputed position = %d, want 10", pos)
	}
	// 已成交旧代号不得恢复为在挂；补挂使用 GEN=1 新代号。
	if _, exists := h.gw.OpenOrders()[filledID]; exists {
		t.Fatalf("filled order %s must not be restored as open", filledID)
	}
	if _, exists := h.gw.OpenOrders()[gen1ID]; !exists {
		t.Fatalf("refilled order %s missing after recovery", gen1ID)
	}
	if h.openCount() != 4 {
		t.Fatalf("open after recover = %d, want 4", h.openCount())
	}

	// 再次注入相同成交回报（ID 已存在），重启后仍然幂等，不重复计持仓。
	dup := types.Trade{
		ID:           "FILL-1",
		Symbol:       testSymbol,
		MakerOrderID: filledID,
		MakerSide:    types.SideBuy,
		Price:        98,
		Qty:          10,
		Seq:          1,
		Time:         base.Add(2 * time.Millisecond),
	}
	obs := h.send(Event{Trade: &TradeEvent{Trade: dup}})
	if !obs.Skipped || obs.Position != 10 {
		t.Fatalf("dup trade after restart: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}
}

func TestCrashRecoveryInProtectedStateStaysFlat(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	h.send(Event{Timer: &TimerEvent{Now: base.Add(200 * time.Millisecond)}})
	if h.openCount() != 0 {
		t.Fatalf("pre-crash open = %d, want 0", h.openCount())
	}

	eng := h.restart(t)
	if eng.risk.State().String() != "PROTECTED" {
		t.Fatalf("risk after recover = %v, want PROTECTED", eng.risk.State())
	}
	if h.openCount() != 0 {
		t.Fatalf("open in protected recovery = %d, want 0", h.openCount())
	}

	// 审计历史跨重启保留。
	trs, err := eng.store.ReadTransitions()
	if err != nil {
		t.Fatal(err)
	}
	if len(trs) < 1 {
		t.Fatalf("audit history lost after restart: %d entries", len(trs))
	}

	// 正常行情后恢复报价。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(300*time.Millisecond))}})
	if obs.Risk.String() != "NORMAL" || h.openCount() != 4 {
		t.Fatalf("recovery quoting: risk=%v open=%d", obs.Risk, h.openCount())
	}
}

func TestNewFillAfterRecoveryGetsFreshTradeID(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	h.fillOpenBuy(0, base.Add(2*time.Millisecond)) // FILL-1, 持仓 10

	eng := h.restart(t)

	// 恢复后用高于历史水位的行情恢复报价。
	h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(300*time.Millisecond))}})

	// 此时再成交一档：内存网关必须分配全新成交 ID（而非复用 FILL-1），
	// 引擎应接受它，持仓变为 20。
	before := eng.Snapshot().LastTradeSeq
	var buy0 string
	for id := range h.gw.OpenOrders() {
		_, side, level, _, _ := pricing.ParseClientOrderID(id)
		if side == types.SideBuy && level == 0 {
			buy0 = id
		}
	}
	if buy0 == "" {
		t.Fatal("no open buy level 0 after recovery")
	}
	fill, err := h.gw.SimulateFill(buy0, base.Add(302*time.Millisecond))
	if err != nil {
		t.Fatalf("fill after recovery: %v", err)
	}
	if fill.ID == "FILL-1" || fill.Seq <= before {
		t.Fatalf("fill after recovery reused stale identity: id=%s seq=%d (last=%d)",
			fill.ID, fill.Seq, before)
	}

	obs := h.send(Event{Trade: &TradeEvent{Trade: fill}})
	if obs.Skipped {
		t.Fatalf("genuine post-recovery fill wrongly treated as duplicate: %s", fill.ID)
	}
	if obs.Position != 20 {
		t.Fatalf("position = %d, want 20", obs.Position)
	}
}

func skewOrPlainCfg() config.Config { return testConfig() }
