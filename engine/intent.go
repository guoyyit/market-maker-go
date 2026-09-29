package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// IntentReconciler 将新的报价意图与在挂订单集合做增量对账，
// 保证任何时刻在挂订单集合与最近一次提交的报价意图严格一致。
//
// 对账以 (方向, 档位) 槽位为身份：
//   - 意图中消失的槽位        -> 撤单
//   - 槽位价格或数量发生变化  -> 先撤旧单再挂新单（撤改）
//   - 其余槽位               -> 保持不动（增量更新）
//
// 订单 ClientID 携带意图版本号，是提交与崩溃恢复时的幂等键。
type IntentReconciler struct {
	liveByID   map[string]Order
	liveBySlot map[string]Order
	lastSeq    int64
}

// NewIntentReconciler 构造对账器。
func NewIntentReconciler() *IntentReconciler {
	return &IntentReconciler{
		liveByID:   map[string]Order{},
		liveBySlot: map[string]Order{},
	}
}

// Live 返回当前在挂订单（ClientID -> Order）的副本。
func (r *IntentReconciler) Live() map[string]Order {
	out := make(map[string]Order, len(r.liveByID))
	for id, order := range r.liveByID {
		out[id] = order
	}
	return out
}

// LastSeq 返回最近对账过的意图版本号。
func (r *IntentReconciler) LastSeq() int64 { return r.lastSeq }

// Reconcile 根据新意图产生撤/改/挂动作序列，并更新在挂集合。
// 同一版本号的意图重复提交时不产生任何动作（幂等）。
func (r *IntentReconciler) Reconcile(intent *Intent) []Action {
	if intent == nil {
		return r.CancelAll()
	}

	desired := map[string]Quote{}
	for _, quote := range intent.Quotes {
		desired[slotKey(quote.Side, quote.Level)] = quote
	}

	var cancels []string
	for slot, old := range r.liveBySlot {
		newQuote, ok := desired[slot]
		if !ok || newQuote.Price != old.Price || newQuote.Quantity != old.Quantity {
			cancels = append(cancels, old.ClientID)
		}
	}
	sort.Strings(cancels)

	var actions []Action
	for _, id := range cancels {
		actions = append(actions, CancelAction{ClientID: id})
		order := r.liveByID[id]
		delete(r.liveByID, id)
		delete(r.liveBySlot, slotKey(order.Side, order.Level))
	}

	type place struct {
		quote Quote
	}
	var places []place
	for slot, quote := range desired {
		if old, ok := r.liveBySlot[slot]; ok && old.Price == quote.Price && old.Quantity == quote.Quantity {
			continue
		}
		places = append(places, place{quote: quote})
	}
	// 固定排序：先买后卖、档位升序，保证动作序列确定。
	sort.Slice(places, func(i, j int) bool {
		a, b := places[i].quote, places[j].quote
		if a.Side != b.Side {
			return a.Side > b.Side
		}
		return a.Level < b.Level
	})
	for _, item := range places {
		quote := item.quote
		order := Order{
			ClientID:  QuoteClientID(intent.Symbol, intent.Seq, quote.Side, quote.Level),
			Symbol:    intent.Symbol,
			Side:      quote.Side,
			Price:     quote.Price,
			Quantity:  quote.Quantity,
			Level:     quote.Level,
			IntentSeq: intent.Seq,
		}
		actions = append(actions, PlaceAction{Order: order})
		r.liveByID[order.ClientID] = order
		r.liveBySlot[slotKey(quote.Side, quote.Level)] = order
	}

	r.lastSeq = intent.Seq
	return actions
}

// CancelAll 全量撤单（进入保护时使用），按 ClientID 排序输出并清空在挂集合。
func (r *IntentReconciler) CancelAll() []Action {
	ids := make([]string, 0, len(r.liveByID))
	for id := range r.liveByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	actions := make([]Action, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, CancelAction{ClientID: id})
	}
	r.liveByID = map[string]Order{}
	r.liveBySlot = map[string]Order{}
	return actions
}

// Restore 从在挂订单集合重建对账器（崩溃恢复用）。
func (r *IntentReconciler) Restore(live map[string]Order) {
	r.liveByID = make(map[string]Order, len(live))
	r.liveBySlot = make(map[string]Order, len(live))
	r.lastSeq = 0
	for _, order := range live {
		r.liveByID[order.ClientID] = order
		r.liveBySlot[slotKey(order.Side, order.Level)] = order
		if order.IntentSeq > r.lastSeq {
			r.lastSeq = order.IntentSeq
		}
	}
}

// QuoteClientID 生成某意图版本下某档报价的确定性 ClientID（幂等键）。
func QuoteClientID(symbol string, seq int64, side Side, level int) string {
	return fmt.Sprintf("%s-%d-%s-L%d", symbol, seq, side.String(), level)
}

func slotKey(side Side, level int) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(int(side)))
	b.WriteByte(':')
	b.WriteString(strconv.Itoa(level))
	return b.String()
}
