package engine

import (
	"context"
	"testing"
)

func replayScenario() []Envelope {
	mk := func(seq int64, mid int64, sec int) Envelope {
		t := mkTick(seq, mid-1, mid+1, sec)
		return Envelope{Tick: &t, WallTime: t.Time}
	}
	fill := func(id string, side Side, qty int64, sec int) Envelope {
		t := mkTick(1, 0, 0, sec)
		f := Fill{FillID: id, OrderID: "o" + id, Side: side, Quantity: qty, Time: t.Time}
		return Envelope{Fill: &f, WallTime: t.Time}
	}
	levels := 3
	update := Envelope{
		Update:   &ConfigUpdate{Levels: &levels},
		WallTime: mkTick(1, 0, 0, 2).Time,
	}
	return []Envelope{
		mk(1, 100, 0),
		mk(2, 101, 1),
		fill("f1", SideBuy, 4, 1),
		update,
		mk(3, 102, 2),
		mk(4, 103, 3),
		fill("f1", SideBuy, 4, 3), // 重复成交，必须忽略
		{Tick: ptrTick(mkTick(2, 100, 102, 1)), WallTime: mkTick(2, 100, 102, 1).Time}, // 乱序行情
	}
}

func TestDeterministicReplay(t *testing.T) {
	cfg := testConfig()
	cfg.RandomSeed = 42
	cfg.QuoteOffset = 3
	cfg.TickStep = 2

	first, err := NewReplayer(cfg).WithGateway(newStubGateway()).Run(context.Background(), replayScenario())
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	second, err := NewReplayer(cfg).WithGateway(newStubGateway()).Run(context.Background(), replayScenario())
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	if at, equal := CompareTraces(first, second); !equal {
		t.Fatalf("two replays diverged at index %d", at)
	}

	// 末态：重复成交不生效，持仓为 4。
	if position := first[len(first)-1].PositionAfter; position != 4 {
		t.Fatalf("want final position 4, got %d", position)
	}
	// 热更新后档位数为 3，行情驱动的意图应包含 6 档报价（无仓位上限限制）。
	lastTickEntry := first[4]
	if intent := lastTickEntry.Result.Intent; intent == nil || len(intent.Quotes) != 6 {
		t.Fatalf("want 6 quotes after level hot update, got %+v", lastTickEntry.Result.Intent)
	}
}

func TestDifferentSeedDivergesOrZeroSeedStable(t *testing.T) {
	// 种子 0 表示无抖动，轨迹同样确定。
	cfg := testConfig()
	cfg.RandomSeed = 0
	a, err := NewReplayer(cfg).Run(context.Background(), replayScenario())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewReplayer(cfg).Run(context.Background(), replayScenario())
	if err != nil {
		t.Fatal(err)
	}
	if _, equal := CompareTraces(a, b); !equal {
		t.Fatal("zero-seed replay must be deterministic")
	}
}
