// Command mm 是本地做市报价引擎的端到端演示：
//
// 在进程内构造内存网关与引擎，通过内存通道注入确定性的
// 行情 / 成交 / 定时器 / 配置事件序列，逐条打印报价、撤单、
// 持仓与保护状态变化。整个过程不连接任何外部服务。
//
// 运行：go run ./cmd/mm [-dir ./run-state]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/engine"
	"github.com/guoyyit/market-maker-go/internal/gateway"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

const symbol = "DEMO"

func demoConfig() config.Config {
	return config.Config{
		Symbol:           symbol,
		Levels:           2,
		BaseQty:          10,
		HalfSpread:       2,
		LevelStep:        1,
		MaxSpread:        20,
		MaxPosition:      100,
		InventoryTarget:  0,
		SkewCoeff:        1,
		SkewDenom:        10,
		QtySkewCoeff:     100,
		HeartbeatTimeout: 100 * time.Millisecond,
		MaxPriceJump:     5,
		JitterTicks:      1,
		RNGSeed:          20260930,
	}
}

func tickEvent(seq uint64, bid, ask int64, at time.Time) engine.Event {
	return engine.Event{Tick: &engine.TickEvent{Tick: types.Tick{
		Symbol: symbol, BidPrice: bid, AskPrice: ask, Seq: seq, Time: at,
	}}}
}

func main() {
	dir := flag.String("dir", "./run-state", "directory for snapshot and audit log")
	flag.Parse()

	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "mm: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	gw := gateway.NewInMemory(128)
	base := time.Unix(1_000_000, 0)

	eng, err := engine.New(engine.Options{
		Symbol:   symbol,
		Initial:  demoConfig(),
		Gateway:  gw,
		StoreDir: dir,
		Now:      func() time.Time { return base },
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := eng.Recover(ctx); err != nil {
		return err
	}
	runDone := make(chan struct{})
	go func() {
		_ = eng.Run(ctx)
		close(runDone)
	}()
	defer func() {
		cancel()
		<-runDone
	}()

	step := func(label string, ev engine.Event) {
		eng.Input() <- ev
		obs := <-eng.Observations()
		printObs(label, obs)
	}

	fmt.Println("== local market-maker demo (in-process, deterministic) ==")

	// 若目录内有历史快照（上次运行崩溃/退出），事件序号从水位之后继续，
	// 并用一个超出心跳窗口的时间发首帧，使保护状态（如有）先恢复。
	snap := eng.Snapshot()
	seq := snap.LastTickSeq
	t0 := base.Add(300 * time.Millisecond)
	if snap.LastTickSeq == 0 {
		t0 = base
	}

	step("quote", tickEvent(seq+1, 99, 101, t0))
	step("reprice", tickEvent(seq+2, 100, 102, t0.Add(10*time.Millisecond)))

	// 模拟买一档被成交，驱动库存与倾斜。
	var buy0 string
	for id := range gw.OpenOrders() {
		_, side, level, _, _ := pricing.ParseClientOrderID(id)
		if side == types.SideBuy && level == 0 {
			buy0 = id
		}
	}
	if buy0 != "" {
		fill, err := gw.SimulateFill(buy0, t0.Add(15*time.Millisecond))
		if err != nil {
			return err
		}
		step("buy level 0 filled (+10)", engine.Event{Trade: &engine.TradeEvent{Trade: fill}})
		step("tick 3 skew", tickEvent(seq+3, 100, 102, t0.Add(20*time.Millisecond)))
	}

	// 行情停更 -> 全撤保护。
	step("heartbeat timeout", engine.Event{Timer: &engine.TimerEvent{
		Now: t0.Add(200 * time.Millisecond),
	}})
	// 恢复报价。
	step("recover", tickEvent(seq+4, 100, 102, t0.Add(210*time.Millisecond)))

	// 热更新到 3 档、更大数量。
	newCfg := demoConfig()
	newCfg.Levels = 3
	newCfg.BaseQty = 12
	step("hot update: 3 levels x 12", engine.Event{Config: &engine.ConfigEvent{Config: newCfg}})

	// 单笔异常跳变 -> 全撤保护。
	step("abnormal jump", tickEvent(seq+5, 120, 122, t0.Add(220*time.Millisecond)))

	fmt.Printf("\nfinal snapshot: %+v\n", eng.Snapshot())
	fmt.Printf("state dir: %s (state.json + risk.log)\n", dir)
	return nil
}

func printObs(label string, obs engine.Observation) {
	fmt.Printf("\n[%s] risk=%s position=%d open=%d",
		label, obs.Risk, obs.Position, len(obs.OpenQuotes))
	if obs.Skipped {
		fmt.Print(" SKIPPED(stale/dup)")
	}
	if obs.ConfigRejected {
		fmt.Printf(" CONFIG-REJECTED(%s)", obs.RejectReason)
	}
	fmt.Println()
	for _, tr := range obs.Transitions {
		fmt.Printf("    transition: %s -> %s reason=%s %s\n",
			tr.From, tr.To, tr.Reason, tr.Detail)
	}
	for _, quote := range obs.OpenQuotes {
		fmt.Printf("    %-14s %s %d x %d\n",
			quote.ClientOrderID, quote.Side, quote.Price, quote.Qty)
	}
}
