package risk

import (
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/types"
)

func testParams() Params {
	return Params{
		HeartbeatTimeout: 100 * time.Millisecond,
		MaxPriceJump:     5,
		MaxSpread:        10,
	}
}

func tick(seq uint64, bid, ask int64, at time.Time) types.Tick {
	return types.Tick{Seq: seq, BidPrice: bid, AskPrice: ask, Time: at}
}

func TestNormalTicksKeepNormal(t *testing.T) {
	m := NewMonitor(testParams())
	base := time.Unix(1000, 0)

	state, tr := m.OnTick(tick(1, 99, 101, base))
	if state != Normal || tr != nil {
		t.Fatalf("first tick: state=%v tr=%v", state, tr)
	}
	state, tr = m.OnTick(tick(2, 100, 102, base.Add(10*time.Millisecond)))
	if state != Normal || tr != nil {
		t.Fatalf("second tick within jump: state=%v tr=%v", state, tr)
	}
}

func TestPriceJumpEntersProtectionAndDoesNotAnchor(t *testing.T) {
	m := NewMonitor(testParams())
	base := time.Unix(1000, 0)
	m.OnTick(tick(1, 99, 101, base))

	// 中间价从 100 跳到 110（跳变 10 > 5）-> 保护。
	state, tr := m.OnTick(tick(2, 109, 111, base.Add(10*time.Millisecond)))
	if state != Protected || tr == nil || tr.Reason != ReasonPriceJump {
		t.Fatalf("jump: state=%v tr=%+v", state, tr)
	}

	// 异常帧不得成为新基准：随后与 100 接近的正常帧应触发恢复，
	// 而不是相对 110 再被判一次跳变。
	state, tr = m.OnTick(tick(3, 99, 101, base.Add(20*time.Millisecond)))
	if state != Normal || tr == nil || tr.Reason != ReasonRecovered {
		t.Fatalf("recovery: state=%v tr=%+v", state, tr)
	}

	transitions := m.Transitions()
	if len(transitions) != 2 {
		t.Fatalf("transitions = %d, want 2", len(transitions))
	}
	if transitions[0].From != Normal || transitions[0].To != Protected {
		t.Fatalf("first transition = %+v", transitions[0])
	}
	if transitions[1].From != Protected || transitions[1].To != Normal {
		t.Fatalf("second transition = %+v", transitions[1])
	}
}

func TestHeartbeatTimeoutEntersProtection(t *testing.T) {
	m := NewMonitor(testParams())
	base := time.Unix(1000, 0)
	m.OnTick(tick(1, 99, 101, base))

	now := base.Add(50 * time.Millisecond)
	if state, tr := m.CheckHeartbeat(now); state != Normal || tr != nil {
		t.Fatalf("within timeout: state=%v tr=%v", state, tr)
	}

	now = base.Add(150 * time.Millisecond)
	state, tr := m.CheckHeartbeat(now)
	if state != Protected || tr == nil || tr.Reason != ReasonHeartbeat {
		t.Fatalf("timeout: state=%v tr=%+v", state, tr)
	}

	// 保护状态下重复心跳检查不再产生流转记录。
	if _, tr := m.CheckHeartbeat(now.Add(10 * time.Second)); tr != nil {
		t.Fatalf("repeated heartbeat in protection should be silent, got %+v", tr)
	}
}

func TestInvalidSpreadEntersProtection(t *testing.T) {
	m := NewMonitor(testParams())
	base := time.Unix(1000, 0)
	m.OnTick(tick(1, 99, 101, base))

	// 价差超过上限。
	state, tr := m.OnTick(tick(2, 90, 120, base.Add(time.Millisecond)))
	if state != Protected || tr == nil || tr.Reason != ReasonInvalidSpread {
		t.Fatalf("wide spread: state=%v tr=%+v", state, tr)
	}

	// 交叉盘（bid >= ask）。
	m.OnTick(tick(3, 99, 101, base.Add(2*time.Millisecond)))
	state, tr = m.OnTick(tick(4, 105, 103, base.Add(3*time.Millisecond)))
	if state != Protected || tr == nil || tr.Reason != ReasonInvalidSpread {
		t.Fatalf("crossed spread: state=%v tr=%+v", state, tr)
	}
}

func TestExportRestoreRoundTrip(t *testing.T) {
	m := NewMonitor(testParams())
	base := time.Unix(1000, 0)
	m.OnTick(tick(1, 99, 101, base))
	snap := m.Export()

	m2 := NewMonitor(testParams())
	m2.Restore(snap)
	if m2.State() != Normal {
		t.Fatalf("restored state = %v, want Normal", m2.State())
	}

	// 恢复后基准为 mid=100：跳 +6 必须被识别。
	state, tr := m2.OnTick(tick(2, 105, 107, base.Add(time.Millisecond)))
	if state != Protected || tr == nil {
		t.Fatalf("post-restore jump not detected: state=%v tr=%v", state, tr)
	}
}
