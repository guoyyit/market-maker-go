package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func runScenarioTo(t *testing.T, dir string, events []Envelope) *Engine {
	t.Helper()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	gw := newStubGateway()
	engine, err := NewEngine(testConfig(), gw, store)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	for _, env := range events {
		engine.ProcessOne(env)
	}
	return engine
}

func TestCrashRecoveryPositionAndOrders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mm")
	events := []Envelope{
		{Tick: ptrTick(mkTick(1, 99, 101, 0)), WallTime: mkTick(1, 99, 101, 0).Time},
		{Tick: ptrTick(mkTick(2, 100, 102, 1)), WallTime: mkTick(2, 100, 102, 1).Time},
	}
	engine := runScenarioTo(t, dir, events)

	// 手工注入成交（桩网关不撮合）：持仓 +6。
	fill := Fill{FillID: "rec-1", Side: SideBuy, Quantity: 6, Time: mkTick(2, 100, 102, 1).Time}
	engine.ProcessOne(Envelope{Fill: &fill, WallTime: fill.Time})

	beforeLive := engine.Gateway().LiveOrders()
	beforePosition := engine.Kernel().Position()
	beforeIntentSeq := engine.Kernel().CurrentIntent().Seq
	if err := engine.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 模拟崩溃后重启：全新网关（空），从持久化恢复。
	store2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	gw2 := newStubGateway()
	recovered, err := RecoverEngine(gw2, store2)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	if recovered.Kernel().Position() != beforePosition {
		t.Fatalf("position mismatch: %d != %d", recovered.Kernel().Position(), beforePosition)
	}
	if recovered.Kernel().CurrentIntent().Seq != beforeIntentSeq {
		t.Fatalf("intent seq mismatch: %d != %d",
			recovered.Kernel().CurrentIntent().Seq, beforeIntentSeq)
	}
	afterLive := recovered.Gateway().LiveOrders()
	if len(afterLive) != len(beforeLive) {
		t.Fatalf("live order count mismatch: %d != %d", len(afterLive), len(beforeLive))
	}
	for id := range beforeLive {
		if _, ok := afterLive[id]; !ok {
			t.Fatalf("recovered gateway missing order %s", id)
		}
	}

	// 再恢复一次（网关已带单）：幂等，不重复挂单。
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	store3, _ := NewFileStore(dir)
	gw3 := newStubGateway()
	recovered2, err := RecoverEngine(gw3, store3)
	if err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if len(recovered2.Gateway().LiveOrders()) != len(beforeLive) {
		t.Fatalf("re-recover must not duplicate orders")
	}

	// 恢复后重复成交回报不得再次影响持仓。
	recovered2.ProcessOne(Envelope{Fill: &fill, WallTime: fill.Time})
	if position := recovered2.Kernel().Position(); position != beforePosition {
		t.Fatalf("duplicate fill after recovery changed position: %d != %d",
			position, beforePosition)
	}

	// 事件日志可重新读取：含 2 笔行情 + 2 次同一笔成交（重复的被标记忽略），
	// 快照文件确实落盘。
	logged, err := store3.ReadEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 4 {
		t.Fatalf("want 4 logged events, got %d", len(logged))
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotFile)); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
}

func TestRecoveryPositionMismatchDetected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mm")
	engine := runScenarioTo(t, dir, []Envelope{
		{Tick: ptrTick(mkTick(1, 99, 101, 0)), WallTime: mkTick(1, 99, 101, 0).Time},
	})
	fill := Fill{FillID: "tamper", Side: SideBuy, Quantity: 10, Time: mkTick(1, 99, 101, 0).Time}
	engine.ProcessOne(Envelope{Fill: &fill, WallTime: fill.Time})
	engine.Close()

	// 篡改快照持仓后恢复：必须因与成交重算不一致而拒绝。
	store2, _ := NewFileStore(dir)
	snap, err := store2.LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snap.Position = 999
	if err := store2.SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	_, err = RecoverEngine(newStubGateway(), store2)
	if !errors.Is(err, ErrPositionMismatch) {
		t.Fatalf("want ErrPositionMismatch, got %v", err)
	}
}

func TestProtectionStatePersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mm")
	store, _ := NewFileStore(dir)
	engine, err := NewEngine(riskTestConfig(), newStubGateway(), store)
	if err != nil {
		t.Fatal(err)
	}
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(1, 99, 101, 0))})
	engine.ProcessOne(Envelope{Tick: ptrTick(mkTick(2, 200, 202, 1))})
	if !engine.Kernel().Protection().Active {
		t.Fatal("expected protection")
	}
	engine.Close()

	store2, _ := NewFileStore(dir)
	recovered, err := RecoverEngine(newStubGateway(), store2)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Kernel().Protection().Active {
		t.Fatal("protection state must survive restart")
	}
	if n := len(recovered.Gateway().LiveOrders()); n != 0 {
		t.Fatalf("no orders must be live under protection after recovery, got %d", n)
	}
}
