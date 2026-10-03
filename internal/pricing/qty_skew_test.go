package pricing

import (
	"testing"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// qtySkewCfg 返回开启数量倾斜的配置：BaseQty=10，每偏离 10 个数量单位，
// 同向一侧每档增减 3 个最小数量单位。
func qtySkewCfg() config.Config {
	cfg := testCfg()
	cfg.BaseQty = 10
	cfg.QtySkewCoeff = 3
	cfg.QtySkewDenom = 10
	return cfg
}

// qtyOf 返回指定侧别在意图中的每档数量（两侧各档数量相同）。
func qtyOf(quotes []types.Quote, side types.Side) (int64, bool) {
	for _, q := range quotes {
		if q.Side == side {
			return q.Qty, true
		}
	}
	return 0, false
}

func TestQtySkewDisabledByDefault(t *testing.T) {
	// 系数为 0（旧配置零值）：两侧数量恒为 BaseQty，不随持仓变化。
	cfg := testCfg()
	cfg.BaseQty = 10
	cfg.QtySkewCoeff = 0
	cfg.MaxPosition = 1000 // 抬高仓位上限，隔离仓位上限对侧别的裁剪
	g := NewGenerator(cfg)

	for _, pos := range []int64{0, 25, -25, 100, -100} {
		quotes := g.Generate(tick100(1), pos)
		buyQty, _ := qtyOf(quotes, types.SideBuy)
		sellQty, _ := qtyOf(quotes, types.SideSell)
		if buyQty != 10 || sellQty != 10 {
			t.Fatalf("pos=%d: buy=%d sell=%d, want 10/10 (skew disabled)",
				pos, buyQty, sellQty)
		}
	}
}

func TestQtySkewAdjustsBothSides(t *testing.T) {
	g := NewGenerator(qtySkewCfg())

	cases := []struct {
		position    int64
		wantBuyQty  int64
		wantSellQty int64
	}{
		{0, 10, 10},    // 目标：两侧基础数量
		{10, 7, 13},    // 高于目标：买减卖增
		{-10, 13, 7},   // 低于目标：买增卖减
		{20, 4, 16},    // 偏离加倍
		{-20, 16, 4},   // 反向加倍
		{30, 1, 19},    // 买侧接近归零
		{-30, 19, 1},   // 卖侧接近归零
	}
	for _, c := range cases {
		quotes := g.Generate(tick100(1), c.position)
		buyQty, okB := qtyOf(quotes, types.SideBuy)
		sellQty, okS := qtyOf(quotes, types.SideSell)
		if !okB || !okS {
			t.Fatalf("pos=%d: missing side in quotes", c.position)
		}
		if buyQty != c.wantBuyQty || sellQty != c.wantSellQty {
			t.Fatalf("pos=%d: buy=%d sell=%d, want %d/%d",
				c.position, buyQty, sellQty, c.wantBuyQty, c.wantSellQty)
		}
	}
}

func TestQtySkewRoundingTowardZero(t *testing.T) {
	// 取整规则：先整除 QtySkewDenom（向 0 截断）再乘以系数，
	// 因此调整幅度恒为 QtySkewCoeff 的整数倍。
	g := NewGenerator(qtySkewCfg())

	cases := []struct {
		position    int64
		wantBuyQty  int64
		wantSellQty int64
	}{
		{3, 10, 10},   // trunc(3/10)=0 -> 不调整
		{9, 10, 10},   // trunc(9/10)=0 -> 不调整
		{10, 7, 13},   // trunc(10/10)=1 -> 调整 3
		{19, 7, 13},   // trunc(19/10)=1 -> 调整 3
		{-3, 10, 10},  // 负偏离对称截断
		{-19, 13, 7},  // trunc(19/10)=1 -> 调整 3
	}
	for _, c := range cases {
		quotes := g.Generate(tick100(1), c.position)
		buyQty, _ := qtyOf(quotes, types.SideBuy)
		sellQty, _ := qtyOf(quotes, types.SideSell)
		if buyQty != c.wantBuyQty || sellQty != c.wantSellQty {
			t.Fatalf("pos=%d: buy=%d sell=%d, want %d/%d",
				c.position, buyQty, sellQty, c.wantBuyQty, c.wantSellQty)
		}
	}
}

func TestQtySkewStopsSideAtZeroAndResumes(t *testing.T) {
	g := NewGenerator(qtySkewCfg())

	// 持仓 +40：买侧数量 = 10 - 3*trunc(40/10) = 10 - 12 = -2 <= 0，
	// 买侧停止挂单，只剩卖侧。
	quotes := g.Generate(tick100(1), 40)
	if len(quotes) != 2 {
		t.Fatalf("quotes at +40 = %d, want 2 (sell-only)", len(quotes))
	}
	for _, q := range quotes {
		if q.Side != types.SideSell {
			t.Fatalf("found %v quote at +40, want sell-only", q.Side)
		}
	}

	// 持仓 -40：卖侧停止，只剩买侧。
	quotes = g.Generate(tick100(2), -40)
	if len(quotes) != 2 {
		t.Fatalf("quotes at -40 = %d, want 2 (buy-only)", len(quotes))
	}
	for _, q := range quotes {
		if q.Side != types.SideBuy {
			t.Fatalf("found %v quote at -40, want buy-only", q.Side)
		}
	}

	// 回落到 +30：买侧数量 = 10 - 3*3 = 1 > 0，自动恢复挂单。
	quotes = g.Generate(tick100(3), 30)
	if len(quotes) != 4 {
		t.Fatalf("quotes at +30 = %d, want 4 (two-sided resumed)", len(quotes))
	}
	buyQty, _ := qtyOf(quotes, types.SideBuy)
	if buyQty != 1 {
		t.Fatalf("buy qty at +30 = %d, want 1", buyQty)
	}
}

func TestQtySkewAndPriceSkewCoexist(t *testing.T) {
	// 数量倾斜与价格倾斜相互独立、同时生效，互不覆盖。
	cfg := qtySkewCfg()
	cfg.SkewCoeff = 1
	cfg.SkewDenom = 10
	g := NewGenerator(cfg)

	// 持仓 +20：价格倾斜 fairMid = 100 - 2 = 98；
	// 数量倾斜：买侧 10 - 3*2 = 4，卖侧 10 + 6 = 16。
	quotes := g.Generate(tick100(1), 20)
	if quotes[0].Price != 96 || quotes[2].Price != 100 {
		t.Fatalf("prices: buy0=%d sell0=%d, want 96/100 (price skew active)",
			quotes[0].Price, quotes[2].Price)
	}
	buyQty, _ := qtyOf(quotes, types.SideBuy)
	sellQty, _ := qtyOf(quotes, types.SideSell)
	if buyQty != 4 || sellQty != 16 {
		t.Fatalf("qty: buy=%d sell=%d, want 4/16 (qty skew active)",
			buyQty, sellQty)
	}
}

func TestQtySkewDoesNotBreakDeterministicJitter(t *testing.T) {
	// 数量倾斜不得破坏固定种子下的确定性抖动序列。
	cfg := qtySkewCfg()
	cfg.JitterTicks = 2
	seqA := pricesAndQtysOf(NewGenerator(cfg), 5, 20)
	seqB := pricesAndQtysOf(NewGenerator(cfg), 5, 20)
	for i := range seqA {
		if seqA[i] != seqB[i] {
			t.Fatalf("not deterministic at frame %d: %v vs %v",
				i, seqA[i], seqB[i])
		}
	}
}

// pricesAndQtysOf 返回固定持仓下连续帧的 (价格, 数量) 序列。
func pricesAndQtysOf(g *Generator, frames int, position int64) [][2]int64 {
	var out [][2]int64
	for seq := uint64(1); seq <= uint64(frames); seq++ {
		quotes := g.Generate(tick100(seq), position)
		out = append(out, [2]int64{quotes[0].Price, quotes[0].Qty})
	}
	return out
}
