package engine

import (
	"strconv"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// quoteSnapshot 把在挂报价折叠成 ID -> "price:qty side:level" 的可比较集合。
func quoteSnapshot(obs Observation) map[string]string {
	out := make(map[string]string, len(obs.OpenQuotes))
	for _, q := range obs.OpenQuotes {
		out[q.ClientOrderID] = quoteSig(q)
	}
	return out
}

func quoteSig(q types.Quote) string {
	return q.Side.String() + "/" + strconv.Itoa(q.Level) +
		"@" + strconv.FormatInt(q.Price, 10) + "x" + strconv.FormatInt(q.Qty, 10)
}

// 一组 3 笔成交：买 7、卖 3、买 5，各自携带不同且与排列无关的序号。
func permTrades(base time.Time) []types.Trade {
	buyMaker := pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 0)
	sellMaker := pricing.ClientOrderID(testSymbol, types.SideSell, 0, 0)
	return []types.Trade{
		{
			ID: "T-BUY7", Symbol: testSymbol, MakerOrderID: buyMaker,
			MakerSide: types.SideBuy, Price: 98, Qty: 7,
			Seq: 30, Time: base.Add(3 * time.Millisecond),
		},
		{
			ID: "T-SELL3", Symbol: testSymbol, MakerOrderID: sellMaker,
			MakerSide: types.SideSell, Price: 102, Qty: 3,
			Seq: 10, Time: base.Add(1 * time.Millisecond),
		},
		{
			ID: "T-BUY5", Symbol: testSymbol, MakerOrderID: buyMaker,
			MakerSide: types.SideBuy, Price: 98, Qty: 5,
			Seq: 20, Time: base.Add(2 * time.Millisecond),
		},
	}
}

// runPermutation 在全新引擎上按给定顺序注入首帧与三笔成交，
// 返回最终持仓与在挂报价快照。
func runPermutation(t *testing.T, order []int) (int64, map[string]string) {
	t.Helper()
	cfg := testConfig()
	cfg.SkewCoeff = 0
	cfg.QtySkewCoeff = 0
	h := newHarness(t, cfg)
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	trades := permTrades(base)
	var obs Observation
	for _, idx := range order {
		obs = h.send(Event{Trade: &TradeEvent{Trade: trades[idx]}})
	}
	return obs.Position, quoteSnapshot(obs)
}

func TestTradeSetResultIndependentOfArrivalOrder(t *testing.T) {
	// 三笔成交的全部 6 种排列。
	perms := [][]int{
		{0, 1, 2},
		{0, 2, 1},
		{1, 0, 2},
		{1, 2, 0},
		{2, 0, 1},
		{2, 1, 0},
	}

	var wantPos int64
	var wantQuotes map[string]string
	for i, order := range perms {
		pos, quotes := runPermutation(t, order)
		// 买 7 + 买 5 - 卖 3 = 9。
		if pos != 9 {
			t.Fatalf("perm %v: position=%d, want 9", order, pos)
		}
		if i == 0 {
			wantPos = pos
			wantQuotes = quotes
			continue
		}
		if pos != wantPos {
			t.Fatalf("perm %v: position %d differs from %d", order, pos, wantPos)
		}
		if len(quotes) != len(wantQuotes) {
			t.Fatalf("perm %v: quote count %d differs from %d",
				order, len(quotes), len(wantQuotes))
		}
		for id, sig := range wantQuotes {
			if got, ok := quotes[id]; !ok || got != sig {
				t.Fatalf("perm %v: quote %s = %q ok=%v, want %q",
					order, id, got, ok, sig)
			}
		}
	}
}

func TestOutOfOrderTradeAfterCrashAppliesOnce(t *testing.T) {
	cfg := testConfig()
	cfg.SkewCoeff = 0
	cfg.QtySkewCoeff = 0
	h := newHarness(t, cfg)
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 先注入序号较大的成交（Seq=50，买 10）。
	first := types.Trade{
		ID: "T-NEW", Symbol: testSymbol,
		MakerOrderID: pricing.ClientOrderID(testSymbol, types.SideBuy, 0, 0),
		MakerSide:    types.SideBuy, Price: 98, Qty: 10,
		Seq: 50, Time: base.Add(5 * time.Millisecond),
	}
	obs := h.send(Event{Trade: &TradeEvent{Trade: first}})
	if obs.Skipped || obs.Position != 10 {
		t.Fatalf("first trade: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}

	// 崩溃重启：持仓按持久化成交集合重算为 10，在挂与最近意图一致。
	eng := h.restart(t)
	if pos := eng.Snapshot().Position; pos != 10 {
		t.Fatalf("position after restart = %d, want 10", pos)
	}
	if h.openCount() != 4 {
		t.Fatalf("open after restart = %d, want 4", h.openCount())
	}

	// 重启后才迟到一笔旧序号（Seq=10，买 4）成交：必须生效一次。
	late := types.Trade{
		ID: "T-LATE", Symbol: testSymbol,
		MakerOrderID: pricing.ClientOrderID(testSymbol, types.SideBuy, 1, 0),
		MakerSide:    types.SideBuy, Price: 97, Qty: 4,
		Seq: 10, Time: base.Add(1 * time.Millisecond),
	}
	obs = h.send(Event{Trade: &TradeEvent{Trade: late}})
	if obs.Skipped {
		t.Fatal("late trade after restart must apply")
	}
	if obs.Position != 14 {
		t.Fatalf("position after late trade = %d, want 14", obs.Position)
	}

	// 再重启一次：持仓必须仍等于对持久化成交集合重算的结果，不重复计。
	eng = h.restart(t)
	if pos := eng.Snapshot().Position; pos != 14 {
		t.Fatalf("position after second restart = %d, want 14", pos)
	}

	// 完全相同的迟到成交重复注入：忽略，持仓不变。
	obs = h.send(Event{Trade: &TradeEvent{Trade: late}})
	if !obs.Skipped || obs.Position != 14 {
		t.Fatalf("dup late trade: skipped=%v pos=%d", obs.Skipped, obs.Position)
	}

	// 在挂报价集合与最近意图逐单一致（4 档，价格为基础价）。
	if h.openCount() != 4 {
		t.Fatalf("final open = %d, want 4", h.openCount())
	}
}
