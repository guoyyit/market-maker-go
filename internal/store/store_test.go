package store

import (
	"errors"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/risk"
	"github.com/guoyyit/market-maker-go/internal/types"
)

func sampleState() State {
	intent := &types.Intent{
		Symbol: "SYM",
		Seq:    9,
		Time:   time.Unix(123, 456),
		Quotes: []types.Quote{{
			Symbol: "SYM", Side: types.SideBuy, Level: 0,
			Price: 100, Qty: 3, ClientOrderID: "SYM-BUY-0-0",
		}},
	}
	tick := &types.Tick{Symbol: "SYM", BidPrice: 99, AskPrice: 101, Seq: 9}
	return State{
		Symbol:     "SYM",
		Position:   4,
		LastIntent: intent,
		LastTick:   tick,
		Risk:       risk.Snapshot{State: risk.Protected, LastTickSeq: 9, HaveTick: true},
		Trades: []types.Trade{{
			ID: "t1", MakerOrderID: "SYM-BUY-0-0",
			MakerSide: types.SideBuy, Price: 100, Qty: 4, Seq: 1,
		}},
		LastTickSeq:  9,
		LastTradeSeq: 1,
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := sampleState()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Symbol != "SYM" || got.Position != 4 ||
		got.LastTickSeq != 9 || got.LastTradeSeq != 1 {
		t.Fatalf("scalar fields mismatch: %+v", got)
	}
	if got.Risk.State != risk.Protected {
		t.Fatalf("risk state = %v, want Protected", got.Risk.State)
	}
	if got.LastIntent == nil || len(got.LastIntent.Quotes) != 1 ||
		got.LastIntent.Quotes[0].ClientOrderID != "SYM-BUY-0-0" {
		t.Fatalf("intent mismatch: %+v", got.LastIntent)
	}
	if got.LastTick == nil || got.LastTick.BidPrice != 99 {
		t.Fatalf("last tick mismatch: %+v", got.LastTick)
	}
	if len(got.Trades) != 1 || got.Trades[0].ID != "t1" ||
		got.Trades[0].MakerOrderID != "SYM-BUY-0-0" {
		t.Fatalf("trades mismatch: %+v", got.Trades)
	}
}

func TestLoadWithoutSnapshotReturnsSentinel(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("err = %v, want ErrNoSnapshot", err)
	}
}

func TestRepeatedSaveOverwritesAtomically(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 5; i++ {
		st := sampleState()
		st.Position = i
		if err := s.Save(st); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Position != 5 {
		t.Fatalf("position = %d, want 5", got.Position)
	}
}

func TestAuditLogAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(10, 0)
	entries := []risk.Transition{
		{From: risk.Normal, To: risk.Protected, Reason: risk.ReasonHeartbeat, TickSeq: 3, Time: at},
		{From: risk.Protected, To: risk.Normal, Reason: risk.ReasonRecovered, TickSeq: 4, Time: at.Add(time.Second)},
	}
	for _, entry := range entries {
		if err := s.AppendTransition(entry); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ReadTransitions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].Reason != risk.ReasonHeartbeat || got[1].Reason != risk.ReasonRecovered {
		t.Fatalf("order/content mismatch: %+v", got)
	}
	if got[0].TickSeq != 3 || !got[1].Time.Equal(at.Add(time.Second)) {
		t.Fatalf("fields mismatch: %+v", got)
	}

	// 重新打开同一目录，日志继续可读（只增不改）。
	same, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := same.ReadTransitions()
	if err != nil || len(again) != 2 {
		t.Fatalf("reopen read: n=%d err=%v", len(again), err)
	}
}
