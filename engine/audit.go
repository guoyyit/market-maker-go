package engine

// AuditLog 收集保护进出、输入丢弃、配置更新等审计事件，全程可追溯。
type AuditLog struct {
	events []AuditEvent
}

// NewAuditLog 构造审计日志。
func NewAuditLog() *AuditLog { return &AuditLog{} }

// Append 记录一条审计事件。
func (l *AuditLog) Append(ev AuditEvent) {
	l.events = append(l.events, ev)
}

// Events 返回按时间顺序排列的审计事件副本。
func (l *AuditLog) Events() []AuditEvent {
	out := make([]AuditEvent, len(l.events))
	copy(out, l.events)
	return out
}

// 审计事件类型常量。
const (
	AuditProtectionEntered = "protection_entered"
	AuditProtectionExited  = "protection_exited"
	AuditTickIgnored       = "tick_ignored"
	AuditFillIgnored       = "fill_ignored"
	AuditConfigUpdated     = "config_updated"
	AuditQuoteCancelAll    = "quote_cancel_all"
)
