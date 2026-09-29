package book

import (
	"testing"

	"github.com/guoyyit/market-maker-go/internal/types"
)

func q(id string, price, qty int64) types.Quote {
	return types.Quote{
		Symbol:        "SYM",
		ClientOrderID: id,
		Price:         price,
		Qty:           qty,
		Side:          types.SideBuy,
	}
}

func intent(quotes ...types.Quote) types.Intent {
	return types.Intent{Symbol: "SYM", Quotes: quotes}
}

func m(quotes ...types.Quote) map[string]types.Quote {
	out := map[string]types.Quote{}
	for _, quote := range quotes {
		out[quote.ClientOrderID] = quote
	}
	return out
}

// apply 在内存里模拟动作执行，返回执行后的在挂集合。
func apply(open map[string]types.Quote, actions []Action) map[string]types.Quote {
	out := map[string]types.Quote{}
	for id, quote := range open {
		out[id] = quote
	}
	for _, action := range actions {
		switch action.Kind {
		case ActionCancel:
			delete(out, action.Quote.ClientOrderID)
		case ActionPlace:
			out[action.Quote.ClientOrderID] = action.Quote
		}
	}
	return out
}

func TestDiffInitialPlace(t *testing.T) {
	r := NewReconciler()
	want := intent(q("a", 100, 1), q("b", 102, 1))
	actions := r.Diff(m(), want)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(actions))
	}
	for _, action := range actions {
		if action.Kind != ActionPlace {
			t.Fatalf("initial action kind = %v, want Place", action.Kind)
		}
	}
	// 动作顺序按 ClientOrderID 升序确定。
	if actions[0].Quote.ClientOrderID != "a" || actions[1].Quote.ClientOrderID != "b" {
		t.Fatalf("unexpected action order: %s then %s",
			actions[0].Quote.ClientOrderID, actions[1].Quote.ClientOrderID)
	}
	r.SetLastIntent(want)
}

func TestDiffPriceChangeIsCancelThenPlace(t *testing.T) {
	r := NewReconciler()
	open := m(q("a", 100, 1))
	newIntent := intent(q("a", 99, 1))

	actions := r.Diff(open, newIntent)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want cancel+place", len(actions))
	}
	if actions[0].Kind != ActionCancel || actions[1].Kind != ActionPlace {
		t.Fatalf("want [Cancel, Place], got %v %v", actions[0].Kind, actions[1].Kind)
	}
	if actions[0].Quote.ClientOrderID != "a" || actions[1].Quote.ClientOrderID != "a" {
		t.Fatalf("both actions must target a, got %+v", actions)
	}

	result := apply(open, actions)
	if got := result["a"]; got.Price != 99 {
		t.Fatalf("price after reconcile = %d, want 99", got.Price)
	}
}

func TestDiffUnchangedProducesNoActions(t *testing.T) {
	r := NewReconciler()
	open := m(q("a", 100, 1), q("b", 102, 1))
	same := intent(q("a", 100, 1), q("b", 102, 1))

	if actions := r.Diff(open, same); len(actions) != 0 {
		t.Fatalf("unchanged intent produced %d actions", len(actions))
	}
}

func TestDiffRemovesStaleOrders(t *testing.T) {
	r := NewReconciler()
	open := m(q("a", 100, 1), q("stale", 50, 1))
	want := intent(q("a", 100, 1))

	actions := r.Diff(open, want)
	if len(actions) != 1 || actions[0].Kind != ActionCancel ||
		actions[0].Quote.ClientOrderID != "stale" {
		t.Fatalf("want single cancel of stale, got %+v", actions)
	}
	result := apply(open, actions)
	if _, exists := result["stale"]; exists {
		t.Fatal("stale order must be removed")
	}
}

func TestDiffConvergesToIntent(t *testing.T) {
	r := NewReconciler()
	open := m(q("a", 100, 1), q("b", 102, 2), q("ghost", 1, 1))
	want := intent(q("a", 101, 1), q("b", 102, 2), q("c", 104, 1))

	result := apply(open, r.Diff(open, want))
	if len(result) != len(want.Quotes) {
		t.Fatalf("open size = %d, want %d", len(result), len(want.Quotes))
	}
	for _, expected := range want.Quotes {
		got, ok := result[expected.ClientOrderID]
		if !ok {
			t.Fatalf("missing expected order %s", expected.ClientOrderID)
		}
		if got.Price != expected.Price || got.Qty != expected.Qty {
			t.Fatalf("order %s = %d x %d, want %d x %d",
				expected.ClientOrderID, got.Price, got.Qty, expected.Price, expected.Qty)
		}
	}
	r.SetLastIntent(want)
	if last, ok := r.LastIntent(); !ok || len(last.Quotes) != 3 {
		t.Fatalf("last intent not recorded correctly: ok=%v", ok)
	}
}

func TestCancelAllOrdersSorted(t *testing.T) {
	open := m(q("z", 1, 1), q("a", 2, 1), q("m", 3, 1))
	actions := CancelAll(open)
	if len(actions) != 3 {
		t.Fatalf("actions = %d, want 3", len(actions))
	}
	wantOrder := []string{"a", "m", "z"}
	for i, action := range actions {
		if action.Kind != ActionCancel || action.Quote.ClientOrderID != wantOrder[i] {
			t.Fatalf("action %d = %+v, want cancel %s", i, action, wantOrder[i])
		}
	}
}
