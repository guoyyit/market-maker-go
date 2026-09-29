package engine

import (
	"testing"
	"time"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Symbol = "TEST"
	cfg.Levels = 2
	cfg.BaseQuantity = 10
	cfg.QuoteOffset = 2
	cfg.TickStep = 1
	cfg.MaxSpread = 100
	cfg.MaxPosition = 100
	cfg.SkewPerUnit = 1
	cfg.QuantitySkewPctPerUnit = 0
	cfg.RandomSeed = 0
	cfg.MaxJumpTicks = 0
	cfg.RecoveryTicks = 1
	cfg.StaleTimeout = 0
	return cfg
}

func mkTick(seq int64, bid, ask int64, offsetSec int) Tick {
	return Tick{
		Symbol:   "TEST",
		Seq:      seq,
		Time:     time.Unix(int64(1000+offsetSec), 0),
		BidPrice: bid,
		AskPrice: ask,
	}
}

func TestQuoterTwoSidedLevelsAndSkew(t *testing.T) {
	cfg := testConfig()
	q := NewQuoter(cfg)
	rng := NewRNG(0)

	tick := mkTick(1, 99, 101, 0)
	intent := q.Generate(100, &tick, 0, 1, rng)
	if len(intent.Quotes) != 4 {
		t.Fatalf("want 4 quotes (2 levels x 2 sides), got %d: %+v", len(intent.Quotes), intent.Quotes)
	}

	var buy, sell *Quote
	for i := range intent.Quotes {
		quote := intent.Quotes[i]
		if quote.Level == 1 && quote.Side == SideBuy {
			buy = &intent.Quotes[i]
		}
		if quote.Level == 1 && quote.Side == SideSell {
			sell = &intent.Quotes[i]
		}
	}
	if buy == nil || sell == nil {
		t.Fatal("missing level-1 quotes")
	}
	if buy.Price != 98 || sell.Price != 102 {
		t.Fatalf("want buy=98 sell=102, got buy=%d sell=%d", buy.Price, sell.Price)
	}

	// 多头持仓 10：中间价整体下压 10 个价位。
	intentSkewed := q.Generate(100, &tick, 10, 2, rng)
	var buy2, sell2 *Quote
	for i := range intentSkewed.Quotes {
		quote := intentSkewed.Quotes[i]
		if quote.Level == 1 && quote.Side == SideBuy {
			buy2 = &intentSkewed.Quotes[i]
		}
		if quote.Level == 1 && quote.Side == SideSell {
			sell2 = &intentSkewed.Quotes[i]
		}
	}
	if buy2.Price != 88 || sell2.Price != 92 {
		t.Fatalf("skewed: want buy=88 sell=92, got buy=%d sell=%d", buy2.Price, sell2.Price)
	}
}

func TestPositionCapStopsSameSide(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPosition = 50
	cfg.Levels = 1
	q := NewQuoter(cfg)
	tick := mkTick(1, 99, 101, 0)

	intent := q.Generate(100, &tick, 50, 1, NewRNG(0))
	for _, quote := range intent.Quotes {
		if quote.Side == SideBuy {
			t.Fatalf("no buy quote allowed at long cap, got %+v", quote)
		}
	}
	if len(intent.Quotes) != 1 || intent.Quotes[0].Side != SideSell {
		t.Fatalf("want only sell quote, got %+v", intent.Quotes)
	}

	intentShort := q.Generate(100, &tick, -50, 2, NewRNG(0))
	if len(intentShort.Quotes) != 1 || intentShort.Quotes[0].Side != SideBuy {
		t.Fatalf("want only buy quote at short cap, got %+v", intentShort.Quotes)
	}
}

func TestFillIdempotent(t *testing.T) {
	ledger := NewPositionLedger()
	fill := Fill{FillID: "f1", Side: SideBuy, Quantity: 5}
	delta, ok := ledger.ApplyFill(fill)
	if !ok || delta != 5 || ledger.Position() != 5 {
		t.Fatalf("first fill: ok=%v delta=%d pos=%d", ok, delta, ledger.Position())
	}
	if delta, ok := ledger.ApplyFill(fill); ok || delta != 0 || ledger.Position() != 5 {
		t.Fatalf("duplicate fill must be ignored: ok=%v delta=%d pos=%d", ok, delta, ledger.Position())
	}

	// 迟到但 FillID 不同的成交正常生效；重复旧回报仍幂等。
	if _, ok := ledger.ApplyFill(Fill{FillID: "f0", Side: SideSell, Quantity: 2}); !ok {
		t.Fatal("late unique fill should apply")
	}
	if _, ok := ledger.ApplyFill(fill); ok {
		t.Fatal("duplicate old fill must still be ignored")
	}
	if position := ledger.Position(); position != 3 {
		t.Fatalf("want position 3, got %d", position)
	}
}

func TestReplayFillsDeterministic(t *testing.T) {
	fills := []Fill{
		{FillID: "a", Side: SideBuy, Quantity: 10},
		{FillID: "a", Side: SideBuy, Quantity: 10},
		{FillID: "b", Side: SideSell, Quantity: 4},
	}
	position, accepted := ReplayFills(fills)
	if position != 6 {
		t.Fatalf("want replayed position 6, got %d", position)
	}
	if len(accepted) != 2 {
		t.Fatalf("want 2 accepted fill ids, got %d", len(accepted))
	}
}

func TestQuoterQuantitySkew(t *testing.T) {
	cfg := testConfig()
	cfg.Levels = 1
	cfg.QuantitySkewPctPerUnit = 10
	q := NewQuoter(cfg)
	tick := mkTick(1, 99, 101, 0)

	// 多头 5：买量减半，卖量变为 1.5 倍。
	intent := q.Generate(100, &tick, 5, 1, NewRNG(0))
	var buyQty, sellQty int64
	for _, quote := range intent.Quotes {
		if quote.Side == SideBuy {
			buyQty = quote.Quantity
		} else {
			sellQty = quote.Quantity
		}
	}
	if buyQty != 5 || sellQty != 15 {
		t.Fatalf("want buy qty 5 sell qty 15, got %d/%d", buyQty, sellQty)
	}
}

func TestConfigApplyAndValidate(t *testing.T) {
	cfg := testConfig()
	levels := 5
	updated := cfg.Apply(ConfigUpdate{Levels: &levels})
	if updated.Levels != 5 || cfg.Levels != 2 {
		t.Fatal("Apply must return new config without mutating original")
	}
	updated.Levels = 0
	if _, err := Validate(updated); err == nil {
		t.Fatal("invalid config must be rejected")
	}
}
