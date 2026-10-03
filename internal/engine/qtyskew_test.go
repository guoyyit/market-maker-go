package engine

import (
	"strconv"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func qtySkewCfg() config.Config {
	cfg := testConfig() // Levels=2, BaseQty=10
	cfg.MaxPosition = 1_000
	cfg.SkewCoeff = 0
	cfg.QtySkewCoeff = 100
	return cfg
}

func quoteKey(q types.Quote) string {
	return q.ClientOrderID
}

func openQuotesByKey(obs Observation) map[string]types.Quote {
	out := make(map[string]types.Quote, len(obs.OpenQuotes))
	for _, q := range obs.OpenQuotes {
		out[quoteKey(q)] = q
	}
	return out
}

func levelQuote(obs Observation, side types.Side, level int) (types.Quote, bool) {
	for _, q := range obs.OpenQuotes {
		if q.Side == side && q.Level == level {
			return q, true
		}
	}
	return types.Quote{}, false
}

func TestQtySkewZeroSideCancelsAndRestoresQuotes(t *testing.T) {
	h := newHarness(t, qtySkewCfg())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 连续 10 笔每笔 10 的买入成交（直接以成交回报注入，序号递增），
	// 持仓线性升至 100。持仓 100 时 adj=100*100/1000=10，
	// 买量 10-10=0：买侧必须完全撤掉。
	var lastObs Observation
	for i := 1; i <= 10; i++ {
		buyMaker := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, i-1)
		tr := types.Trade{
			ID:           "B" + strconv.Itoa(i),
			Symbol:       testSymbol,
			MakerOrderID: buyMaker,
			MakerSide:    types.SideBuy,
			Price:        98,
			Qty:          10,
			Seq:          uint64(i),
			Time:         base.Add(time.Duration(2+2*i) * time.Millisecond),
		}
		lastObs = h.send(Event{Trade: &TradeEvent{Trade: tr}})
	}

	if pos := lastObs.Position; pos != 100 {
		t.Fatalf("position = %d, want 100", pos)
	}
	if got := h.openCount(); got != 2 {
		t.Fatalf("open with buy qty zero = %d, want 2 (sell only)", got)
	}

	// 一笔 10 量的卖出回报，持仓回落到 90：买量恢复为正（10-9=1），
	// 买侧自动恢复挂单。
	sellMaker := pricing.ClientOrderID(testSymbol, types.SideSell, 0, 0)
	sellTrade := types.Trade{
		ID:           "S1",
		Symbol:       testSymbol,
		MakerOrderID: sellMaker,
		MakerSide:    types.SideSell,
		Price:        102,
		Qty:          10,
		Seq:          100,
		Time:         base.Add(30 * time.Millisecond),
	}
	obs := h.send(Event{Trade: &TradeEvent{Trade: sellTrade}})
	if obs.Position != 90 {
		t.Fatalf("position = %d, want 90", obs.Position)
	}
	if got := h.openCount(); got != 4 {
		t.Fatalf("open after buy side restored = %d, want 4", got)
	}
	buy0, ok := levelQuote(obs, types.SideBuy, 0)
	if !ok || buy0.Qty != 1 {
		t.Fatalf("restored buy level 0 = %+v ok=%v, want qty 1", buy0, ok)
	}
}

func TestQtySkewHotUpdateTouchesOnlyChangedLevels(t *testing.T) {
	// 初始关闭数量倾斜，便于先确认"系数变化但数量不变时在挂单保持不动"。
	cfg := qtySkewCfg()
	cfg.QtySkewCoeff = 0
	h := newHarness(t, cfg)
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	before := openQuotesByKey(obs)
	if len(before) != 4 {
		t.Fatalf("initial open = %d, want 4", len(before))
	}

	// 持仓 0 时无论系数多少 adj 恒为 0：热更新打开倾斜，数量未变，
	// 任何档位都不应撤改，ID 集合逐单不变。
	hot := qtySkewCfg()
	hot.QtySkewCoeff = 100
	obs = h.send(Event{Config: &ConfigEvent{Config: hot}})
	after := openQuotesByKey(obs)
	if len(after) != 4 {
		t.Fatalf("open after hot update = %d, want 4", len(after))
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			t.Fatalf("unchanged level %s must stay open after hot update", id)
		}
	}

	// 买入成交 10：补挂时按新系数计算，买侧 9、卖侧 11。
	h.fillOpenBuy(0, base.Add(2*time.Millisecond))
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(10*time.Millisecond))}})
	buy0, ok0 := levelQuote(obs, types.SideBuy, 0)
	buy1, ok1 := levelQuote(obs, types.SideBuy, 1)
	sell0, ok2 := levelQuote(obs, types.SideSell, 0)
	if !ok0 || !ok1 || !ok2 {
		t.Fatalf("missing levels after fill: %v %v %v", ok0, ok1, ok2)
	}
	if buy0.Qty != 9 || buy1.Qty != 9 || sell0.Qty != 11 {
		t.Fatalf("skewed qty: buy0=%d buy1=%d sell0=%d, want 9/9/11",
			buy0.Qty, buy1.Qty, sell0.Qty)
	}
	if genOf(buy0.ClientOrderID) != 1 {
		t.Fatalf("filled buy level 0 must use gen 1, got %s", buy0.ClientOrderID)
	}

	// 系数 100 -> 110：偏离 10 下截断结果仍为 adj=1，数量完全不变，
	// 全部在挂 ID 保持不动。
	idsBefore := openQuotesByKey(obs)
	hot2 := qtySkewCfg()
	hot2.QtySkewCoeff = 110
	obs = h.send(Event{Config: &ConfigEvent{Config: hot2}})
	idsAfter := openQuotesByKey(obs)
	if len(idsAfter) != len(idsBefore) {
		t.Fatalf("open count changed on no-op update: %d -> %d",
			len(idsBefore), len(idsAfter))
	}
	for id := range idsBefore {
		if got, ok := idsAfter[id]; !ok || got.Qty != idsBefore[id].Qty {
			t.Fatalf("level %s must stay identical, got %+v ok=%v", id, got, ok)
		}
	}

	// 系数 110 -> 200：adj 变为 2，买 8 卖 10，所有保留档位数量改变，
	// 撤旧代号挂新代号；意图与在挂集合仍逐单一致。
	hot3 := qtySkewCfg()
	hot3.QtySkewCoeff = 200
	obs = h.send(Event{Config: &ConfigEvent{Config: hot3}})
	if h.openCount() != 4 {
		t.Fatalf("open after effective update = %d, want 4", h.openCount())
	}
	for id := range idsBefore {
		if _, ok := openQuotesByKey(obs)[id]; ok {
			t.Fatalf("changed level %s must be replaced with new generation", id)
		}
	}
	for _, q := range obs.OpenQuotes {
		var want int64
		if q.Side == types.SideBuy {
			want = 8
		} else {
			want = 12
		}
		if q.Qty != want {
			t.Fatalf("quote %s qty=%d, want %d", q.ClientOrderID, q.Qty, want)
		}
	}
}

// genOf 仅用于测试断言，解析订单代号中的代号段。
func genOf(id string) int {
	_, _, _, gen, err := pricing.ParseClientOrderID(id)
	if err != nil {
		return -1
	}
	return gen
}
