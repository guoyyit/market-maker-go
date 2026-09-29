package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/guoyyit/market-maker-go/engine"
)

// main 是本地做市报价引擎的最小演示入口：
// 行情与成交全部由内存管道注入，使用内存持久化，不连接任何外部服务。
func main() {
	cfg := engine.DefaultConfig()
	cfg.Symbol = "DEMO"
	cfg.Levels = 2
	cfg.BaseQuantity = 10
	cfg.QuoteOffset = 3
	cfg.TickStep = 2
	cfg.MaxSpread = 40
	cfg.MaxPosition = 50
	cfg.SkewPerUnit = 1
	cfg.QuantitySkewPctPerUnit = 5
	cfg.MaxJumpTicks = 20
	cfg.StaleTimeout = 5 * engine.Second
	cfg.RecoveryTicks = 2
	cfg.RandomSeed = 7

	store, err := engine.NewFileStore(filepath.Join(".", "demo-data"))
	if err != nil {
		panic(err)
	}
	defer store.Close()

	var e *engine.Engine
	if engine.HasSnapshot(store) {
		fmt.Println("recovering engine from demo-data ...")
		e, err = engine.RecoverEngine(engine.NewSimGateway(), store)
	} else {
		e, err = engine.NewEngine(cfg, engine.NewSimGateway(), store)
	}
	if err != nil {
		panic(err)
	}

	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	ticks := []engine.Tick{
		{Symbol: "DEMO", Seq: 1, Time: base, BidPrice: 99, AskPrice: 101, LastPrice: 100},
		{Symbol: "DEMO", Seq: 2, Time: base.Add(time.Second), BidPrice: 99, AskPrice: 101, LastPrice: 100},
		// 买盘上抬穿过我方卖价：触发模拟卖出成交，持仓变负、报价随后倾斜。
		{Symbol: "DEMO", Seq: 3, Time: base.Add(2 * time.Second), BidPrice: 109, AskPrice: 111, LastPrice: 110},
		{Symbol: "DEMO", Seq: 4, Time: base.Add(3 * time.Second), BidPrice: 109, AskPrice: 111, LastPrice: 110},
		// 异常跳变：进入保护并全撤。
		{Symbol: "DEMO", Seq: 5, Time: base.Add(4 * time.Second), BidPrice: 200, AskPrice: 202, LastPrice: 201},
		// 第一笔恢复基准，其后两笔稳定健康行情：满足 RecoveryTicks=2 后恢复报价。
		{Symbol: "DEMO", Seq: 6, Time: base.Add(5 * time.Second), BidPrice: 110, AskPrice: 112, LastPrice: 111},
		{Symbol: "DEMO", Seq: 7, Time: base.Add(6 * time.Second), BidPrice: 110, AskPrice: 112, LastPrice: 111},
		{Symbol: "DEMO", Seq: 8, Time: base.Add(7 * time.Second), BidPrice: 110, AskPrice: 112, LastPrice: 111},
	}

	for _, tick := range ticks {
		result := e.ProcessOne(engine.Envelope{Tick: &tick, WallTime: tick.Time})
		fmt.Printf("tick seq=%d accepted=%v actions=%d fills=%d position=%d protected=%v\n",
			tick.Seq, result.Accepted, len(result.Actions), len(result.Fills),
			e.Kernel().Position(), e.Kernel().Protection().Active)
		for _, ev := range result.Audits {
			fmt.Printf("  audit: %s %s\n", ev.Kind, ev.Detail)
		}

		// 在第 3 笔行情后通过本地管道注入一笔外部成交回报（例如回放历史成交）。
		if tick.Seq == 3 {
			fill := engine.Fill{
				FillID:   "external-fill-1",
				OrderID:  "DEMO-1-sell-L1",
				Symbol:   "DEMO",
				Side:     engine.SideSell,
				Price:    102,
				Quantity: 10,
				Time:     tick.Time,
			}
			fillResult := e.ProcessOne(engine.Envelope{Fill: &fill, WallTime: fill.Time})
			fmt.Printf("external fill accepted=%v position=%d actions=%d\n",
				fillResult.Accepted, e.Kernel().Position(), len(fillResult.Actions))
		}
	}

	fmt.Printf("final live orders: %d, position: %d\n",
		len(e.Gateway().LiveOrders()), e.Kernel().Position())
}
