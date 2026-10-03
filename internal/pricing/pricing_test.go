package pricing

import (
	"testing"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func testCfg() config.Config {
	return config.Config{
		Symbol:           "SYM",
		Levels:           2,
		BaseQty:          7,
		HalfSpread:       2,
		LevelStep:        1,
		MaxSpread:        20,
		MaxPosition:      100,
		InventoryTarget:  0,
		SkewCoeff:        1,
		SkewDenom:        10,
		HeartbeatTimeout: 1_000_000_000,
		MaxPriceJump:     50,
		JitterTicks:      0,
		RNGSeed:          42,
	}
}

func tick100(seq uint64) types.Tick {
	return types.Tick{Symbol: "SYM", BidPrice: 99, AskPrice: 101, Seq: seq}
}

func TestSymmetricQuotesAtTarget(t *testing.T) {
	g := NewGenerator(testCfg())
	quotes := g.Generate(tick100(1), 0)
	if len(quotes) != 4 {
		t.Fatalf("quotes = %d, want 4 (2 levels x 2 sides)", len(quotes))
	}
	// 期望顺序：买0 买1 卖0 卖1。
	want := []struct {
		side  types.Side
		level int
		price int64
	}{
		{types.SideBuy, 0, 98},
		{types.SideBuy, 1, 97},
		{types.SideSell, 0, 102},
		{types.SideSell, 1, 103},
	}
	for i, w := range want {
		got := quotes[i]
		if got.Side != w.side || got.Level != w.level || got.Price != w.price {
			t.Fatalf("quote %d = %+v, want side=%v level=%d price=%d",
				i, got, w.side, w.level, w.price)
		}
		if got.Qty != 7 {
			t.Fatalf("quote %d qty = %d, want 7", i, got.Qty)
		}
	}
}

func TestInventorySkewShiftsFairMidDown(t *testing.T) {
	cfg := testCfg()
	g := NewGenerator(cfg)

	// 持仓 +20，目标 0：skew = 1 * (20/10) = 2，fairMid 100 -> 98，
	// 买价 96/95，卖价 100/101。
	quotes := g.Generate(tick100(1), 20)
	if quotes[0].Price != 96 || quotes[2].Price != 100 {
		t.Fatalf("skewed prices: buy0=%d sell0=%d, want 96/100",
			quotes[0].Price, quotes[2].Price)
	}

	// 负持仓对称上移：持仓 -20 -> fairMid 102，买价 100，卖价 104。
	quotes = g.Generate(tick100(2), -20)
	if quotes[0].Price != 100 || quotes[2].Price != 104 {
		t.Fatalf("negative skew prices: buy0=%d sell0=%d, want 100/104",
			quotes[0].Price, quotes[2].Price)
	}
}

func TestPositionCapCutsExposureSide(t *testing.T) {
	cfg := testCfg()
	cfg.MaxPosition = 20
	g := NewGenerator(cfg)

	// 持仓已达上限：停止挂买单，只剩卖侧两档。
	quotes := g.Generate(tick100(1), 20)
	if len(quotes) != 2 {
		t.Fatalf("quotes at +cap = %d, want 2 sell-only", len(quotes))
	}
	for _, quote := range quotes {
		if quote.Side != types.SideSell {
			t.Fatalf("quote side = %v, want only SELL", quote.Side)
		}
	}

	// 持仓达负上限：只剩买侧。
	quotes = g.Generate(tick100(2), -20)
	if len(quotes) != 2 {
		t.Fatalf("quotes at -cap = %d, want 2 buy-only", len(quotes))
	}
	for _, quote := range quotes {
		if quote.Side != types.SideBuy {
			t.Fatalf("quote side = %v, want only BUY", quote.Side)
		}
	}
}

func qtyByLevel(quotes []types.Quote, side types.Side) map[int]int64 {
	out := map[int]int64{}
	for _, q := range quotes {
		if q.Side == side {
			out[q.Level] = q.Qty
		}
	}
	return out
}

func TestQtySkewAdjustsBothSidesSymmetrically(t *testing.T) {
	cfg := testCfg() // BaseQty=7
	cfg.QtySkewCoeff = 100
	g := NewGenerator(cfg)

	// 偏离 +10：adj = 10*100/1000 = 1（整除向 0 截断）。
	// 买侧 7-1=6 递减，卖侧 7+1=8 递增；价格倾斜同时生效（与数量互不覆盖）。
	quotes := g.Generate(tick100(1), 10)
	buys, sells := qtyByLevel(quotes, types.SideBuy), qtyByLevel(quotes, types.SideSell)
	if len(buys) != 2 || len(sells) != 2 {
		t.Fatalf("side quote counts: buy=%d sell=%d, want 2/2", len(buys), len(sells))
	}
	for level := 0; level < 2; level++ {
		if buys[level] != 6 || sells[level] != 8 {
			t.Fatalf("level %d qty buy=%d sell=%d, want 6/8", level, buys[level], sells[level])
		}
	}
	if quotes[0].Price != 97 || quotes[2].Price != 101 {
		t.Fatalf("prices must still skew independently: buy0=%d sell0=%d",
			quotes[0].Price, quotes[2].Price)
	}

	// 偏离 -10：方向相反，买 8 卖 6。
	quotes = g.Generate(tick100(2), -10)
	buys, sells = qtyByLevel(quotes, types.SideBuy), qtyByLevel(quotes, types.SideSell)
	for level := 0; level < 2; level++ {
		if buys[level] != 8 || sells[level] != 6 {
			t.Fatalf("negative deviation level %d: buy=%d sell=%d, want 8/6",
				level, buys[level], sells[level])
		}
	}

	// 整除向 0 截断：偏离 ±4 时 adj=0，两侧仍为基础数量。
	quotes = g.Generate(tick100(3), 4)
	buys, sells = qtyByLevel(quotes, types.SideBuy), qtyByLevel(quotes, types.SideSell)
	for level := 0; level < 2; level++ {
		if buys[level] != 7 || sells[level] != 7 {
			t.Fatalf("truncated deviation level %d: buy=%d sell=%d, want 7/7",
				level, buys[level], sells[level])
		}
	}
}

func TestQtySkewZeroCoeffKeepsBaseQty(t *testing.T) {
	cfg := testCfg()
	cfg.QtySkewCoeff = 0
	g := NewGenerator(cfg)
	quotes := g.Generate(tick100(1), 9999)
	for _, q := range quotes {
		if q.Qty != 7 {
			t.Fatalf("qty = %d, want base 7 with skew disabled", q.Qty)
		}
	}
}

func TestQtySkewNonPositiveSideStopsAndRecovers(t *testing.T) {
	cfg := testCfg() // BaseQty=7，上限足够大，本用例只由数量倾斜触发停挂。
	cfg.MaxPosition = 1_000
	cfg.QtySkewCoeff = 100
	g := NewGenerator(cfg)

	// 偏离 +70：买侧 adj=7 -> 买量 0，买侧停止挂单，只剩卖侧。
	quotes := g.Generate(tick100(1), 70)
	if buys := qtyByLevel(quotes, types.SideBuy); len(buys) != 0 {
		t.Fatalf("buy side must stop at non-positive qty, got %d quotes", len(buys))
	}
	if sells := qtyByLevel(quotes, types.SideSell); len(sells) != 2 {
		t.Fatalf("sell side must remain, got %d quotes", len(sells))
	}

	// 偏离回落：买侧数量恢复为正，自动重新挂出。
	quotes = g.Generate(tick100(2), 10)
	if buys := qtyByLevel(quotes, types.SideBuy); len(buys) != 2 || buys[0] != 6 {
		t.Fatalf("buy side must recover, got %+v", buys)
	}

	// 负偏离下对称：卖侧归零停止，买侧保留。
	quotes = g.Generate(tick100(3), -70)
	if sells := qtyByLevel(quotes, types.SideSell); len(sells) != 0 {
		t.Fatalf("sell side must stop at -deviation, got %d quotes", len(sells))
	}
	if buys := qtyByLevel(quotes, types.SideBuy); len(buys) != 2 {
		t.Fatalf("buy side must remain at -deviation, got %d quotes", len(buys))
	}
}

func TestQtySkewCombinedWithPositionCap(t *testing.T) {
	cfg := testCfg() // BaseQty=7，MaxPosition=20
	cfg.MaxPosition = 20
	cfg.QtySkewCoeff = 100
	g := NewGenerator(cfg)

	// 未达上限（偏离 +10）：数量随偏离调整，两侧都在挂。
	quotes := g.Generate(tick100(1), 10)
	if len(quotes) != 4 {
		t.Fatalf("below cap: quotes=%d, want 4", len(quotes))
	}

	// 达到上限（持仓 20）：即使按系数买量仍为正（7-2=5），
	// 同向敞口一侧（买侧）也必须完全停止，只保留卖侧。
	quotes = g.Generate(tick100(2), 20)
	if len(quotes) != 2 {
		t.Fatalf("at cap: quotes=%d, want 2 sell-only", len(quotes))
	}
	for _, q := range quotes {
		if q.Side != types.SideSell {
			t.Fatalf("at +cap quote side=%v, want SELL", q.Side)
		}
	}
}

func TestQtySkewHotUpdateAppliesWithoutResettingRNG(t *testing.T) {
	cfg := testCfg()
	g := NewGenerator(cfg)
	before := g.Generate(tick100(1), 10)
	if before[0].Qty != 7 {
		t.Fatalf("initial qty = %d, want 7", before[0].Qty)
	}

	// 热更新打开数量倾斜：立即生效。
	cfg.QtySkewCoeff = 100
	g.SetConfig(cfg)
	after := g.Generate(tick100(2), 10)
	if after[0].Qty != 6 || after[2].Qty != 8 {
		t.Fatalf("after hot update qty buy0=%d sell0=%d, want 6/8",
			after[0].Qty, after[2].Qty)
	}

	// 热更新系数归零：恢复旧行为（两侧恒为基础数量）。
	cfg.QtySkewCoeff = 0
	g.SetConfig(cfg)
	again := g.Generate(tick100(3), 10)
	if again[0].Qty != 7 || again[2].Qty != 7 {
		t.Fatalf("after coeff reset qty buy0=%d sell0=%d, want 7/7",
			again[0].Qty, again[2].Qty)
	}
}

func TestGenerationAdvancesAfterFill(t *testing.T) {
	g := NewGenerator(testCfg())
	quotes := g.Generate(tick100(1), 0)
	buy0Before := quotes[0].ClientOrderID

	g.AdvanceFill(types.SideBuy, 0)
	quotes = g.Generate(tick100(2), 0)
	if quotes[0].ClientOrderID == buy0Before {
		t.Fatalf("buy level 0 id must change after fill, both %s", buy0Before)
	}
	if quotes[1].ClientOrderID == quotes[0].ClientOrderID {
		t.Fatal("buy level 1 id must not change when only level 0 filled")
	}

	_, side, level, gen, err := ParseClientOrderID(quotes[0].ClientOrderID)
	if err != nil || side != types.SideBuy || level != 0 || gen != 1 {
		t.Fatalf("parsed = side=%v level=%d gen=%d err=%v", side, level, gen, err)
	}
}

func TestDeterministicJitterSequence(t *testing.T) {
	cfg := testCfg()
	cfg.JitterTicks = 2

	seqA := pricesOf(NewGenerator(cfg), 5)
	seqB := pricesOf(NewGenerator(cfg), 5)
	for i := range seqA {
		if seqA[i] != seqB[i] {
			t.Fatalf("jitter not deterministic at frame %d: %d vs %d", i, seqA[i], seqB[i])
		}
	}

	// 同一 tick 帧重复生成（成交后补挂场景）必须复用抖动，价格不变。
	g := NewGenerator(cfg)
	first := g.Generate(tick100(7), 0)
	second := g.Generate(tick100(7), 0)
	if first[0].Price != second[0].Price {
		t.Fatalf("same tick requote changed price: %d -> %d",
			first[0].Price, second[0].Price)
	}
}

func TestRestoreAlignsJitterAndGenerations(t *testing.T) {
	cfg := testCfg()
	cfg.JitterTicks = 3
	g := NewGenerator(cfg)
	for seq := uint64(1); seq <= 3; seq++ {
		g.Generate(tick100(seq), 0)
	}
	g.AdvanceFill(types.SideSell, 1)
	saved := g.Export()
	want := g.Generate(tick100(4), 0)

	g2 := NewGenerator(cfg)
	g2.Restore(saved)
	got := g2.Generate(tick100(4), 0)
	for i := range want {
		if want[i].ClientOrderID != got[i].ClientOrderID ||
			want[i].Price != got[i].Price {
			t.Fatalf("restored generator diverges at %d:\nwant %+v\ngot  %+v",
				i, want[i], got[i])
		}
	}
}

func pricesOf(g *Generator, frames int) []int64 {
	var out []int64
	for seq := uint64(1); seq <= uint64(frames); seq++ {
		quotes := g.Generate(tick100(seq), 0)
		out = append(out, quotes[0].Price)
	}
	return out
}
