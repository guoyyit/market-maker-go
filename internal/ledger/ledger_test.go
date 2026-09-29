package ledger

import (
	"testing"

	"github.com/guoyyit/market-maker-go/internal/types"
)

func buy(id string, qty int64, seq uint64) types.Trade {
	return types.Trade{ID: id, MakerSide: types.SideBuy, Qty: qty, Seq: seq}
}

func sell(id string, qty int64, seq uint64) types.Trade {
	return types.Trade{ID: id, MakerSide: types.SideSell, Qty: qty, Seq: seq}
}

func TestApplyBuySellAndPosition(t *testing.T) {
	l := New()

	if pos, applied, err := l.Apply(buy("t1", 10, 1)); err != nil || !applied || pos != 10 {
		t.Fatalf("buy: pos=%d applied=%v err=%v", pos, applied, err)
	}
	if pos, applied, err := l.Apply(sell("t2", 4, 2)); err != nil || !applied || pos != 6 {
		t.Fatalf("sell: pos=%d applied=%v err=%v", pos, applied, err)
	}
	if l.Position() != 6 {
		t.Fatalf("position = %d, want 6", l.Position())
	}
}

func TestDuplicateTradeIDIsIdempotent(t *testing.T) {
	l := New()
	first := buy("dup", 5, 1)
	if _, applied, _ := l.Apply(first); !applied {
		t.Fatal("first apply should be applied")
	}
	// 完全重复的回报。
	if pos, applied, err := l.Apply(first); err != nil || applied || pos != 5 {
		t.Fatalf("duplicate exact: pos=%d applied=%v err=%v", pos, applied, err)
	}
	// 即使重复回报携带更大的序号或不同数量，也不得二次影响持仓。
	replayed := buy("dup", 999, 100)
	if pos, applied, _ := l.Apply(replayed); applied || pos != 5 {
		t.Fatalf("duplicate id with new seq: pos=%d applied=%v", pos, applied)
	}
	if l.AppliedCount() != 1 {
		t.Fatalf("applied count = %d, want 1", l.AppliedCount())
	}
}

func TestOutOfOrderTradesProduceSameResult(t *testing.T) {
	seqA := []types.Trade{buy("a", 10, 1), sell("b", 3, 2), buy("c", 2, 3)}
	seqB := []types.Trade{buy("c", 2, 3), buy("a", 10, 1), sell("b", 3, 2)}

	la := New()
	lb := New()
	pa, errA := la.Rebuild(seqA)
	pb, errB := lb.Rebuild(seqB)
	if errA != nil || errB != nil {
		t.Fatalf("rebuild errors: %v %v", errA, errB)
	}
	if pa != pb || pa != 9 {
		t.Fatalf("positions differ: %d vs %d (want 9)", pa, pb)
	}

	gotA := la.Trades()
	gotB := lb.Trades()
	if len(gotA) != len(gotB) || len(gotA) != 3 {
		t.Fatalf("trade counts: %d vs %d", len(gotA), len(gotB))
	}
	for i := range gotA {
		if gotA[i].ID != gotB[i].ID || gotA[i].Seq != gotB[i].Seq {
			t.Fatalf("trade order not deterministic at %d: %+v %+v", i, gotA[i], gotB[i])
		}
	}
}

func TestRebuildDedupsWithinSequence(t *testing.T) {
	l := New()
	pos, err := l.Rebuild([]types.Trade{buy("x", 7, 1), buy("x", 7, 1), sell("y", 2, 2)})
	if err != nil {
		t.Fatal(err)
	}
	if pos != 5 {
		t.Fatalf("pos = %d, want 5", pos)
	}
	if l.AppliedCount() != 2 {
		t.Fatalf("applied = %d, want 2", l.AppliedCount())
	}
}

func TestInvalidTradeRejected(t *testing.T) {
	l := New()
	if _, _, err := l.Apply(types.Trade{ID: "", MakerSide: types.SideBuy, Qty: 1}); err == nil {
		t.Fatal("empty id should be rejected")
	}
	if _, _, err := l.Apply(buy("z", 0, 1)); err == nil {
		t.Fatal("zero qty should be rejected")
	}
	if l.Position() != 0 || l.AppliedCount() != 0 {
		t.Fatalf("invalid apply must not mutate ledger: pos=%d n=%d",
			l.Position(), l.AppliedCount())
	}
}
