package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/types"
)

func sampleQuote() types.Quote {
	return types.Quote{
		Symbol:        "SYM",
		Side:          types.SideBuy,
		Level:         0,
		Price:         100,
		Qty:           5,
		ClientOrderID: "SYM-BUY-0-0",
	}
}

func TestSubmitIdempotentForOpenOrder(t *testing.T) {
	g := NewInMemory(4)

	r1, err := g.Submit(sampleQuote())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.Submit(sampleQuote())
	if err != nil {
		t.Fatalf("resubmit open order must be idempotent: %v", err)
	}
	if r1.ExchangeID != r2.ExchangeID {
		t.Fatalf("exchange id changed: %s vs %s", r1.ExchangeID, r2.ExchangeID)
	}
	if len(g.OpenOrders()) != 1 {
		t.Fatalf("open orders = %d, want 1", len(g.OpenOrders()))
	}
}

func TestCannotResubmitFinalOrderID(t *testing.T) {
	g := NewInMemory(4)
	q := sampleQuote()
	if _, err := g.Submit(q); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Cancel(q.ClientOrderID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Submit(q); !errors.Is(err, ErrOrderFinal) {
		t.Fatalf("resubmit cancelled id: err=%v, want ErrOrderFinal", err)
	}

	// 全新 ID 可以正常挂单。
	other := q
	other.ClientOrderID = "SYM-BUY-0-1"
	if _, err := g.Submit(other); err != nil {
		t.Fatalf("fresh id submit failed: %v", err)
	}
}

func TestCancelUnknownIsNotFound(t *testing.T) {
	g := NewInMemory(4)
	if _, err := g.Cancel("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestFillFlow(t *testing.T) {
	g := NewInMemory(4)
	q := sampleQuote()
	if _, err := g.Submit(q); err != nil {
		t.Fatal(err)
	}

	fill, err := g.SimulateFill(q.ClientOrderID, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if fill.MakerOrderID != q.ClientOrderID || fill.Qty != 5 ||
		fill.MakerSide != types.SideBuy || fill.Price != 100 {
		t.Fatalf("unexpected fill: %+v", fill)
	}

	select {
	case got := <-g.Fills():
		if got.ID != fill.ID {
			t.Fatalf("channel fill = %s, want %s", got.ID, fill.ID)
		}
	default:
		t.Fatal("fill not delivered on channel")
	}

	if len(g.OpenOrders()) != 0 {
		t.Fatalf("filled order must leave open set, got %d", len(g.OpenOrders()))
	}
	if _, err := g.SimulateFill(q.ClientOrderID, time.Unix(101, 0)); !errors.Is(err, ErrOrderFinal) {
		t.Fatalf("double fill: err=%v, want ErrOrderFinal", err)
	}
}

func TestSellFillHasSellSide(t *testing.T) {
	g := NewInMemory(4)
	q := sampleQuote()
	q.Side = types.SideSell
	q.ClientOrderID = "SYM-SELL-0-0"
	if _, err := g.Submit(q); err != nil {
		t.Fatal(err)
	}
	fill, err := g.SimulateFill(q.ClientOrderID, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if fill.MakerSide != types.SideSell {
		t.Fatalf("fill side = %v, want SELL", fill.MakerSide)
	}
}

func TestResetClearsState(t *testing.T) {
	g := NewInMemory(4)
	if _, err := g.Submit(sampleQuote()); err != nil {
		t.Fatal(err)
	}
	g.Reset()
	if len(g.OpenOrders()) != 0 {
		t.Fatalf("open after reset = %d, want 0", len(g.OpenOrders()))
	}
	// reset 后同一 ID 可再次使用。
	if _, err := g.Submit(sampleQuote()); err != nil {
		t.Fatalf("submit after reset: %v", err)
	}
}
