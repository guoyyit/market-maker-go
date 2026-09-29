package engine

import (
	"testing"
	"time"
)

func riskTestConfig() Config {
	cfg := testConfig()
	cfg.MaxJumpTicks = 10
	cfg.StaleTimeout = 3 * Second
	cfg.RecoveryTicks = 2
	return cfg
}

func TestPriceJumpEntersProtection(t *testing.T) {
	m := NewRiskMonitor(riskTestConfig())

	if accepted, changed, _ := m.ObserveTick(mkTick(1, 99, 101, 0)); changed || !accepted {
		t.Fatal("first tick should be accepted without protection")
	}
	// 中间价从 100 跳到 120（20 > 10 阈值）。
	accepted, changed, audits := m.ObserveTick(mkTick(2, 119, 121, 1))
	if !accepted || !changed || !m.Protection().Active {
		t.Fatalf("jump tick must enter protection: accepted=%v changed=%v", accepted, changed)
	}
	if len(audits) != 1 || audits[0].Kind != AuditProtectionEntered {
		t.Fatalf("want protection_entered audit, got %+v", audits)
	}
}

func TestStaleTimeoutEntersProtection(t *testing.T) {
	m := NewRiskMonitor(riskTestConfig())
	m.ObserveTick(mkTick(1, 99, 101, 0))

	changed, audits := m.ObserveTime(time.Unix(1004, 0))
	if !changed || !m.Protection().Active {
		t.Fatal("heartbeat timeout must enter protection")
	}
	if len(audits) != 1 || audits[0].Kind != AuditProtectionEntered {
		t.Fatalf("want protection_entered audit, got %+v", audits)
	}

	// 保护期间再来超时时钟不得重复产生事件。
	if changed, _ := m.ObserveTime(time.Unix(1005, 0)); changed {
		t.Fatal("must not re-enter protection")
	}
}

func TestRecoveryTicksExitProtection(t *testing.T) {
	m := NewRiskMonitor(riskTestConfig())
	m.ObserveTick(mkTick(1, 99, 101, 0))
	m.ObserveTick(mkTick(2, 200, 202, 1)) // 跳变进入保护

	// 需要连续 2 笔健康行情才能退出。
	accepted, changed, _ := m.ObserveTick(mkTick(3, 99, 101, 2))
	if !accepted || changed {
		t.Fatal("first healthy tick must not exit protection yet")
	}
	accepted, changed, audits := m.ObserveTick(mkTick(4, 99, 101, 3))
	if !accepted || !changed || m.Protection().Active {
		t.Fatalf("second healthy tick must exit protection: changed=%v active=%v",
			changed, m.Protection().Active)
	}
	if len(audits) != 1 || audits[0].Kind != AuditProtectionExited {
		t.Fatalf("want protection_exited audit, got %+v", audits)
	}
}

func TestStaleTickIgnoredDoesNotOverwrite(t *testing.T) {
	m := NewRiskMonitor(riskTestConfig())
	m.ObserveTick(mkTick(10, 99, 101, 5))

	accepted, changed, audits := m.ObserveTick(mkTick(9, 200, 202, 4))
	if accepted || changed {
		t.Fatal("late tick must be ignored")
	}
	if len(audits) != 1 || audits[0].Kind != AuditTickIgnored {
		t.Fatalf("want tick_ignored audit, got %+v", audits)
	}
	if m.Protection().Active {
		t.Fatal("late tick must not trigger jump protection")
	}
	last := m.LastTick()
	if last == nil || last.Seq != 10 {
		t.Fatalf("last tick must remain seq=10, got %+v", last)
	}

	// 重复行情同样忽略。
	if accepted, _, _ := m.ObserveTick(mkTick(10, 99, 101, 5)); accepted {
		t.Fatal("duplicate tick must be ignored")
	}
}
