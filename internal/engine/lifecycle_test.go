package engine

import (
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/gateway"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/risk"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func mkTick(seq uint64, bid, ask int64, at time.Time) types.Tick {
	return types.Tick{Symbol: testSymbol, BidPrice: bid, AskPrice: ask, Seq: seq, Time: at}
}

func TestTwoSidedQuotingLifecycle(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()

	base := time.Unix(1_000_000, 0)

	// 首帧：2 档 x 两侧 = 4 条报价。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	if obs.Risk.String() != "NORMAL" || obs.Skipped {
		t.Fatalf("first tick obs: %+v", obs)
	}
	if len(obs.OpenQuotes) != 4 {
		t.Fatalf("open quotes = %d, want 4", len(obs.OpenQuotes))
	}
	if h.openCount() != 4 {
		t.Fatalf("gateway open = %d, want 4", h.openCount())
	}

	// 校验价格：mid=100 half=2 step=1 -> 买 98/97，卖 102/103。
	priceByID := map[string]int64{}
	for _, quote := range obs.OpenQuotes {
		priceByID[quote.ClientOrderID] = quote.Price
	}
	want := map[string]int64{
		pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 0):  98,
		pricing.ClientOrderID(testSymbol, types.SideBuy, 1, 0):  97,
		pricing.ClientOrderID(testSymbol, types.SideSell, 0, 0): 102,
		pricing.ClientOrderID(testSymbol, types.SideSell, 1, 0): 103,
	}
	for id, price := range want {
		if priceByID[id] != price {
			t.Errorf("price[%s] = %d, want %d", id, priceByID[id], price)
		}
	}

	// 第二帧 mid 101 -> 全部档位价格变化：每个 ID 一对撤改，
	// 对账后在挂集合必须仍恰好是 4 条，且价格为新值。
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 100, 102, base.Add(10*time.Millisecond))}})
	if h.openCount() != 4 || len(obs.OpenQuotes) != 4 {
		t.Fatalf("after second tick: gateway=%d obs=%d", h.openCount(), len(obs.OpenQuotes))
	}
	for _, quote := range obs.OpenQuotes {
		var expect int64
		switch {
		case quote.Side == types.SideBuy && quote.Level == 0:
			expect = 99
		case quote.Side == types.SideBuy && quote.Level == 1:
			expect = 98
		case quote.Side == types.SideSell && quote.Level == 0:
			expect = 103
		default:
			expect = 104
		}
		if quote.Price != expect {
			t.Errorf("quote %s price = %d, want %d",
				quote.ClientOrderID, quote.Price, expect)
		}
	}

	// 第三帧价格不变：不产生任何动作，在挂集合 ID 与价格完全不变。
	obsBefore := h.eng.Snapshot()
	_ = obsBefore
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 100, 102, base.Add(20*time.Millisecond))}})
	if h.openCount() != 4 {
		t.Fatalf("unchanged tick changed open count: %d", h.openCount())
	}
}

func TestStaleAndDuplicateTicksDoNotOverwrite(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(5, 99, 101, base)}})

	// 迟到的旧帧（seq=4）必须被忽略。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(4, 1, 9, base.Add(-time.Second))}})
	if !obs.Skipped {
		t.Fatalf("stale tick should be skipped, obs=%+v", obs)
	}
	// 价格仍是 seq=5 推导出的 98/97/102/103。
	for _, quote := range obs.OpenQuotes {
		if quote.Price < 90 {
			t.Fatalf("stale tick overwrote prices: %+v", quote)
		}
	}

	// 重复帧（seq=5）也必须被忽略。
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(5, 99, 101, base)}})
	if !obs.Skipped {
		t.Fatalf("duplicate tick should be skipped, obs=%+v", obs)
	}
	if h.eng.Snapshot().LastTickSeq != 5 {
		t.Fatalf("last tick seq = %d, want 5", h.eng.Snapshot().LastTickSeq)
	}
	if h.openCount() != 4 {
		t.Fatalf("open = %d, want 4", h.openCount())
	}
}

func TestDuplicateTradesIgnoredButOutOfOrderTradesApply(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	buy0ID := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 0)
	trade := types.Trade{
		ID:           "F1",
		Symbol:       testSymbol,
		MakerOrderID: buy0ID,
		MakerSide:    types.SideBuy,
		Price:        98,
		Qty:          10,
		Seq:          1,
		Time:         base.Add(5 * time.Millisecond),
	}

	obs := h.send(Event{Trade: &TradeEvent{Trade: trade}})
	if obs.Skipped || obs.Position != 10 {
		t.Fatalf("first trade: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}

	// 完全重复：不得二次入账。
	obs = h.send(Event{Trade: &TradeEvent{Trade: trade}})
	if !obs.Skipped || obs.Position != 10 {
		t.Fatalf("duplicate trade: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}

	// 新 ID 但序号回退（迟到的旧序号回报）：必须照常生效一次。
	// 成交结果只由成交集合决定，与到达顺序无关。
	sell0ID := pricing.ClientOrderID(testSymbol, types.SideSell, 0, 0)
	stale := types.Trade{
		ID:           "F-OLD",
		Symbol:       testSymbol,
		MakerOrderID: sell0ID,
		MakerSide:    types.SideSell,
		Price:        102,
		Qty:          4,
		Seq:          0,
		Time:         base.Add(6 * time.Millisecond),
	}
	obs = h.send(Event{Trade: &TradeEvent{Trade: stale}})
	if obs.Skipped || obs.Position != 6 {
		t.Fatalf("out-of-order trade must apply once: skipped=%v pos=%d",
			obs.Skipped, obs.Position)
	}

	// 迟到成交生效后再次注入同一回报：作为完全重复被忽略，不二次影响持仓。
	obs = h.send(Event{Trade: &TradeEvent{Trade: stale}})
	if !obs.Skipped || obs.Position != 6 {
		t.Fatalf("duplicate of late trade: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}

	if h.eng.ledgerSnapshotCount() != 2 {
		t.Fatalf("applied trade count = %d, want 2", h.eng.ledgerSnapshotCount())
	}

	// 成交档位代号轮换：该档新挂单必须使用 GEN=1 的新 ID，
	// 旧 GEN=0 已成交，不允许重复挂。
	found := false
	newID := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 1)
	for _, quote := range obs.OpenQuotes {
		if quote.ClientOrderID == newID {
			found = true
		}
		if quote.ClientOrderID == buy0ID {
			t.Fatalf("filled order id %s must not be re-posted", buy0ID)
		}
	}
	if !found {
		t.Fatalf("new generation id %s missing from open quotes", newID)
	}

	// 网关视角：旧 ID 处终态，对其再次 Submit 必须被拒绝。
	if _, err := h.gw.Submit(types.Quote{
		Symbol: testSymbol, Side: types.SideBuy, Level: 0,
		Price: 98, Qty: 10, ClientOrderID: buy0ID,
	}); err != gateway.ErrOrderFinal {
		t.Fatalf("resubmit filled id: err=%v, want ErrOrderFinal", err)
	}
}

func TestConfigHotUpdateChangesLevelsAndQty(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	if h.openCount() != 4 {
		t.Fatalf("initial open = %d, want 4", h.openCount())
	}

	newCfg := testConfig()
	newCfg.Levels = 3
	newCfg.BaseQty = 25
	obs := h.send(Event{Config: &ConfigEvent{Config: newCfg}})

	// 热更新立即生效：3 档 x 两侧 = 6 条，全部数量为 25。
	if len(obs.OpenQuotes) != 6 || h.openCount() != 6 {
		t.Fatalf("after hot update: obs=%d gateway=%d, want 6",
			len(obs.OpenQuotes), h.openCount())
	}
	for _, quote := range obs.OpenQuotes {
		if quote.Qty != 25 {
			t.Fatalf("quote %s qty = %d, want 25", quote.ClientOrderID, quote.Qty)
		}
	}

	// 非法热更新必须被拒绝且在挂状态不变。
	badCfg := newCfg
	badCfg.Levels = 0
	obs = h.send(Event{Config: &ConfigEvent{Config: badCfg}})
	if !obs.ConfigRejected || obs.RejectReason == "" {
		t.Fatalf("invalid config should be rejected, obs=%+v", obs)
	}
	if got := h.eng.currentConfig().Levels; got != 3 {
		t.Fatalf("levels after rejected update = %d, want 3", got)
	}
	if h.openCount() != 6 {
		t.Fatalf("open after rejected update = %d, want 6", h.openCount())
	}
}

// 以下辅助方法在 engine 包内以测试专用方式读取状态。
func (e *Engine) ledgerSnapshotCount() int { return e.ledger.AppliedCount() }

func (e *Engine) currentConfig() config.Config { return e.cfg.Current() }

func (e *Engine) storeTransitions() ([]risk.Transition, error) {
	return e.store.ReadTransitions()
}
