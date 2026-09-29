package engine

import (
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func skewConfig() config.Config {
	cfg := testConfig()
	cfg.SkewCoeff = 1
	cfg.SkewDenom = 10
	return cfg
}

// fillOpenBuy 模拟成交当前在挂的指定买档（按代号匹配），并把回报注入引擎。
func (h *harness) fillOpenBuy(level int, at time.Time) {
	h.t.Helper()
	var id string
	for openID := range h.gw.OpenOrders() {
		_, side, lv, _, err := pricing.ParseClientOrderID(openID)
		if err == nil && side == types.SideBuy && lv == level {
			id = openID
			break
		}
	}
	if id == "" {
		h.t.Fatalf("no open buy level %d to fill", level)
	}
	fill, err := h.gw.SimulateFill(id, at)
	if err != nil {
		h.t.Fatalf("simulate fill: %v", err)
	}
	obs := h.send(Event{Trade: &TradeEvent{Trade: fill}})
	if obs.Skipped {
		h.t.Fatalf("fill %s was skipped", fill.ID)
	}
}

func TestInventorySkewAdjustsQuotes(t *testing.T) {
	h := newHarness(t, skewConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 两次买入成交，每次 10，持仓达到 +20：
	// fairMid = 100 - 1*(20/10) = 98 -> 买0=96 卖0=100。
	h.fillOpenBuy(0, base.Add(2*time.Millisecond))
	h.fillOpenBuy(0, base.Add(4*time.Millisecond))

	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(10*time.Millisecond))}})
	if obs.Position != 20 {
		t.Fatalf("position = %d, want 20", obs.Position)
	}
	var buy0, sell0 int64
	for _, quote := range obs.OpenQuotes {
		if quote.Side == types.SideBuy && quote.Level == 0 {
			buy0 = quote.Price
		}
		if quote.Side == types.SideSell && quote.Level == 0 {
			sell0 = quote.Price
		}
	}
	if buy0 != 96 || sell0 != 100 {
		t.Fatalf("skewed top of book buy=%d sell=%d, want 96/100", buy0, sell0)
	}
}

func TestPositionCapStopsSameDirectionQuotes(t *testing.T) {
	cfg := skewConfig()
	cfg.MaxPosition = 15
	cfg.BaseQty = 15
	h := newHarness(t, cfg)
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	if h.openCount() != 4 {
		t.Fatalf("initial open = %d, want 4", h.openCount())
	}

	// 一档买单成交 15，持仓恰好达上限：停止挂买单，只剩卖侧两档。
	h.fillOpenBuy(0, base.Add(2*time.Millisecond))

	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(10*time.Millisecond))}})
	if obs.Position != 15 {
		t.Fatalf("position = %d, want 15", obs.Position)
	}
	if h.openCount() != 2 {
		t.Fatalf("open at cap = %d, want 2 (sell only)", h.openCount())
	}
	for _, quote := range obs.OpenQuotes {
		if quote.Side != types.SideSell {
			t.Fatalf("found %v quote at +cap, want sell-only", quote.Side)
		}
	}

	// 一卖单成交使持仓回到 0 以下敞口允许范围前，先回落到上限内：
	// 模拟卖出 15，持仓归零，两侧报价必须恢复。
	var sellID string
	for openID := range h.gw.OpenOrders() {
		_, side, level, _, _ := pricing.ParseClientOrderID(openID)
		if side == types.SideSell && level == 0 {
			sellID = openID
		}
	}
	fill, err := h.gw.SimulateFill(sellID, base.Add(12*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	h.send(Event{Trade: &TradeEvent{Trade: fill}})
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 99, 101, base.Add(20*time.Millisecond))}})
	if obs.Position != 0 {
		t.Fatalf("position = %d, want 0", obs.Position)
	}
	if h.openCount() != 4 {
		t.Fatalf("open after unwind = %d, want 4 (two-sided restored)", h.openCount())
	}
}
