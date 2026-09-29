package engine

import (
	"errors"
	"sort"
	"time"
)

// Gateway 是本地内存模拟的订单网关：所有动作在本地立即生效，不依赖外部服务。
// 提交以 ClientID 幂等，重复提交同一 ClientID 不会产生两张订单。
type Gateway interface {
	Place(order Order) (accepted bool, err error)
	Cancel(clientID string) (found bool, err error)
	LiveOrders() map[string]Order
}

var (
	// ErrDuplicateClientID 表示同一 ClientID 的订单已存在。
	ErrDuplicateClientID = errors.New("duplicate client order id")
	// ErrOrderNotFound 表示撤单时找不到对应订单（可能已成交）。
	ErrOrderNotFound = errors.New("order not found")
)

// SimGateway 是 Gateway 的内存实现，另提供确定性的本地撮合 Match。
type SimGateway struct {
	live map[string]Order
}

// NewSimGateway 构造模拟网关。
func NewSimGateway() *SimGateway {
	return &SimGateway{live: map[string]Order{}}
}

// Place 幂等提交订单：ClientID 已存在时返回 ErrDuplicateClientID 且不改变状态。
func (g *SimGateway) Place(order Order) (bool, error) {
	if order.ClientID == "" {
		return false, errors.New("empty client order id")
	}
	if _, exists := g.live[order.ClientID]; exists {
		return false, ErrDuplicateClientID
	}
	g.live[order.ClientID] = order
	return true, nil
}

// Cancel 撤销订单；不存在（已成交或已撤）返回 ErrOrderNotFound。
func (g *SimGateway) Cancel(clientID string) (bool, error) {
	if _, ok := g.live[clientID]; !ok {
		return false, ErrOrderNotFound
	}
	delete(g.live, clientID)
	return true, nil
}

// LiveOrders 返回在挂订单副本。
func (g *SimGateway) LiveOrders() map[string]Order {
	out := make(map[string]Order, len(g.live))
	for id, order := range g.live {
		out[id] = order
	}
	return out
}

// Match 在给定行情下对在挂订单做确定性模拟撮合，返回产生的成交回报并移除已成交订单。
// 规则：买单价格 >= tick.AskPrice 时以 AskPrice 全部成交；
// 卖单价格 <= tick.BidPrice 时以 BidPrice 全部成交（不模拟部分成交）。
// 多档同时可成交时严格按 ClientID 字典序处理，FillID 由 ClientID 确定性派生，
// 因此同序列行情回放必然产生同一组成交。
func (g *SimGateway) Match(tick Tick, now time.Time) []Fill {
	if tick.BidPrice <= 0 || tick.AskPrice <= 0 {
		return nil
	}
	ids := make([]string, 0, len(g.live))
	for id := range g.live {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var fills []Fill
	for _, id := range ids {
		order, ok := g.live[id]
		if !ok {
			continue
		}
		fillPrice := int64(0)
		switch order.Side {
		case SideBuy:
			if order.Price >= tick.AskPrice {
				fillPrice = tick.AskPrice
			}
		case SideSell:
			if order.Price <= tick.BidPrice {
				fillPrice = tick.BidPrice
			}
		}
		if fillPrice == 0 {
			continue
		}
		fills = append(fills, Fill{
			FillID:   id + "#f1",
			OrderID:  id,
			Symbol:   order.Symbol,
			Side:     order.Side,
			Price:    fillPrice,
			Quantity: order.Quantity,
			Time:     now,
		})
		delete(g.live, id)
	}
	return fills
}

// Restore 从在挂订单集合重建网关（崩溃恢复对账用）。
func (g *SimGateway) Restore(live map[string]Order) {
	g.live = make(map[string]Order, len(live))
	for id, order := range live {
		g.live[id] = order
	}
}
