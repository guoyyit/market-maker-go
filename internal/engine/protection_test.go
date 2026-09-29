package engine

import (
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/risk"
)

func TestHeartbeatTimeoutCancelsAllThenRecover(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)

	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	if h.openCount() != 4 {
		t.Fatalf("initial open = %d, want 4", h.openCount())
	}

	// 心跳超时：全量撤单并进入保护。
	obs := h.send(Event{Timer: &TimerEvent{Now: base.Add(200 * time.Millisecond)}})
	if obs.Risk != risk.Protected {
		t.Fatalf("risk after timeout = %v, want Protected", obs.Risk)
	}
	if len(obs.Transitions) != 1 || obs.Transitions[0].Reason != risk.ReasonHeartbeat {
		t.Fatalf("transitions = %+v, want one heartbeat", obs.Transitions)
	}
	if h.openCount() != 0 || len(obs.OpenQuotes) != 0 {
		t.Fatalf("protection must flatten book: gateway=%d obs=%d",
			h.openCount(), len(obs.OpenQuotes))
	}

	// 保护期间收到的正常心跳检查不会重新挂单。
	obs = h.send(Event{Timer: &TimerEvent{Now: base.Add(300 * time.Millisecond)}})
	if obs.Risk != risk.Protected || h.openCount() != 0 {
		t.Fatalf("still protected expected, risk=%v open=%d", obs.Risk, h.openCount())
	}

	// 收到一帧与基准接近的正常行情后才允许恢复并重新双侧报价。
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 99, 101, base.Add(400*time.Millisecond))}})
	if obs.Risk != risk.Normal {
		t.Fatalf("risk = %v, want Normal", obs.Risk)
	}
	if len(obs.Transitions) != 1 || obs.Transitions[0].Reason != risk.ReasonRecovered {
		t.Fatalf("recovery transition missing: %+v", obs.Transitions)
	}
	if h.openCount() != 4 {
		t.Fatalf("open after recovery = %d, want 4", h.openCount())
	}

	// 审计日志全程可查：进入与退出各一条。
	transitions, err := h.eng.storeTransitions()
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 {
		t.Fatalf("persisted transitions = %d, want 2", len(transitions))
	}
	if transitions[0].Reason != risk.ReasonHeartbeat ||
		transitions[1].Reason != risk.ReasonRecovered {
		t.Fatalf("audit order wrong: %+v", transitions)
	}
}

func TestPriceJumpCancelsAllAndAbnormalFrameIgnored(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// mid 100 -> 110，跳变 10 > 阈值 5：全撤并保护。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 109, 111, base.Add(10*time.Millisecond))}})
	if obs.Risk != risk.Protected {
		t.Fatalf("risk = %v, want Protected", obs.Risk)
	}
	if h.openCount() != 0 {
		t.Fatalf("open after jump = %d, want 0", h.openCount())
	}

	// 异常帧不得锚定：接下来接近 100 的正常帧直接恢复。
	obs = h.send(Event{Tick: &TickEvent{Tick: mkTick(3, 99, 101, base.Add(20*time.Millisecond))}})
	if obs.Risk != risk.Normal || h.openCount() != 4 {
		t.Fatalf("recovery wrong: risk=%v open=%d", obs.Risk, h.openCount())
	}
}

func TestInvalidSpreadTriggersProtection(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})

	// 交叉盘买一 >= 卖一。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 105, 103, base.Add(time.Millisecond))}})
	if obs.Risk != risk.Protected || h.openCount() != 0 {
		t.Fatalf("crossed book: risk=%v open=%d", obs.Risk, h.openCount())
	}
	if len(obs.Transitions) != 1 || obs.Transitions[0].Reason != risk.ReasonInvalidSpread {
		t.Fatalf("want invalid_spread transition, got %+v", obs.Transitions)
	}
}

func TestTicksWhileProtectedDoNotQuoteUntilRecover(t *testing.T) {
	h := newHarness(t, testConfig())
	defer h.stop()
	base := time.Unix(1_000_000, 0)
	h.send(Event{Tick: &TickEvent{Tick: mkTick(1, 99, 101, base)}})
	h.send(Event{Timer: &TimerEvent{Now: base.Add(200 * time.Millisecond)}})

	// 保护状态下再来一帧小跳变行情，继续保护、不挂单。
	obs := h.send(Event{Tick: &TickEvent{Tick: mkTick(2, 120, 122, base.Add(210*time.Millisecond))}})
	if obs.Risk != risk.Protected || h.openCount() != 0 {
		t.Fatalf("jump while protected: risk=%v open=%d", obs.Risk, h.openCount())
	}
}
