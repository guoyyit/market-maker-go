// Package config 定义做市引擎的运行参数及其热更新机制。
//
// 所有价格相关字段均以"最小价位"为整数单位，数量以"最小数量单位"为整数单位，
// 保证确定性回放与跨平台一致。
package config

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Config 是一份不可变的配置快照。配置更新通过整体替换快照完成，
// 避免引擎读到半更新的参数。
type Config struct {
	// Symbol 本引擎做市的标的，当前版本单引擎服务单标的。
	Symbol string

	// Levels 每侧挂单档位数（>=1）。
	Levels int

	// BaseQty 每个档位的基础挂单数量（>0）。
	BaseQty int64

	// HalfSpread 最优档相对中间价的半价差（最小价位，>0）。
	HalfSpread int64

	// LevelStep 相邻档位的价格间距（最小价位，>0）。
	LevelStep int64

	// MaxSpread 允许的最大买卖价差（最优买卖之间，最小价位，>0）。
	// 行情原始价差超过该值时视为行情异常。
	MaxSpread int64

	// MaxPosition 持仓绝对值上限（>=0）：触及后停止增加同方向敞口的报价。
	MaxPosition int64

	// InventoryTarget 目标库存。
	InventoryTarget int64

	// SkewCoeff 与 SkewDenom 定义库存倾斜：
	// 相对目标每偏离 SkewDenom 个数量单位，报价中点平移 SkewCoeff 个价位，
	// 持仓高于目标时向下平移（鼓励买入被成交的概率降低、卖出概率升高）。
	SkewCoeff int64
	SkewDenom int64

	// QtySkewCoeff 数量倾斜系数（千分位，>=0）：
	// 相对目标每偏离 1 个数量单位，每侧每档数量按系数的千分之一调整：
	//   adj   = deviation * QtySkewCoeff / 1000（全程整数运算，向 0 截断）
	//   bidQty = BaseQty - adj，askQty = BaseQty + adj
	// 持仓高于目标时买侧数量递减、卖侧数量递增；低于目标时方向相反。
	// 取 0 时两侧数量恒为 BaseQty（旧行为，旧配置零值即兼容，无需迁移）。
	// 计算结果 <= 0 的一侧停止挂单（详见 pricing 包与 docs/quoting.md）。
	QtySkewCoeff int64

	// HeartbeatTimeout 行情心跳超时阈值：距最近一帧行情超过该时长则撤单保护。
	HeartbeatTimeout time.Duration

	// MaxPriceJump 单笔中间价跳变阈值（最小价位，>0）：
	// 相邻两帧中间价变化绝对值超过该值视为异常跳变。
	MaxPriceJump int64

	// JitterTicks 每帧报价的确定性随机抖动上限（最小价位，0 表示关闭抖动）。
	JitterTicks int64

	// RNGSeed 确定性随机种子：相同种子与相同输入序列产生逐帧一致的抖动序列。
	RNGSeed int64
}

// Validate 检查配置合法性，返回具体错误。
func (c Config) Validate() error {
	if c.Symbol == "" {
		return errors.New("config: symbol must not be empty")
	}
	if c.Levels < 1 {
		return errors.New("config: levels must be >= 1")
	}
	if c.BaseQty <= 0 {
		return errors.New("config: base qty must be > 0")
	}
	if c.HalfSpread <= 0 {
		return errors.New("config: half spread must be > 0")
	}
	if c.LevelStep <= 0 {
		return errors.New("config: level step must be > 0")
	}
	if c.MaxSpread <= 0 {
		return errors.New("config: max spread must be > 0")
	}
	if c.MaxPosition < 0 {
		return errors.New("config: max position must be >= 0")
	}
	if c.SkewDenom <= 0 {
		return errors.New("config: skew denom must be > 0")
	}
	if c.SkewCoeff < 0 {
		return errors.New("config: skew coeff must be >= 0")
	}
	if c.QtySkewCoeff < 0 {
		return errors.New("config: qty skew coeff must be >= 0")
	}
	if c.HeartbeatTimeout <= 0 {
		return errors.New("config: heartbeat timeout must be > 0")
	}
	if c.MaxPriceJump <= 0 {
		return errors.New("config: max price jump must be > 0")
	}
	if c.JitterTicks < 0 {
		return errors.New("config: jitter ticks must be >= 0")
	}
	// 半价差为两侧各退的距离，因此最优买卖价差 = 2*HalfSpread，
	// 它不得超过配置允许的最大价差，否则生成的报价自身就过宽。
	if 2*c.HalfSpread > c.MaxSpread {
		return fmt.Errorf("config: 2*half spread (%d) exceeds max spread (%d)",
			2*c.HalfSpread, c.MaxSpread)
	}
	return nil
}

// Manager 以原子快照方式保存当前配置，支持并发读取与串行热更新。
type Manager struct {
	mu      sync.RWMutex
	current Config
	// updateCh 每次成功更新后写入一个信号（缓冲为1），供引擎感知并对账。
	updateCh chan struct{}
}

// NewManager 创建配置管理器并写入初始配置。
func NewManager(initial Config) *Manager {
	return &Manager{
		current:  initial,
		updateCh: make(chan struct{}, 1),
	}
}

// Current 返回当前配置快照。
func (m *Manager) Current() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Update 以整份新快照热更新配置；非法配置被拒绝并返回错误，旧配置保持不变。
func (m *Manager) Update(next Config) error {
	if err := next.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	m.current = next
	m.mu.Unlock()

	// 非阻塞发信号：缓冲为 1，若上一个信号尚未被消费则合并，
	// 引擎只需感知"配置发生过更新"，不依赖信号计数。
	select {
	case m.updateCh <- struct{}{}:
	default:
	}
	return nil
}

// Updates 返回配置更新信号通道：每次成功更新产生一个可读信号。
func (m *Manager) Updates() <-chan struct{} {
	return m.updateCh
}
