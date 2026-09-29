// Package engine 把行情、成交、配置与定时输入串行消费，
// 驱动本地网关维护与最近报价意图严格一致的两侧挂单。
//
// 关键不变式：
//   - 引擎内部只有一个处理循环（单 goroutine 状态机），
//     所有状态变更天然串行，结果仅由输入序列决定；
//   - 任何时刻对账完成后，网关在挂集合 == 最近一次意图；
//   - 风控进入 Protected 后第一件事是全量撤单，恢复前不挂任何新单；
//   - 每个状态提交点同步持久化，重启后按成交重算持仓、按意图重建挂单，
//     依赖网关的 ClientOrderID 幂等，不重复挂单。
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/guoyyit/market-maker-go/internal/book"
	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/gateway"
	"github.com/guoyyit/market-maker-go/internal/ledger"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/risk"
	"github.com/guoyyit/market-maker-go/internal/store"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// TickEvent 注入一帧行情。
type TickEvent struct {
	Tick types.Tick
}

// TradeEvent 注入一笔成交回报。
type TradeEvent struct {
	Trade types.Trade
}

// ConfigEvent 注入一份热更新配置。
type ConfigEvent struct {
	Config config.Config
}

// TimerEvent 注入一个心跳检查点（用于行情停更判定）。
type TimerEvent struct {
	Now time.Time
}

// Event 是引擎输入信封，四个具体负载之一非空。
type Event struct {
	Tick   *TickEvent
	Trade  *TradeEvent
	Config *ConfigEvent
	Timer  *TimerEvent
}

// Engine 是做市报价引擎。
type Engine struct {
	cfg    *config.Manager
	gw     gateway.Gateway
	ledger *ledger.Ledger
	risk   *risk.Monitor
	pricer *pricing.Generator
	rec    *book.Reconciler
	store  *store.Store

	// 输入与输出通道。
	in  chan Event
	out chan Observation

	// 序号水位：旧序号输入不覆盖新状态。
	lastTickSeq  uint64
	lastTradeSeq uint64

	// 最近一帧有效行情（定价与风控使用）。
	lastTick types.Tick
	haveTick bool

	// pendingTransitions 暂存当前事件触发的风控流转，观测后清空。
	pendingTransitions []risk.Transition

	symbol string
	now    func() time.Time
}

// Observation 是引擎对外暴露的处理结果（测试/回放断言与审计使用），
// 每处理一个输入事件产出一条，序列在相同输入下逐条确定。
type Observation struct {
	Time       time.Time
	Position   int64
	Risk       risk.State
	OpenQuotes []types.Quote
	// Skipped 标记该事件是否因乱序/重复被确定性忽略。
	Skipped bool
	// ConfigRejected 标记配置事件因校验失败被拒绝（旧配置保持生效）。
	ConfigRejected bool
	// RejectReason 为配置被拒绝的原因。
	RejectReason string
	// Transitions 记录本事件引发的风控状态流转（通常为空或一条）。
	Transitions []risk.Transition
}

// Options 是引擎构造参数。
type Options struct {
	Symbol   string
	Initial  config.Config
	Gateway  gateway.Gateway
	StoreDir string
	// Now 注入时钟；nil 用真实时钟。回放时注入虚拟时钟保证确定性。
	Now func() time.Time
}

// New 创建引擎实例（不启动循环）。
func New(opts Options) (*Engine, error) {
	if err := opts.Initial.Validate(); err != nil {
		return nil, err
	}
	if opts.Gateway == nil {
		return nil, errors.New("engine: gateway is required")
	}
	if opts.StoreDir == "" {
		return nil, errors.New("engine: store dir is required")
	}

	store, err := store.New(opts.StoreDir)
	if err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	e := &Engine{
		cfg:    config.NewManager(opts.Initial),
		gw:     opts.Gateway,
		ledger: ledger.New(),
		risk:   risk.NewMonitor(riskParams(opts.Initial)),
		pricer: pricing.NewGenerator(opts.Initial),
		rec:    book.NewReconciler(),
		store:  store,
		in:     make(chan Event, 1024),
		out:    make(chan Observation, 1024),
		symbol: opts.Initial.Symbol,
		now:    now,
	}
	return e, nil
}

// Recover 从持久化状态恢复：重算持仓、重建风控状态与意图对账基准，
// 并把恢复后的意图同步到网关（依赖幂等，不产生重复挂单）。
func (e *Engine) Recover(ctx context.Context) error {
	st, err := e.store.Load()
	if errors.Is(err, store.ErrNoSnapshot) {
		// 首次启动：无状态可恢复。
		return nil
	}
	if err != nil {
		return err
	}

	// 1) 持仓以成交集合重算为唯一事实来源；快照中的冗余持仓仅做一致性校验。
	recomputed, err := e.ledger.Rebuild(st.Trades)
	if err != nil {
		return fmt.Errorf("engine: rebuild ledger: %w", err)
	}
	if st.Position != 0 || recomputed != 0 {
		if st.Position != recomputed {
			// 记录不一致但不致命：以重算值为准，避免脏持仓驱动报价。
			fmt.Fprintf(os.Stderr,
				"engine: WARNING persisted position %d != recomputed %d, using recomputed\n",
				st.Position, recomputed)
		}
	}

	// 2) 恢复序号水位与有效行情基准。
	e.lastTickSeq = st.LastTickSeq
	e.lastTradeSeq = st.LastTradeSeq
	// 对齐本地网关的成交自增序号，防止重启后新成交复用历史成交 ID。
	if seeder, ok := e.gw.(gateway.FillSeqSeeder); ok {
		seeder.SeedFillSeq(st.LastTradeSeq)
	}
	if st.LastTick != nil {
		e.lastTick = *st.LastTick
		e.haveTick = true
	}

	// 2a) 恢复最近一次生效配置（热更新结果跨重启保留）。
	if st.Config.Symbol != "" {
		if err := st.Config.Validate(); err != nil {
			return fmt.Errorf("engine: persisted config invalid: %w", err)
		}
		e.cfgPoke(st.Config)
	}

	// 3) 恢复风控状态与审计历史。
	e.risk.Restore(st.Risk)
	if transitions, terr := e.store.ReadTransitions(); terr == nil {
		e.risk.LoadTransitions(transitions)
	} else {
		return terr
	}

	// 4) 恢复报价生成器状态（档位代号与 RNG 计数）。
	e.pricer.Restore(st.Pricer)

	// 5) 恢复时若处于保护状态：以全量撤单为第一动作，且不重建任何报价。
	if e.risk.State() == risk.Protected {
		if err := e.cancelAll(ctx); err != nil {
			return err
		}
		e.rec.Clear()
	} else if st.LastIntent != nil && e.haveTick {
		// 正常状态下以"最后有效行情 + 重算持仓"重新生成意图，再与网关现状对账。
		// 档位/价格/数量由确定规则重放得到，能正确剔除已成交档位的旧终态 ID；
		// ClientOrderID 幂等保证重启后重复 Submit 同一在挂 ID 不产生重复挂单。
		quotes := e.pricer.Generate(e.lastTick, recomputed)
		open := e.openQuoteMap()
		openIDs := map[string]bool{}
		for id := range open {
			openIDs[id] = true
		}
		// 恢复模式只按价格指纹决定是否换号，不因网关为空而凭空换代号。
		e.pricer.RotateChanged(quotes, openIDs, true)

		intent := types.Intent{
			Symbol: e.symbol,
			Quotes: quotes,
			Seq:    e.lastTickSeq,
			Time:   e.lastTick.Time,
		}
		actions := e.rec.Diff(open, intent)
		if err := e.applyActions(ctx, actions); err != nil {
			return fmt.Errorf("engine: recover reconcile: %w", err)
		}
		e.rec.SetLastIntent(intent)
		e.pricer.MarkPosted(quotes)
	}

	// 7) 以重算后的真实状态再落一次盘，收敛任何历史不一致。
	return e.persist()
}

// Run 启动单处理循环直到 ctx 取消并完成排空。
func (e *Engine) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			// 关闭语义：取消后不再处理新事件；调用方可在关闭前显式全撤。
			return ctx.Err()
		case ev, ok := <-e.in:
			if !ok {
				return nil
			}
			obs, err := e.process(ctx, ev)
			if err != nil {
				return fmt.Errorf("engine: process event: %w", err)
			}
			e.out <- obs
		}
	}
}

// Input 返回引擎输入通道。
func (e *Engine) Input() chan<- Event {
	return e.in
}

// Observations 返回引擎处理结果通道（每处理一个输入事件产出一条）。
func (e *Engine) Observations() <-chan Observation {
	return e.out
}

// Snapshot 返回当前引擎状态的只读摘要（测试与持久化使用）。
func (e *Engine) Snapshot() SnapshotView {
	return SnapshotView{
		Position:     e.ledger.Position(),
		RiskState:    e.risk.State(),
		LastTickSeq:  e.lastTickSeq,
		LastTradeSeq: e.lastTradeSeq,
		OpenCount:    len(e.gw.OpenOrders()),
	}
}

func riskParams(c config.Config) risk.Params {
	return risk.Params{
		HeartbeatTimeout: c.HeartbeatTimeout,
		MaxPriceJump:     c.MaxPriceJump,
		MaxSpread:        c.MaxSpread,
	}
}

// SnapshotView 是引擎状态摘要。
type SnapshotView struct {
	Position     int64
	RiskState    risk.State
	LastTickSeq  uint64
	LastTradeSeq uint64
	OpenCount    int
}
