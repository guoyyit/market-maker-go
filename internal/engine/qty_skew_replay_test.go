package engine

import (
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// qtySkewReplayCfg 返回开启数量倾斜的回放配置。
func qtySkewReplayCfg() config.Config {
	cfg := testConfig()
	cfg.BaseQty = 10
	cfg.QtySkewCoeff = 3
	cfg.QtySkewDenom = 10
	cfg.MaxPosition = 1000
	cfg.JitterTicks = 2
	cfg.RNGSeed = 999
	return cfg
}

// runQtySkewScenario 在全新引擎上执行同一数量倾斜场景，返回逐条观测。
func runQtySkewScenario(t *testing.T) []Observation {
	t.Helper()
	h := newHarness(t, qtySkewReplayCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	var obs []Observation
	collect := func(o Observation) { obs = append(obs, o) }

	collect(h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}}))
	collect(h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 100, 102, base.Add(10*time.Millisecond))}}))
	collect(h.injectTrade(types.SideBuy, 25, 0, 1, base.Add(1*time.Millisecond)))
	collect(h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 100, 102, base.Add(20*time.Millisecond))}}))
	collect(h.injectTrade(types.SideSell, 15, 0, 2, base.Add(2*time.Millisecond)))
	collect(h.send(Event{Tick: &TickEvent{Tick: mkTick(4, 101, 103, base.Add(30*time.Millisecond))}}))
	collect(h.send(Event{Config: &ConfigEvent{Config: qtySkewReplayCfg()}}))
	return obs
}

func TestQtySkewDeterministicReplay(t *testing.T) {
	obsA := runQtySkewScenario(t)
	obsB := runQtySkewScenario(t)

	if len(obsA) != len(obsB) {
		t.Fatalf("observation count differs: %d vs %d", len(obsA), len(obsB))
	}
	for i := range obsA {
		if obsA[i].Position != obsB[i].Position ||
			len(obsA[i].OpenQuotes) != len(obsB[i].OpenQuotes) {
			t.Fatalf("step %d differs:\nA %+v\nB %+v", i, obsA[i], obsB[i])
		}
		for j := range obsA[i].OpenQuotes {
			a, b := obsA[i].OpenQuotes[j], obsB[i].OpenQuotes[j]
			if a.ClientOrderID != b.ClientOrderID || a.Price != b.Price || a.Qty != b.Qty {
				t.Fatalf("step %d quote %d differs:\nA %+v\nB %+v", i, j, a, b)
			}
		}
	}

	// 关键语义抽查：持仓 +25 时买侧 2（10-3*2）、卖侧 16（10+3*2）。
	afterBuy := obsA[2]
	buyQty, _ := qtyBySide(afterBuy.OpenQuotes, types.SideBuy)
	sellQty, _ := qtyBySide(afterBuy.OpenQuotes, types.SideSell)
	if afterBuy.Position != 25 || buyQty != 4 || sellQty != 16 {
		t.Fatalf("after buy: pos=%d buy=%d sell=%d, want 25/4/16",
			afterBuy.Position, buyQty, sellQty)
	}
}
