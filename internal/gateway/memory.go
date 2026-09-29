package gateway

import (
	"strconv"
	"sync"
	"time"

	"github.com/guoyyit/market-maker-go/internal/types"
)

// InMemory 是进程内的本地网关实现，不依赖任何外部服务。
//
// 一个 ClientOrderID 在网关生命周期内只对应一个订单记录：
// 成交/撤销后进入终态，再次 Submit 返回 ErrOrderFinal，
// 从而保证引擎崩溃恢复时不会重复挂单。
// 成交由 SimulateFill 按确定性规则触发，回报通过 fillCh 输出。
type InMemory struct {
	mu     sync.Mutex
	orders map[string]Report
	fillCh chan Fill

	// exchangeSeq 用于生成确定性的交易所订单号。
	exchangeSeq uint64
	// fillSeq 用于生成确定性的成交序号。
	fillSeq uint64
}

// NewInMemory 创建内存网关，fillBuffer 为成交通道缓冲长度。
func NewInMemory(fillBuffer int) *InMemory {
	if fillBuffer < 1 {
		fillBuffer = 1
	}
	return &InMemory{
		orders: make(map[string]Report),
		fillCh: make(chan Fill, fillBuffer),
	}
}

var _ Gateway = (*InMemory)(nil)

// Submit 实现 Gateway，幂等提交挂单。
func (g *InMemory) Submit(q types.Quote) (Report, error) {
	if q.ClientOrderID == "" || q.Price <= 0 || q.Qty <= 0 {
		return Report{}, ErrBadOrder
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if existing, ok := g.orders[q.ClientOrderID]; ok {
		// 幂等语义：
		//   - 同一在挂单重复提交：返回既有回报，不产生第二条挂单；
		//   - 已成交/已撤销的终态 ID 再次提交：拒绝，防止"复用 ID 重新挂单"。
		if existing.Status == StatusOpen {
			return existing, nil
		}
		return existing, ErrOrderFinal
	}

	g.exchangeSeq++
	report := Report{
		ClientOrderID: q.ClientOrderID,
		ExchangeID:    "EX-" + strconv.FormatUint(g.exchangeSeq, 10),
		Status:        StatusOpen,
		Price:         q.Price,
		Qty:           q.Qty,
		Symbol:        q.Symbol,
		Side:          q.Side,
	}
	g.orders[q.ClientOrderID] = report
	return report, nil
}

// Cancel 实现 Gateway，幂等撤单。
func (g *InMemory) Cancel(clientOrderID string) (Report, error) {
	if clientOrderID == "" {
		return Report{}, ErrNotFound
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	report, ok := g.orders[clientOrderID]
	if !ok {
		// 撤不存在的单是无害的空操作，错误仅用于告知调用方。
		return Report{}, ErrNotFound
	}
	if report.Status == StatusOpen {
		report.Status = StatusCancelled
		g.orders[clientOrderID] = report
	}
	return report, nil
}

// OpenOrders 实现 Gateway，返回在挂订单快照。
func (g *InMemory) OpenOrders() map[string]Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]Report, len(g.orders))
	for id, r := range g.orders {
		if r.Status == StatusOpen {
			out[id] = r
		}
	}
	return out
}

// Fills 实现 Gateway，返回成交回报通道。
func (g *InMemory) Fills() <-chan Fill {
	return g.fillCh
}

// Reset 实现 Gateway，清空全部状态并重置序号。
func (g *InMemory) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orders = make(map[string]Report)
	g.exchangeSeq = 0
	g.fillSeq = 0
	// 排空成交通道中可能残留的回报。
	for {
		select {
		case <-g.fillCh:
			continue
		default:
			return
		}
	}
}

// SeedFillSeq 把成交自增序号推进到至少 last（崩溃恢复后调用），
// 保证之后模拟产生的成交 ID 大于任何历史成交 ID。
func (g *InMemory) SeedFillSeq(last uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last > g.fillSeq {
		g.fillSeq = last
	}
}

// SimulateFill 模拟指定在挂单全部成交，并向 Fills 通道发送一条回报。
// 对不存在、非在挂或已处理过的 ClientOrderID 返回错误。
func (g *InMemory) SimulateFill(clientOrderID string, atTime time.Time) (Fill, error) {
	g.mu.Lock()
	report, ok := g.orders[clientOrderID]
	if !ok {
		g.mu.Unlock()
		return Fill{}, ErrNotFound
	}
	if report.Status != StatusOpen {
		g.mu.Unlock()
		return Fill{}, ErrOrderFinal
	}

	g.fillSeq++
	fill := Fill{
		ID:           "FILL-" + strconv.FormatUint(g.fillSeq, 10),
		MakerOrderID: clientOrderID,
		Symbol:       report.Symbol,
		MakerSide:    report.Side,
		Price:        report.Price,
		Qty:          report.Qty,
		Seq:          g.fillSeq,
		Time:         atTime,
	}
	report.Status = StatusFilled
	g.orders[clientOrderID] = report
	g.mu.Unlock()

	g.fillCh <- fill
	return fill, nil
}
