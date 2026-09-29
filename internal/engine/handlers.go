package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/guoyyit/market-maker-go/internal/book"
	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/gateway"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/risk"
	"github.com/guoyyit/market-maker-go/internal/store"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// process 是单循环内唯一的事件入口，返回该事件处理后的确定性观测。
func (e *Engine) process(ctx context.Context, ev Event) (Observation, error) {
	var err error
	obs := Observation{Time: e.now()}

	switch {
	case ev.Tick != nil:
		obs.Skipped, err = e.handleTick(ctx, ev.Tick)
	case ev.Trade != nil:
		obs.Skipped, err = e.handleTrade(ctx, ev.Trade)
	case ev.Config != nil:
		var rejected string
		rejected, err = e.handleConfig(ctx, ev.Config)
		if rejected != "" {
			obs.ConfigRejected = true
			obs.RejectReason = rejected
		}
	case ev.Timer != nil:
		err = e.handleTimer(ctx, ev.Timer)
	default:
		err = errors.New("engine: empty event")
	}
	if err != nil {
		return Observation{}, err
	}

	obs.Position = e.ledger.Position()
	obs.Risk = e.risk.State()
	obs.Transitions = e.takeTransitionObs()
	obs.OpenQuotes = e.openQuoteSnapshot()
	return obs, nil
}

// transitionAcc 暂存当前事件触发、尚未对外暴露的风控流转记录。
// 与 risk.Monitor 自身的全量审计不同，这里只收集本事件新增的记录。
func (e *Engine) takeTransitionObs() []risk.Transition {
	if len(e.pendingTransitions) == 0 {
		return nil
	}
	out := e.pendingTransitions
	e.pendingTransitions = nil
	return out
}

// handleTick 处理一帧行情：
// 乱序/重复过滤 -> 风控判定 -> 进入保护则全撤 / 恢复或正常则重新报价 -> 持久化。
// 返回 skipped=true 表示该帧因序号过旧被确定性忽略。
func (e *Engine) handleTick(ctx context.Context, ev *TickEvent) (bool, error) {
	tick := ev.Tick
	if tick.Seq <= e.lastTickSeq {
		// 重复或迟到的旧行情：绝不覆盖已推进的状态。
		return true, nil
	}
	e.lastTickSeq = tick.Seq

	state, tr := e.risk.OnTick(tick)
	if tr != nil {
		if err := e.recordTransition(ctx, *tr); err != nil {
			return false, err
		}
	}

	if state == risk.Protected {
		if err := e.cancelAll(ctx); err != nil {
			return false, err
		}
		e.rec.Clear()
		// 异常帧不更新 lastTick/havenTick 基准（risk 包内部已按此语义处理）。
		if err := e.persist(); err != nil {
			return false, err
		}
		return false, nil
	}

	// 正常帧：更新有效行情基准并按最新状态重新生成意图。
	e.lastTick = tick
	e.haveTick = true
	if err := e.requote(ctx); err != nil {
		return false, err
	}
	if err := e.persist(); err != nil {
		return false, err
	}
	return false, nil
}

// handleTrade 处理一笔成交回报：
// 重复 ID / 旧序号过滤 -> 账本去重入账 -> 档位代号轮换 ->
// 重新生成意图并对账 -> 持久化。
func (e *Engine) handleTrade(ctx context.Context, ev *TradeEvent) (bool, error) {
	tr := ev.Trade
	if e.ledger.Has(tr.ID) {
		return true, nil
	}
	// 乱序容错：序号水位只接受更新的回报。
	// 注意 ID 去重先于序号判断，因此"迟到的重复回报"在任何序号下都被忽略。
	if tr.Seq <= e.lastTradeSeq {
		return true, nil
	}

	position, applied, err := e.ledger.Apply(tr)
	if err != nil {
		return false, err
	}
	if !applied {
		return true, nil
	}
	e.lastTradeSeq = tr.Seq

	// 轮换被成交档位的代号，使重新挂单使用全新 ClientOrderID。
	if _, side, level, _, perr := pricing.ParseClientOrderID(tr.MakerOrderID); perr == nil {
		e.pricer.AdvanceFill(side, level)
	}

	// 成交后必须立即重新对账：旧单已处终态，网关在挂集合中已缺该档，
	// requote 会以新代号补挂并套用最新库存倾斜/上限裁剪。
	if e.risk.State() == risk.Normal && e.haveTick {
		if err := e.requoteWith(ctx, position); err != nil {
			return false, err
		}
	}

	if err := e.persist(); err != nil {
		return false, err
	}
	return false, nil
}

// handleConfig 热更新配置：非法配置被确定性拒绝（不影响在挂报价），
// 返回非空拒绝原因；合法配置原子替换后立即按新参数对账。
func (e *Engine) handleConfig(ctx context.Context, ev *ConfigEvent) (string, error) {
	next := ev.Config
	if err := next.Validate(); err != nil {
		return err.Error(), nil
	}
	e.cfgPoke(next)

	if e.risk.State() == risk.Normal && e.haveTick {
		if err := e.requote(ctx); err != nil {
			return "", err
		}
	}
	return "", e.persist()
}

// handleTimer 心跳检查：超时则全撤并进入保护状态。
func (e *Engine) handleTimer(ctx context.Context, ev *TimerEvent) error {
	state, tr := e.risk.CheckHeartbeat(ev.Now)
	if tr == nil {
		return nil
	}
	if err := e.recordTransition(ctx, *tr); err != nil {
		return err
	}
	if state == risk.Protected {
		if err := e.cancelAll(ctx); err != nil {
			return err
		}
		e.rec.Clear()
		return e.persist()
	}
	return nil
}

// cfgPoke 把新配置同步到配置管理器、定价器与风控参数。
// 生效语义：立即对在挂报价做一次全量对账；RNG 种子不重置（抖动流连续）。
func (e *Engine) cfgPoke(next config.Config) {
	_ = e.cfg.Update(next)
	e.pricer.SetConfig(next)
	e.risk.SetParams(riskParams(next))
}

// requote 以最新持仓生成意图并增量对账。
func (e *Engine) requote(ctx context.Context) error {
	return e.requoteWith(ctx, e.ledger.Position())
}

// requoteWith 以指定持仓（调用方刚从账本获得，避免重复读取）生成意图。
func (e *Engine) requoteWith(ctx context.Context, position int64) error {
	quotes := e.pricer.Generate(e.lastTick, position)
	open := e.openQuoteMap()
	openIDs := make(map[string]bool, len(open))
	for id := range open {
		openIDs[id] = true
	}
	e.pricer.RotateChanged(quotes, openIDs, false)

	intent := types.Intent{
		Symbol: e.symbol,
		Quotes: quotes,
		Seq:    e.lastTickSeq,
		Time:   e.lastTick.Time,
	}

	actions := e.rec.Diff(open, intent)
	if err := e.applyActions(ctx, actions); err != nil {
		return err
	}
	e.rec.SetLastIntent(intent)
	e.pricer.MarkPosted(quotes)
	return nil
}

// applyActions 按确定性顺序（先撤后挂，各自按 ID 升序）提交动作。
func (e *Engine) applyActions(ctx context.Context, actions []book.Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, action := range actions {
		switch action.Kind {
		case book.ActionCancel:
			_, err := e.gw.Cancel(action.Quote.ClientOrderID)
			if err != nil && !errors.Is(err, gateway.ErrNotFound) {
				return fmt.Errorf("engine: cancel %s: %w",
					action.Quote.ClientOrderID, err)
			}
		case book.ActionPlace:
			if _, err := e.gw.Submit(action.Quote); err != nil {
				return fmt.Errorf("engine: submit %s: %w",
					action.Quote.ClientOrderID, err)
			}
		default:
			return fmt.Errorf("engine: unknown action kind %d", action.Kind)
		}
	}
	return nil
}

// cancelAll 撤销网关全部在挂单（进入保护时调用）。
func (e *Engine) cancelAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	open := e.openQuoteMap()
	return e.applyActions(ctx, book.CancelAll(open))
}

// recordTransition 记录一条风控流转：内存暂存（供观测）并追加审计日志。
func (e *Engine) recordTransition(ctx context.Context, tr risk.Transition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.pendingTransitions = append(e.pendingTransitions, tr)
	return e.store.AppendTransition(tr)
}

// openQuoteMap 把网关在挂订单转换为报价形式的快照。
func (e *Engine) openQuoteMap() map[string]types.Quote {
	open := e.gw.OpenOrders()
	out := make(map[string]types.Quote, len(open))
	for id, report := range open {
		_, _, level, _, err := pricing.ParseClientOrderID(id)
		if err != nil {
			level = 0
		}
		out[id] = types.Quote{
			Symbol:        report.Symbol,
			Side:          report.Side,
			Level:         level,
			Price:         report.Price,
			Qty:           report.Qty,
			ClientOrderID: id,
		}
	}
	return out
}

// openQuoteSnapshot 返回在挂报价的确定性（ID 升序）副本。
func (e *Engine) openQuoteSnapshot() []types.Quote {
	open := e.openQuoteMap()
	ids := make([]string, 0, len(open))
	for id := range open {
		ids = append(ids, id)
	}
	sortStrings(ids)
	out := make([]types.Quote, 0, len(ids))
	for _, id := range ids {
		out = append(out, open[id])
	}
	return out
}

// persist 把当前引擎状态写入原子快照。
func (e *Engine) persist() error {
	st := store.State{
		Symbol:       e.symbol,
		Config:       e.cfg.Current(),
		Trades:       e.ledger.Trades(),
		Position:     e.ledger.Position(),
		Risk:         e.risk.Export(),
		Pricer:       e.pricer.Export(),
		LastTickSeq:  e.lastTickSeq,
		LastTradeSeq: e.lastTradeSeq,
	}
	if intent, ok := e.rec.LastIntent(); ok {
		clone := intent
		st.LastIntent = &clone
	}
	if e.haveTick {
		tick := e.lastTick
		st.LastTick = &tick
	}
	return e.store.Save(st)
}

func sortStrings(s []string) {
	sort.Strings(s)
}
