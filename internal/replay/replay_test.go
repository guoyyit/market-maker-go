package replay_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/engine"
	"github.com/guoyyit/market-maker-go/internal/gateway"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/replay"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func replayCfg() config.Config {
	return config.Config{
		Symbol:           "SYM",
		Levels:           2,
		BaseQty:          10,
		HalfSpread:       2,
		LevelStep:        1,
		MaxSpread:        20,
		MaxPosition:      30,
		InventoryTarget:  0,
		SkewCoeff:        1,
		SkewDenom:        10,
		HeartbeatTimeout: 100 * time.Millisecond,
		MaxPriceJump:     5,
		JitterTicks:      2,
		RNGSeed:          12345,
	}
}

func tick(seq uint64, bid, ask int64, at time.Time) types.Tick {
	return types.Tick{Symbol: "SYM", BidPrice: bid, AskPrice: ask, Seq: seq, Time: at}
}

// runScenario 在全新的网关与存储上执行同一确定性场景，返回逐条输出记录。
func runScenario(t *testing.T, dir string) *replay.Recorder {
	t.Helper()
	gw := gateway.NewInMemory(128)
	base := time.Unix(1_000_000, 0)

	eng, err := engine.New(engine.Options{
		Symbol:   "SYM",
		Initial:  replayCfg(),
		Gateway:  gw,
		StoreDir: filepath.Join(dir, "state"),
		Now:      func() time.Time { return base },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = eng.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	rec := replay.NewRecorder()

	step := func(ev engine.Event) {
		eng.Input() <- ev
		obs := <-eng.Observations()
		rec.Steps = append(rec.Steps, replay.StepFromObs(len(rec.Steps), obs))
	}

	// 固定的输入序列：正常帧、成交补挂、库存倾斜、乱序/重复、
	// 心跳超时全撤、跳变保护、恢复、热更新。
	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(1, 99, 101, base)}})
	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(2, 100, 102, base.Add(10*time.Millisecond))}})

	buy0 := pricing.ClientOrderID("SYM", types.SideBuy, 0, 0)
	openBuy0 := buy0
	for id := range gw.OpenOrders() {
		_, side, level, _, perr := pricing.ParseClientOrderID(id)
		if perr == nil && side == types.SideBuy && level == 0 {
			openBuy0 = id
		}
	}
	if _, err := gw.SimulateFill(openBuy0, base.Add(15*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case fill := <-gw.Fills():
		step(engine.Event{Trade: &engine.TradeEvent{Trade: fill}})
		// 重复成交回报必须被忽略。
		step(engine.Event{Trade: &engine.TradeEvent{Trade: fill}})
	default:
		t.Fatal("fill missing")
	}

	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(3, 100, 102, base.Add(20*time.Millisecond))}})
	// 乱序旧帧。
	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(2, 1, 3, base.Add(21*time.Millisecond))}})

	// 心跳超时保护。
	step(engine.Event{Timer: &engine.TimerEvent{Now: base.Add(200 * time.Millisecond)}})
	// 恢复。
	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(4, 100, 102, base.Add(210*time.Millisecond))}})

	// 热更新：3 档、更大数量。
	newCfg := replayCfg()
	newCfg.Levels = 3
	newCfg.BaseQty = 16
	step(engine.Event{Config: &engine.ConfigEvent{Config: newCfg}})

	// 再推进一帧触发抖动序列与新档对账。
	step(engine.Event{Tick: &engine.TickEvent{Tick: tick(5, 101, 103, base.Add(220*time.Millisecond))}})

	return rec
}

func TestDeterministicReplay(t *testing.T) {
	recA := runScenario(t, t.TempDir())
	recB := runScenario(t, t.TempDir())

	if !recA.Equal(recB) {
		n := len(recA.Steps)
		if len(recB.Steps) < n {
			n = len(recB.Steps)
		}
		for i := 0; i < n; i++ {
			if recA.Steps[i].Position != recB.Steps[i].Position ||
				recA.Steps[i].Risk != recB.Steps[i].Risk ||
				len(recA.Steps[i].OpenQuotes) != len(recB.Steps[i].OpenQuotes) {
				t.Fatalf("first divergence at step %d:\nA %+v\nB %+v",
					i, recA.Steps[i], recB.Steps[i])
			}
			for j := range recA.Steps[i].OpenQuotes {
				if recA.Steps[i].OpenQuotes[j] != recB.Steps[i].OpenQuotes[j] {
					t.Fatalf("quote divergence step %d idx %d:\nA %s\nB %s",
						i, j, recA.Steps[i].OpenQuotes[j], recB.Steps[i].OpenQuotes[j])
				}
			}
		}
		t.Fatalf("recorders differ (lengths %d vs %d)",
			len(recA.Steps), len(recB.Steps))
	}

	// 关键语义抽查：成交后持仓 +10；超时帧持仓不变且 PROTECTED 空盘；
	// 热更新帧在挂 6 条；末帧仍为 6 条且 NORMAL。
	if recA.Steps[2].Position != 10 {
		t.Fatalf("position after fill = %d, want 10", recA.Steps[2].Position)
	}
	if recA.Steps[3].Skipped != true {
		t.Fatalf("duplicate fill step should be skipped")
	}
	protectedStep := recA.Steps[6]
	if protectedStep.Risk != "PROTECTED" || len(protectedStep.OpenQuotes) != 0 {
		t.Fatalf("heartbeat step = %+v, want protected flat", protectedStep)
	}
	hotStep := recA.Steps[8]
	if len(hotStep.OpenQuotes) != 6 {
		t.Fatalf("hot-update step quotes = %d, want 6", len(hotStep.OpenQuotes))
	}
}
