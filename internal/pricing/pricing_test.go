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
