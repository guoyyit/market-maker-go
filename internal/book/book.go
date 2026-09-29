// Package book 维护"最近一次提交的报价意图"与网关在挂订单之间的对账。
//
// 不变式：对账成功后，网关在挂集合必须与最近一次意图完全一致
// （同一 ClientOrderID 价格数量一致），多余的在挂单全部撤销。
// 动作序列完全由 (旧意图, 新意图, 网关在挂快照) 决定，顺序固定：
// 先撤（消失或变价的档位），再挂（新增或变价的档位），保证确定可回放。
package book

import (
	"sort"

	"github.com/guoyyit/market-maker-go/internal/types"
)

// ActionKind 是对账动作类型。
type ActionKind int

const (
	// ActionCancel 撤销一条在挂单。
	ActionCancel ActionKind = iota + 1
	// ActionPlace 新挂一条单。
	ActionPlace
)

// Action 是一条对账指令。变价的档位产生一对 Cancel+Place，
// 因为本系统的本地网关不支持原地改单，用确定性的撤改表达。
type Action struct {
	Kind  ActionKind
	Quote types.Quote // Place 时为期望报价；Cancel 时仅 ClientOrderID 有效
}

// Reconciler 记录最近一次提交成功的意图，并据此生成对账动作。
type Reconciler struct {
	last    types.Intent
	hasLast bool
}

// NewReconciler 创建空对账器。
func NewReconciler() *Reconciler {
	return &Reconciler{}
}

// Diff 比较"当前在挂集合"与"新意图"，产出确定性动作序列。
// open 为 ClientOrderID -> 在挂报价（价格/数量）的快照。
// 序列顺序：先全部 Cancel（按 ClientOrderID 升序），再全部 Place（同序）。
func (r *Reconciler) Diff(open map[string]types.Quote, intent types.Intent) []Action {
	want := make(map[string]types.Quote, len(intent.Quotes))
	for _, q := range intent.Quotes {
		want[q.ClientOrderID] = q
	}

	var cancels, places []string
	cancelSet := map[string]struct{}{}

	// 在挂但意图中消失，或价格/数量不一致 -> 先撤。
	for id, openQuote := range open {
		wantQuote, ok := want[id]
		if !ok || !sameQuote(openQuote, wantQuote) {
			cancels = append(cancels, id)
			cancelSet[id] = struct{}{}
		}
	}

	// 意图中有、但在挂缺失，或刚被撤销的变价档位 -> 再挂。
	for id := range want {
		if _, needCancel := cancelSet[id]; needCancel {
			places = append(places, id)
			continue
		}
		if _, ok := open[id]; !ok {
			places = append(places, id)
		}
	}

	sortStrings(cancels)
	sortStrings(places)

	actions := make([]Action, 0, len(cancels)+len(places))
	for _, id := range cancels {
		actions = append(actions, Action{
			Kind:  ActionCancel,
			Quote: open[id],
		})
	}
	for _, id := range places {
		actions = append(actions, Action{
			Kind:  ActionPlace,
			Quote: want[id],
		})
	}
	return actions
}

// LastIntent 返回最近一次提交成功的意图；尚无意图时第二个返回值为 false。
func (r *Reconciler) LastIntent() (types.Intent, bool) {
	return r.last, r.hasLast
}

// SetLastIntent 由引擎在对账动作全部提交成功后调用，固化最近意图。
func (r *Reconciler) SetLastIntent(intent types.Intent) {
	r.last = cloneIntent(intent)
	r.hasLast = true
}

// Clear 在进入保护/全撤时清空已记录意图（网关侧也应已全部撤销）。
func (r *Reconciler) Clear() {
	r.last = types.Intent{}
	r.hasLast = false
}

// CancelAll 生成撤销 open 中全部在挂单的动作（按 ClientOrderID 升序）。
func CancelAll(open map[string]types.Quote) []Action {
	ids := make([]string, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sortStrings(ids)
	actions := make([]Action, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, Action{
			Kind:  ActionCancel,
			Quote: open[id],
		})
	}
	return actions
}

func sameQuote(a, b types.Quote) bool {
	return a.ClientOrderID == b.ClientOrderID &&
		a.Price == b.Price &&
		a.Qty == b.Qty &&
		a.Side == b.Side &&
		a.Symbol == b.Symbol &&
		a.Level == b.Level
}

func cloneIntent(in types.Intent) types.Intent {
	out := in
	if in.Quotes != nil {
		out.Quotes = append([]types.Quote(nil), in.Quotes...)
	}
	return out
}

func sortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}
