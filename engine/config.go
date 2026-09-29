package engine

import "errors"

// Config 是做市引擎的完整配置（可整体或局部热更新）。
// 价格字段以最小变动价位为单位，数量字段以最小交易单位为单位。
type Config struct {
	Symbol string

	// 报价档位数（每侧）。
	Levels int
	// 每档基础报价数量。
	BaseQuantity int64
	// 相邻档位的价格间距。
	TickStep int64
	// 第一档相对中间价的偏移（买向下、卖向上）。
	QuoteOffset int64
	// 允许向客挂出的最大买卖价差，超过时不报价。
	MaxSpread int64

	// 库存目标持仓。
	TargetPosition int64
	// 持仓硬上限（绝对值），达到后停止同方向增加敞口的报价。
	MaxPosition int64
	// 库存倾斜系数：每偏离目标 1 单位，中间价平移的价位数。
	SkewPerUnit int64
	// 每偏离目标 1 单位，该侧数量调整的基础数量比例步长（百分比）。
	QuantitySkewPctPerUnit int64

	// 单笔中间价相对最近有效中间价的最大允许跳变（价位数），超过进入保护。
	MaxJumpTicks int64
	// 行情心跳超时（按输入事件时间戳判定）。
	StaleTimeout Duration
	// 退出保护需要连续观察到的健康行情笔数。
	RecoveryTicks int

	// 确定性报价抖动（避免同价）所用随机种子；0 表示不抖动。
	RandomSeed uint64
}

// Duration 是与 time.Duration 解耦的可持久化时长类型（纳秒）。
type Duration int64

// 常用时长，单位为纳秒。
const (
	Nanosecond  Duration = 1
	Millisecond          = 1_000_000 * Nanosecond
	Second               = 1_000 * Millisecond
)

// ConfigUpdate 描述一次热更新。nil 字段表示保持原值。
type ConfigUpdate struct {
	Levels                 *int
	BaseQuantity           *int64
	TickStep               *int64
	QuoteOffset            *int64
	MaxSpread              *int64
	TargetPosition         *int64
	MaxPosition            *int64
	SkewPerUnit            *int64
	QuantitySkewPctPerUnit *int64
	MaxJumpTicks           *int64
	StaleTimeout           *Duration
	RecoveryTicks          *int
}

// DefaultConfig 返回一份可用的默认配置。
func DefaultConfig() Config {
	return Config{
		Symbol:                 "DEMO",
		Levels:                 2,
		BaseQuantity:           10,
		TickStep:               1,
		QuoteOffset:            2,
		MaxSpread:              20,
		TargetPosition:         0,
		MaxPosition:            100,
		SkewPerUnit:            0,
		QuantitySkewPctPerUnit: 1,
		MaxJumpTicks:           50,
		StaleTimeout:           3 * Second,
		RecoveryTicks:          2,
		RandomSeed:             1,
	}
}

// Validate 校验配置合法性。除 Symbol 与数量必须为正外，不对业务取值做隐式修改。
func Validate(c Config) (Config, error) {
	if c.Symbol == "" {
		return c, errors.New("symbol must not be empty")
	}
	if c.Levels <= 0 {
		return c, errors.New("levels must be positive")
	}
	if c.BaseQuantity <= 0 {
		return c, errors.New("base quantity must be positive")
	}
	if c.TickStep <= 0 {
		return c, errors.New("tick step must be positive")
	}
	if c.QuoteOffset < 0 {
		return c, errors.New("quote offset must be non-negative")
	}
	if c.MaxSpread <= 0 {
		return c, errors.New("max spread must be positive")
	}
	if c.MaxPosition < 0 {
		return c, errors.New("max position must be non-negative")
	}
	if c.MaxJumpTicks < 0 {
		return c, errors.New("max jump ticks must be non-negative")
	}
	if c.StaleTimeout < 0 {
		return c, errors.New("stale timeout must be non-negative")
	}
	if c.RecoveryTicks < 0 {
		return c, errors.New("recovery ticks must be non-negative")
	}
	if c.QuantitySkewPctPerUnit < 0 || c.QuantitySkewPctPerUnit > 100 {
		return c, errors.New("quantity skew percent per unit must be in [0,100]")
	}
	return c, nil
}

// Apply 在拷贝上应用热更新，nil 字段保持原值，返回新配置。
// 该函数只做字段覆盖；合法性校验与在途报价生效规则由内核负责。
func (c Config) Apply(u ConfigUpdate) Config {
	next := c
	if u.Levels != nil {
		next.Levels = *u.Levels
	}
	if u.BaseQuantity != nil {
		next.BaseQuantity = *u.BaseQuantity
	}
	if u.TickStep != nil {
		next.TickStep = *u.TickStep
	}
	if u.QuoteOffset != nil {
		next.QuoteOffset = *u.QuoteOffset
	}
	if u.MaxSpread != nil {
		next.MaxSpread = *u.MaxSpread
	}
	if u.TargetPosition != nil {
		next.TargetPosition = *u.TargetPosition
	}
	if u.MaxPosition != nil {
		next.MaxPosition = *u.MaxPosition
	}
	if u.SkewPerUnit != nil {
		next.SkewPerUnit = *u.SkewPerUnit
	}
	if u.QuantitySkewPctPerUnit != nil {
		next.QuantitySkewPctPerUnit = *u.QuantitySkewPctPerUnit
	}
	if u.MaxJumpTicks != nil {
		next.MaxJumpTicks = *u.MaxJumpTicks
	}
	if u.StaleTimeout != nil {
		next.StaleTimeout = *u.StaleTimeout
	}
	if u.RecoveryTicks != nil {
		next.RecoveryTicks = *u.RecoveryTicks
	}
	return next
}
