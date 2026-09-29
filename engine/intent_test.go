package engine

import (
	"context"
	"testing"
	"time"
)

func TestReconcilerIncremental(t *testing.T) {
	r := NewIntentReconciler()
	now := time.Unix(0, 0)
	intent1 := &Intent{Seq: 1, Symbol: "T", CreatedAt: now, Quotes: []Quote{
		{Side: SideBuy, Level: 1, Price: 98, Quantity: 10},
		{Side: SideSell, Level: 1, Price: 102, Quantity: 10},
	}}
	actions := r.Reconcile(intent1)
	if len(actions) != 2 {
		t.Fatalf("want 2 places, got %d", len(actions))
	}

	// 同内容同版本：幂等，无动作。
	if actions := r.Reconcile(intent1); len(actions) != 0 {
		t.Fatalf("identical intent must produce no actions, got %d", len(actions))
	}

	// 买单改价、卖单不变：只撤旧买单并挂新买单（撤改）。
	intent2 := &Intent{Seq: 2, Symbol: "T", CreatedAt: now, Quotes: []Quote{
		{Side: SideBuy, Level: 1, Price: 97, Quantity: 10},
		{Side: SideSell, Level: 1, Price: 102, Quantity: 10},
	}}
	actions = r.Reconcile(intent2)
	if len(actions) != 2 {
		t.Fatalf("want cancel+place (2 actions), got %d", len(actions))
	}
	if _, ok := actions[0].(CancelAction); !ok {
		t.Fatalf("first action must be cancel, got %T", actions[0])
	}
	if _, ok := actions[1].(PlaceAction); !ok {
		t.Fatalf("second action must be place, got %T", actions[1])
	}
	if len(r.Live()) != 2 {
		t.Fatalf("want 2 live orders, got %d", len(r.Live()))
	}

	// 全撤。
	if actions := r.CancelAll(); len(actions) != 2 || len(r.Live()) != 0 {
		t.Fatalf("cancel all must remove 2 orders")
	}
}

func TestClientIDDeterministic(t *testing.T) {
	id1 := QuoteClientID("T", 7, SideBuy, 2)
	id2 := QuoteClientID("T", 7, SideBuy, 2)
	if id1 != id2 || id1 != "T-7-buy-L2" {
		t.Fatalf("unexpected client id: %q", id1)
	}
}

func TestFeedRunLoop(t *testing.T) {
	engine := newTestEngine(t, testConfig())
	feed := NewFeed(4)
	feed.PostTick(mkTick(1, 99, 101, 0))
	feed.PostTick(mkTick(2, 100, 102, 1))

	done := make(chan struct{})
	go func() {
		_ = engine.Run(context.Background(), feed)
		close(done)
	}()

	// 两笔行情已在缓冲通道中；关闭 feed 后 Run 排空事件并返回。
	feed.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not drain feed")
	}

	// 事件循环已结束（Close 后 Run 返回），此时读取状态无并发。
	if seq := engine.Kernel().CurrentIntent().Seq; seq != 2 {
		t.Fatalf("want intent seq 2, got %d", seq)
	}
}
