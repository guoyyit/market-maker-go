package engine

import "testing"

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	engine, err := NewEngine(cfg, NewSimGateway(), NewMemoryStore())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func newTestEngineWithGW(t *testing.T, cfg Config, gw Gateway) *Engine {
	t.Helper()
	engine, err := NewEngine(cfg, gw, NewMemoryStore())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func TestQuoteLifecycleAndReconcile(t *testing.T) {
	engine := newTestEngine(t, testConfig())

	// 首笔行情：挂出 2 档共 4 张订单。
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(1, 99, 101, 0))})
	if live := len(engine.Gateway().LiveOrders()); live != 4 {
		t.Fatalf("want 4 live orders, got %d", live)
	}

	// 中间价变动：价格变化的档位撤改，其余档位若同价则保留。
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(2, 100, 102, 1))})
	live := engine.Gateway().LiveOrders()
	if len(live) != 4 {
		t.Fatalf("want still 4 live orders after update, got %d", len(live))
	}
	intent := engine.Kernel().CurrentIntent()
	if intent == nil || intent.Seq != 2 {
		t.Fatalf("want intent seq 2, got %+v", intent)
	}
	// 网关在挂集合必须与意图完全一致。
	desired := engine.Kernel().LiveOrders()
	if len(desired) != len(live) {
		t.Fatal("kernel live set size mismatch")
	}
	for id := range desired {
		if _, ok := live[id]; !ok {
			t.Fatalf("gateway missing desired order %s", id)
		}
	}
}

func TestProtectionCancelsAll(t *testing.T) {
	cfg := riskTestConfig()
	engine := newTestEngine(t, cfg)
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(1, 99, 101, 0))})
	if n := len(engine.Gateway().LiveOrders()); n == 0 {
		t.Fatal("expected live quotes")
	}

	// 跳变 -> 全量撤单并进入保护。
	result := engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(2, 200, 202, 1))})
	if !engine.Kernel().Protection().Active {
		t.Fatal("expected protection active")
	}
	if n := len(engine.Gateway().LiveOrders()); n != 0 {
		t.Fatalf("all orders must be cancelled, got %d", n)
	}
	cancels := 0
	for _, action := range result.Actions {
		if _, ok := action.(CancelAction); ok {
			cancels++
		}
	}
	if cancels == 0 {
		t.Fatal("expected cancel actions on protection entry")
	}

	// 保护期间的行情不得产生新挂单。
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(3, 200, 202, 2))})
	if n := len(engine.Gateway().LiveOrders()); n != 0 {
		t.Fatalf("no new orders while protected, got %d", n)
	}

	// 连续健康行情满足恢复条件后重新报价。
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(4, 99, 101, 3))})
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(5, 99, 101, 4))}) // 仍跳变，等待新基准
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(6, 99, 101, 5))}) // 恢复基准
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(7, 99, 101, 6))}) // 第二笔健康行情
	if engine.Kernel().Protection().Active {
		t.Fatal("protection should be exited")
	}
	if n := len(engine.Gateway().LiveOrders()); n == 0 {
		t.Fatal("quotes should resume after recovery")
	}
}

func TestStaleTickAndDuplicateFillEndToEnd(t *testing.T) {
	engine := newTestEngine(t, riskTestConfig())
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(10, 99, 101, 0))})

	// 乱序旧行情：不覆盖、不重报（动作中不应出现撤改）。
	result := engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(9, 120, 122, -1))})
	if result.Accepted {
		t.Fatal("stale tick must be rejected")
	}
	if len(result.Actions) != 0 {
		t.Fatalf("stale tick must not change orders, got %d actions", len(result.Actions))
	}

	// 注入两笔相同 FillID 的成交回报：持仓只变化一次。
	fill := Fill{FillID: "x1", OrderID: "o1", Side: SideBuy, Price: 100, Quantity: 7}
	r1 := engine.ProcessOne(Envelope{Fill: &fill, WallTime: mkTick(10, 99, 101, 0).Time})
	r2 := engine.ProcessOne(Envelope{Fill: &fill, WallTime: mkTick(10, 99, 101, 0).Time})
	if !r1.Accepted || r2.Accepted {
		t.Fatalf("first fill accepted, duplicate rejected; got %v/%v", r1.Accepted, r2.Accepted)
	}
	if position := engine.Kernel().Position(); position != 7 {
		t.Fatalf("position must be 7 after duplicate fill, got %d", position)
	}
}

func TestPositionCapEndToEnd(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPosition = 5
	cfg.Levels = 1
	gw := newStubGateway()
	engine := newTestEngineWithGW(t, cfg, gw)
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(1, 99, 101, 0))})

	// 买入成交 5，达到上限后下一次重报不得再有买单。
	fill := Fill{FillID: "cap1", Side: SideBuy, Price: 98, Quantity: 5}
	engine.ProcessOne(Envelope{Fill: &fill, WallTime: mkTick(1, 99, 101, 0).Time})
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(2, 99, 101, 1))})
	for _, order := range engine.Gateway().LiveOrders() {
		if order.Side == SideBuy {
			t.Fatalf("buy order must be removed at position cap: %+v", order)
		}
	}
}

func TestHotUpdateLevelsAndSpread(t *testing.T) {
	cfg := testConfig()
	cfg.Levels = 1
	engine := newTestEngine(t, cfg)
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(1, 99, 101, 0))})
	if n := len(engine.Gateway().LiveOrders()); n != 2 {
		t.Fatalf("want 2 orders for 1 level, got %d", n)
	}

	// 热更新档位数为 3：立即按新意图对账，在挂订单变为 6 张。
	levels := 3
	if err := engine.UpdateConfig(ConfigUpdate{Levels: &levels}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n := len(engine.Gateway().LiveOrders()); n != 6 {
		t.Fatalf("want 6 orders after hot update, got %d", n)
	}

	// 非法配置被拒绝且不改变在挂订单。
	badLevels := 0
	if err := engine.UpdateConfig(ConfigUpdate{Levels: &badLevels}); err == nil {
		t.Fatal("invalid update must be rejected")
	}
	if n := len(engine.Gateway().LiveOrders()); n != 6 {
		t.Fatalf("failed update must not change orders, got %d", n)
	}
}

func ptrTick(t Tick) *Tick { return &t }

// stubGateway 只记录在挂订单，不做自动撮合，用于手工注入成交回报的场景。
type stubGateway struct {
	live map[string]Order
}

func newStubGateway() *stubGateway { return &stubGateway{live: map[string]Order{}} }

func (g *stubGateway) Place(order Order) (bool, error) {
	if _, ok := g.live[order.ClientID]; ok {
		return false, ErrDuplicateClientID
	}
	g.live[order.ClientID] = order
	return true, nil
}

func (g *stubGateway) Cancel(clientID string) (bool, error) {
	if _, ok := g.live[clientID]; !ok {
		return false, ErrOrderNotFound
	}
	delete(g.live, clientID)
	return true, nil
}

func (g *stubGateway) LiveOrders() map[string]Order {
	out := make(map[string]Order, len(g.live))
	for id, order := range g.live {
		out[id] = order
	}
	return out
}
