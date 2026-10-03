package engine

import (
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// fillOpen 模拟成交当前在挂的指定侧别/档位，返回成交回报。
func (h *harness) fillOpen(side types.Side, level int, at time.Time) types.Trade {
	h.t.Helper()
	var id string
	for openID := range h.gw.OpenOrders() {
		_, s, lvl, _, err := pricing.ParseClientOrderID(openID)
		if err == nil && s == side && lvl == level {
			id = openID
			break
		}
	}
	if id == "" {
		h.t.Fatalf("no open %v level %d to fill", side, level)
	}
	fill, err := h.gw.SimulateFill(id, at)
	if err != nil {
		h.t.Fatalf("simulate fill: %v", err)
	}
	return fill
}

// simulateThreeFills 在买0、买1、卖0 三个不同档位各模拟一笔成交，
// 得到 FILL-1/2/3（序号 1/2/3）。三笔成交方向/档位不同，
// 以任意顺序注入时最终持仓与在挂报价都应收敛一致。
func simulateThreeFills(h *harness, base time.Time) []types.Trade {
	t1 := h.fillOpen(types.SideBuy, 0, base.Add(1*time.Millisecond))
	t2 := h.fillOpen(types.SideBuy, 1, base.Add(2*time.Millisecond))
	t3 := h.fillOpen(types.SideSell, 0, base.Add(3*time.Millisecond))
	return []types.Trade{t1, t2, t3}
}

// quoteStrings 把报价集合转为排序后的 "ID@价格x数量" 字符串切片。
func quoteStrings(quotes []types.Quote) []string {
	out := make([]string, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, q.ClientOrderID+"@"+
			strconv.FormatInt(q.Price, 10)+"x"+strconv.FormatInt(q.Qty, 10))
	}
	sort.Strings(out)
	return out
}

// quoteFingerprints 把报价集合转为排序后的 "侧别-档位@价格x数量" 字符串切片
// （不含订单代号）。成交顺序不同会使代号轮换路径不同，
// 但价格/数量仅由 (最近行情, 持仓) 决定，应逐单一致。
func quoteFingerprints(quotes []types.Quote) []string {
	out := make([]string, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, q.Side.String()+"-"+strconv.Itoa(q.Level)+"@"+
			strconv.FormatInt(q.Price, 10)+"x"+strconv.FormatInt(q.Qty, 10))
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// permutations 返回 0..n-1 的全部排列。
func permutations(n int) [][]int {
	var out [][]int
	arr := make([]int, n)
	var gen func(k int)
	gen = func(k int) {
		if k == 1 {
			cp := make([]int, n)
			copy(cp, arr)
			out = append(out, cp)
			return
		}
		gen(k - 1)
		for i := 0; i < k-1; i++ {
			if k%2 == 0 {
				arr[i], arr[k-1] = arr[k-1], arr[i]
			} else {
				arr[0], arr[k-1] = arr[k-1], arr[0]
			}
			gen(k - 1)
		}
	}
	for i := 0; i < n; i++ {
		arr[i] = i
	}
	gen(n)
	return out
}

func TestOutOfOrderTradesSameFinalPositionAndQuotes(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	perms := permutations(3)

	var wantOpen []string
	var wantPos int64
	for _, p := range perms {
		h := newHarness(t, testConfig())
		h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

		trades := simulateThreeFills(h, base)

		// 按排列顺序注入成交回报。
		var obs Observation
		for _, idx := range p {
			obs = h.send(Event{Trade: &TradeEvent{Trade: trades[idx]}})
			if obs.Skipped {
				t.Fatalf("trade %d was skipped in permutation %v", idx, p)
			}
		}

		// 最终持仓必须等于按成交集合重算的结果（+10 -10 = +10），
		// 价格/数量仅由 (最近行情, 持仓) 决定，必须逐单一致；
		// 订单代号因成交顺序不同而轮换路径不同，不纳入比较。
		if obs.Position != 10 {
			t.Fatalf("permutation %v: position = %d, want 10", p, obs.Position)
		}
		gotOpen := quoteFingerprints(obs.OpenQuotes)
		if wantOpen == nil {
			wantOpen = gotOpen
			wantPos = obs.Position
		} else if !equalStrings(wantOpen, gotOpen) || wantPos != obs.Position {
			t.Fatalf("permutation %v: result differs:\nwant pos=%d open=%v\ngot  pos=%d open=%v",
				p, wantPos, wantOpen, obs.Position, gotOpen)
		}
		h.stop()
	}
}

func TestOutOfOrderTradesThenCrashRecovery(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	trades := simulateThreeFills(h, base)

	// 乱序注入：FILL-3（卖0）先到，FILL-2（买1）次之，FILL-1（买0）最后。
	for _, idx := range []int{2, 1, 0} {
		obs := h.send(Event{Trade: &TradeEvent{Trade: trades[idx]}})
		if obs.Skipped {
			t.Fatalf("trade %d was skipped", idx)
		}
	}

	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(10*time.Millisecond))}})
	if obs.Position != 10 {
		t.Fatalf("position before crash = %d, want 10", obs.Position)
	}
	beforeOpen := quoteStrings(obs.OpenQuotes)

	// 崩溃重启：持仓必须等于按持久化成交集合重算的结果，
	// 在挂报价与最近意图一致，不重复挂单。
	eng := h.restart(t)
	if eng.Snapshot().Position != 10 {
		t.Fatalf("recomputed position = %d, want 10", eng.Snapshot().Position)
	}
	afterObs := h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 99, 101, base.Add(20*time.Millisecond))}})
	afterOpen := quoteStrings(afterObs.OpenQuotes)
	if !equalStrings(beforeOpen, afterOpen) {
		t.Fatalf("open quotes differ after recovery:\nbefore %v\nafter  %v",
			beforeOpen, afterOpen)
	}
}

func TestDuplicateTradeIgnoredRegardlessOfOrder(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	perms := permutations(3)

	for _, p := range perms {
		h := newHarness(t, testConfig())
		h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
		trades := simulateThreeFills(h, base)

		// 每个排列都注入一次完全重复的成交回报（与排列无关），
		// 重复回报必须被忽略，持仓不变。
		dup := trades[p[0]]
		var obs Observation
		for _, idx := range p {
			obs = h.send(Event{Trade: &TradeEvent{Trade: trades[idx]}})
			if obs.Skipped {
				t.Fatalf("trade %d was skipped in permutation %v", idx, p)
			}
		}
		posBefore := obs.Position
		obs = h.send(Event{Trade: &TradeEvent{Trade: dup}})
		if !obs.Skipped || obs.Position != posBefore {
			t.Fatalf("permutation %v: duplicate trade not ignored: skipped=%v pos=%d",
				p, obs.Skipped, obs.Position)
		}
		h.stop()
	}
}
