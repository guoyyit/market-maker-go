package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// qtySkewEngineCfg 返回开启数量倾斜的引擎配置：
// BaseQty=10，每偏离 10 个数量单位，同向一侧每档增减 3 个单位。
func qtySkewEngineCfg() config.Config {
	cfg := testConfig()
	cfg.BaseQty = 10
	cfg.QtySkewCoeff = 3
	cfg.QtySkewDenom = 10
	cfg.MaxPosition = 1000 // 抬高仓位上限，隔离仓位上限对侧别的裁剪
	return cfg
}

// injectTrade 注入一笔指定方向、数量与档位的成交回报，
// MakerOrderID 使用该档当前代号，返回处理后的观测。
func (h *harness) injectTrade(side types.Side, qty int64, level int, seq uint64, at time.Time) Observation {
	h.t.Helper()
	gen := h.eng.pricer.GenerationOf(side, level)
	makerID := pricing.ClientOrderID(testSymbol, side, level, gen)
	return h.send(Event{Trade: &TradeEvent{Trade: types.Trade{
		ID:           fmt.Sprintf("T-%d", seq),
		Symbol:       testSymbol,
		MakerOrderID: makerID,
		MakerSide:    side,
		Price:        100,
		Qty:          qty,
		Seq:          seq,
		Time:         at,
	}}})
}

// qtyBySide 返回观测中指定侧别的每档数量（两侧各档数量相同）。
func qtyBySide(obs []types.Quote, side types.Side) (int64, bool) {
	for _, q := range obs {
		if q.Side == side {
			return q.Qty, true
		}
	}
	return 0, false
}

func TestQtySkewEngineAdjustsQuotes(t *testing.T) {
	h := newHarness(t, qtySkewEngineCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 持仓 +10：买侧 7、卖侧 13。
	obs := h.injectTrade(types.SideBuy, 10, 0, 1, base.Add(1*time.Millisecond))
	buyQty, _ := qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ := qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 7 || sellQty != 13 {
		t.Fatalf("pos=10: buy=%d sell=%d, want 7/13", buyQty, sellQty)
	}

	// 持仓 +20：买侧 4、卖侧 16。
	obs = h.injectTrade(types.SideBuy, 10, 0, 2, base.Add(2*time.Millisecond))
	buyQty, _ = qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ = qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 4 || sellQty != 16 {
		t.Fatalf("pos=20: buy=%d sell=%d, want 4/16", buyQty, sellQty)
	}

	// 持仓回到 +10（卖出 10）：买侧恢复到 7。
	obs = h.injectTrade(types.SideSell, 10, 0, 3, base.Add(3*time.Millisecond))
	buyQty, _ = qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ = qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 7 || sellQty != 13 {
		t.Fatalf("pos=10 after unwind: buy=%d sell=%d, want 7/13", buyQty, sellQty)
	}
}

func TestQtySkewEngineStopsAndResumes(t *testing.T) {
	h := newHarness(t, qtySkewEngineCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 持仓 +40：买侧数量 <= 0，买侧停止挂单并撤掉已有报价。
	obs := h.injectTrade(types.SideBuy, 40, 0, 1, base.Add(1*time.Millisecond))
	if len(obs.OpenQuotes) != 2 {
		t.Fatalf("open at +40 = %d, want 2 (sell-only)", len(obs.OpenQuotes))
	}
	for _, q := range obs.OpenQuotes {
		if q.Side != types.SideSell {
			t.Fatalf("found %v quote at +40, want sell-only", q.Side)
		}
	}

	// 卖出 40 使持仓归零：两侧自动恢复挂单。
	obs = h.injectTrade(types.SideSell, 40, 0, 2, base.Add(2*time.Millisecond))
	if len(obs.OpenQuotes) != 4 {
		t.Fatalf("open after unwind = %d, want 4 (two-sided resumed)", len(obs.OpenQuotes))
	}
	buyQty, _ := qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ := qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 10 || sellQty != 10 {
		t.Fatalf("pos=0 after unwind: buy=%d sell=%d, want 10/10", buyQty, sellQty)
	}
}

func TestQtySkewEngineHotUpdate(t *testing.T) {
	h := newHarness(t, qtySkewEngineCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 热更新：持仓 +10 后再开启数量倾斜，买侧应变 7、卖侧 13。
	h.injectTrade(types.SideBuy, 10, 0, 1, base.Add(1*time.Millisecond))
	obs := h.send(Event{Config: &ConfigEvent{Config: qtySkewEngineCfg()}})
	buyQty, _ := qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ := qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 7 || sellQty != 13 {
		t.Fatalf("after enable: buy=%d sell=%d, want 7/13", buyQty, sellQty)
	}

	// 热更新调大系数：买侧进一步递减。
	more := qtySkewEngineCfg()
	more.QtySkewCoeff = 4
	obs = h.send(Event{Config: &ConfigEvent{Config: more}})
	buyQty, _ = qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ = qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 6 || sellQty != 14 {
		t.Fatalf("after coeff=4: buy=%d sell=%d, want 6/14", buyQty, sellQty)
	}

	// 热更新关闭数量倾斜（系数 0）：两侧恢复 BaseQty，旧配置无需迁移。
	off := qtySkewEngineCfg()
	off.QtySkewCoeff = 0
	obs = h.send(Event{Config: &ConfigEvent{Config: off}})
	buyQty, _ = qtyBySide(obs.OpenQuotes, types.SideBuy)
	sellQty, _ = qtyBySide(obs.OpenQuotes, types.SideSell)
	if buyQty != 10 || sellQty != 10 {
		t.Fatalf("after disable: buy=%d sell=%d, want 10/10", buyQty, sellQty)
	}
}
