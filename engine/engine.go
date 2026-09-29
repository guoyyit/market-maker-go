package engine

import (
	"context"
	"errors"
)

// Engine 把确定性内核、内存网关、输入管道与本地持久化装配为可运行引擎。
// 每个输入事件严格按“持久化 -> 内核推进 -> 网关执行 -> 模拟撮合 -> 快照”顺序处理，
// 因此崩溃在任意一步之后都可恢复，且不会重复挂单。
type Engine struct {
	kernel *Kernel
	gw     Gateway
	sim    *SimGateway
	store  Store
	hasSim bool
}

// NewEngine 构造全新引擎（空持仓、无在挂订单）。
func NewEngine(cfg Config, gw Gateway, store Store) (*Engine, error) {
	cfg, err := Validate(cfg)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		kernel: NewKernel(cfg),
		gw:     gw,
		store:  store,
	}
	if sim, ok := gw.(*SimGateway); ok {
		e.sim = sim
		e.hasSim = true
	}
	if err := store.SaveSnapshot(e.kernel.Snapshot()); err != nil {
		return nil, err
	}
	return e, nil
}

// RecoverEngine 从持久化状态恢复引擎。
// 持仓以事件日志中的成交回报重算并与快照核对；在挂订单从最近报价意图重建，
// 网关内已存在的同 ClientID 订单不会被重复提交。
func RecoverEngine(gw Gateway, store Store) (*Engine, error) {
	snap, err := store.LoadSnapshot()
	if err != nil {
		return nil, err
	}
	events, err := store.ReadEvents(context.Background())
	if err != nil {
		return nil, err
	}
	var fills []Fill
	for _, env := range events {
		if env.Fill != nil {
			fills = append(fills, *env.Fill)
		}
	}
	kernel, err := RestoreKernel(snap, fills)
	if err != nil {
		return nil, err
	}

	e := &Engine{kernel: kernel, gw: gw, store: store}
	if sim, ok := gw.(*SimGateway); ok {
		e.sim = sim
		e.hasSim = true
	}

	// 以最近意图为权威集合恢复网关：先补齐缺失订单，再撤销多余订单。
	if err := e.reconcileGatewayWithKernel(); err != nil {
		return nil, err
	}
	return e, nil
}

// reconcileGatewayWithKernel 让网关在挂集合与内核报价意图一致。
// 网关已存在的同 ClientID 订单被视为同一意图的订单，不重复提交。
func (e *Engine) reconcileGatewayWithKernel() error {
	desired := e.kernel.LiveOrders()
	current := e.gw.LiveOrders()

	for id := range current {
		if _, ok := desired[id]; !ok {
			if _, err := e.gw.Cancel(id); err != nil && !errors.Is(err, ErrOrderNotFound) {
				return err
			}
		}
	}
	for id, order := range desired {
		if _, ok := current[id]; ok {
			continue
		}
		if _, err := e.gw.Place(order); err != nil && !errors.Is(err, ErrDuplicateClientID) {
			return err
		}
	}
	return nil
}

// Run 消费 feed 直到其关闭或 ctx 取消。
func (e *Engine) Run(ctx context.Context, feed *Feed) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case env, ok := <-feed.C():
			if !ok {
				return nil
			}
			e.ProcessOne(env)
		}
	}
}

// ProcessOne 同步处理单个事件（供测试与确定性回放使用），并执行其产生的全部副作用。
func (e *Engine) ProcessOne(env Envelope) StepResult {
	ctx := context.Background()

	// 1. 先持久化输入事件（成交回报是恢复持仓的真相来源）。
	if e.store != nil {
		_ = e.store.AppendEvent(ctx, env)
	}

	// 2. 推进确定性内核。
	result := e.kernel.Step(env)

	// 3. 在网关上执行动作。模拟网关的幂等提交使恢复/重放不会重复挂单。
	e.executeActions(result.Actions)

	// 4. 若输入是被接受的行情且使用内置模拟网关，立即做一次确定性撮合；
	//    撮合产生的成交回报作为内部事件级联处理（先持久化后生效）。
	if env.Tick != nil && e.hasSim && result.Accepted {
		for _, fill := range e.sim.Match(*env.Tick, env.WallTime) {
			fillEnv := Envelope{Fill: &fill, WallTime: fill.Time}
			if e.store != nil {
				_ = e.store.AppendEvent(ctx, fillEnv)
			}
			fillResult := e.kernel.Step(fillEnv)
			e.executeActions(fillResult.Actions)
			result.SubSteps = append(result.SubSteps, fillResult)
			result.Actions = append(result.Actions, fillResult.Actions...)
			result.Fills = append(result.Fills, fillResult.Fills...)
			result.Audits = append(result.Audits, fillResult.Audits...)
		}
	}

	// 5. 持久化最新快照。
	if e.store != nil {
		_ = e.store.SaveSnapshot(e.kernel.Snapshot())
	}
	return result
}

func (e *Engine) executeActions(actions []Action) {
	for _, action := range actions {
		switch a := action.(type) {
		case PlaceAction:
			_, _ = e.gw.Place(a.Order)
		case CancelAction:
			_, _ = e.gw.Cancel(a.ClientID)
		}
	}
}

// UpdateConfig 热更新配置：立即生成新版本报价意图并增量对账，
// 在途报价的生效规则见 README“配置热更新”一节。
func (e *Engine) UpdateConfig(u ConfigUpdate) error {
	env := Envelope{Update: &u}
	result := e.ProcessOne(env)
	if !result.Accepted {
		return errors.New(result.IgnoredReason)
	}
	return nil
}

// Kernel 暴露内核以便检查状态。
func (e *Engine) Kernel() *Kernel { return e.kernel }

// Gateway 暴露网关。
func (e *Engine) Gateway() Gateway { return e.gw }

// Close 关闭持久化资源。
func (e *Engine) Close() error {
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}

// HasSnapshot 报告持久化中是否存在快照（供启动流程判断是否走恢复）。
func HasSnapshot(store Store) bool {
	_, err := store.LoadSnapshot()
	return err == nil
}
