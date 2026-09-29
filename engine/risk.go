package engine

import (
	"fmt"
	"time"
)

// RiskMonitor 维护行情保护状态：心跳停更与单笔价格异常跳变。
// 全部判定只依赖输入事件的时间戳与序号，不读墙钟，因此可确定性回放。
type RiskMonitor struct {
	cfg          Config
	protection   Protection
	lastTick     *Tick
	lastMid      int64
	haveMid      bool
	healthyRun   int
	now          time.Time
	pendingAudit AuditEvent
	// needBaseline 在进入保护后置位：下一笔行情作为恢复基准，不做跳变检查。
	needBaseline bool
}

// NewRiskMonitor 构造风险监控器。
func NewRiskMonitor(cfg Config) *RiskMonitor {
	return &RiskMonitor{cfg: cfg}
}

// UpdateConfig 热更新风控参数。
// 处于保护中时缩窄 RecoveryTicks 不立即退出，仍需满足新条件后由后续行情驱动退出。
func (m *RiskMonitor) UpdateConfig(cfg Config) { m.cfg = cfg }

// ObserveTick 接收一笔行情。
// 乱序/重复行情（Seq 不大于已接受的最近 Seq，Seq>0 时生效）直接忽略，不改变任何状态。
// accepted 为 false 表示该行情被丢弃；changed 表示保护状态发生进入/退出切换。
func (m *RiskMonitor) ObserveTick(tick Tick) (accepted, changed bool, audits []AuditEvent) {
	if tick.Seq > 0 && m.lastTick != nil && tick.Seq <= m.lastTick.Seq {
		return false, false, []AuditEvent{{
			Time:   tick.Time,
			Kind:   AuditTickIgnored,
			Detail: fmt.Sprintf("stale tick seq=%d <= last=%d", tick.Seq, m.lastTick.Seq),
		}}
	}

	mid := MidPrice(&tick)
	m.now = tick.Time

	if m.protection.Active {
		changed = m.observeWhileProtected(tick, mid)
		return true, changed, m.flushAuditsLocked(tick.Time)
	}

	if m.haveMid && m.cfg.MaxJumpTicks > 0 {
		jump := mid - m.lastMid
		if jump < 0 {
			jump = -jump
		}
		if jump > m.cfg.MaxJumpTicks {
			m.enterProtection(tick.Time, fmt.Sprintf(
				"price jump %d ticks > max %d (mid %d -> %d)", jump, m.cfg.MaxJumpTicks, m.lastMid, mid))
			tickCopy := tick
			m.lastTick = &tickCopy
			m.lastMid = mid
			m.haveMid = true
			return true, true, m.flushAuditsLocked(tick.Time)
		}
	}

	tickCopy := tick
	m.lastTick = &tickCopy
	m.lastMid = mid
	m.haveMid = true
	return true, false, nil
}

func (m *RiskMonitor) observeWhileProtected(tick Tick, mid int64) bool {
	prevMid := m.lastMid
	tickCopy := tick
	m.lastTick = &tickCopy
	m.lastMid = mid
	m.haveMid = true

	// 进入保护后的第一笔行情是恢复基准：仅重置基线，不做跳变检查，
	// 否则“恢复行情相对触发跳变的末笔价格”会被永远判为再次跳变。
	if m.needBaseline {
		m.needBaseline = false
		m.healthyRun = 1
		m.protection.RecoveryTicks = m.healthyRun
		if m.cfg.RecoveryTicks > 0 && m.healthyRun >= m.cfg.RecoveryTicks {
			m.exitProtection(tick.Time)
			return true
		}
		return false
	}

	// 其后的行情相对恢复基准做跳变检查；再次异常跳变则重置健康计数并等待新基准。
	jump := mid - prevMid
	if jump < 0 {
		jump = -jump
	}
	if m.cfg.MaxJumpTicks > 0 && jump > m.cfg.MaxJumpTicks {
		m.healthyRun = 0
		m.needBaseline = true
		return false
	}
	m.healthyRun++
	m.protection.RecoveryTicks = m.healthyRun
	if m.cfg.RecoveryTicks > 0 && m.healthyRun >= m.cfg.RecoveryTicks {
		m.exitProtection(tick.Time)
		return true
	}
	return false
}

// ObserveTime 推进事件时钟，检测心跳超时。
// now 早于已观察时间时忽略（乱序时钟不得触发保护）。
func (m *RiskMonitor) ObserveTime(now time.Time) (changed bool, audits []AuditEvent) {
	if now.Before(m.now) {
		return false, nil
	}
	m.now = now
	if m.protection.Active || m.lastTick == nil || m.cfg.StaleTimeout <= 0 {
		return false, nil
	}
	if now.Sub(m.lastTick.Time) > time.Duration(m.cfg.StaleTimeout) {
		m.enterProtection(now, fmt.Sprintf(
			"market data stale: %s since last tick > timeout %s",
			now.Sub(m.lastTick.Time), time.Duration(m.cfg.StaleTimeout)))
		return true, m.flushAuditsLocked(now)
	}
	return false, nil
}

func (m *RiskMonitor) enterProtection(at time.Time, reason string) {
	lastSeq := int64(0)
	if m.lastTick != nil {
		lastSeq = m.lastTick.Seq
	}
	m.protection = Protection{
		Active:      true,
		EnteredAt:   at,
		Reason:      reason,
		LastGoodSeq: lastSeq,
	}
	m.healthyRun = 0
	m.pendingAudit = AuditEvent{Time: at, Kind: AuditProtectionEntered, Detail: reason}
	m.needBaseline = true
}

func (m *RiskMonitor) exitProtection(at time.Time) {
	reason := m.protection.Reason
	m.protection = Protection{}
	m.healthyRun = 0
	m.pendingAudit = AuditEvent{
		Time:   at,
		Kind:   AuditProtectionExited,
		Detail: fmt.Sprintf("recovered from protection: %s", reason),
	}
}

// pendingAudit 暂存最近一次状态切换产生的审计事件，随下一次观测返回。
func (m *RiskMonitor) flushAuditsLocked(at time.Time) []AuditEvent {
	if m.pendingAudit.Kind == "" {
		return nil
	}
	ev := m.pendingAudit
	m.pendingAudit = AuditEvent{}
	return []AuditEvent{ev}
}

// Protection 返回当前保护状态（副本）。
func (m *RiskMonitor) Protection() Protection { return m.protection }

// LastTick 返回最近接受的行情（副本指针），无则 nil。
func (m *RiskMonitor) LastTick() *Tick {
	if m.lastTick == nil {
		return nil
	}
	tickCopy := *m.lastTick
	return &tickCopy
}

// LastMid 返回最近有效中间价。
func (m *RiskMonitor) LastMid() (int64, bool) { return m.lastMid, m.haveMid }

// Restore 从快照恢复保护状态。
func (m *RiskMonitor) Restore(p Protection, lastTick *Tick, lastMid int64, haveMid bool) {
	m.protection = p
	m.healthyRun = p.RecoveryTicks
	m.needBaseline = p.Active && p.RecoveryTicks == 0
	m.lastTick = lastTick
	m.lastMid = lastMid
	m.haveMid = haveMid
	if lastTick != nil {
		m.now = lastTick.Time
	}
}
