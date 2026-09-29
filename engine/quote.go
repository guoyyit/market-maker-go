package engine

// RNG 是确定性伪随机源（线性同余），状态会被持久化以保证跨重启一致。
// 递推：state = state*6364136223846793005 + 1442695040888963407（mod 2^64，Knuth MMIX）。
type RNG struct {
	state uint64
}

// NewRNG 以种子构造随机源。种子为 0 时归一化为 1，避免 LCG 停在零状态。
func NewRNG(seed uint64) *RNG {
	if seed == 0 {
		seed = 1
	}
	return &RNG{state: seed}
}

// Next 返回下一个伪随机数并推进状态。
func (r *RNG) Next() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

// State / RestoreState 用于快照持久化。
func (r *RNG) State() uint64 { return r.state }

func (r *RNG) RestoreState(s uint64) {
	if s == 0 {
		s = 1
	}
	r.state = s
}

// Quoter 依据行情、配置、持仓与随机源生成双侧报价意图。
type Quoter struct {
	cfg Config
}

// NewQuoter 构造报价器。
func NewQuoter(cfg Config) *Quoter { return &Quoter{cfg: cfg} }

// UpdateConfig 热更新报价参数。
func (q *Quoter) UpdateConfig(cfg Config) { q.cfg = cfg }

// Generate 生成新的报价意图。
// mid 为当前有效中间价；position 为净持仓；seq 为意图版本号。
// 返回的意图一定非 nil；不满足报价条件时 Quotes 为空且在 Reason 中说明。
func (q *Quoter) Generate(mid int64, tick *Tick, position int64, seq int64, rng *RNG) *Intent {
	cfg := q.cfg
	intent := &Intent{
		Seq:       seq,
		Symbol:    cfg.Symbol,
		CreatedAt: tick.Time,
		Reason:    "normal",
	}

	// 库存倾斜：多头偏离把全部报价下压（促进卖出、抑制买入），空头反之。
	deviation := position - cfg.TargetPosition
	adjustedMid := mid - deviation*cfg.SkewPerUnit

	// 持仓硬上限：达到上限后停止同方向增加敞口的报价（另一侧仍可减仓）。
	allowBuy := position < cfg.MaxPosition
	allowSell := position > -cfg.MaxPosition

	// 数量倾斜：买侧随多头偏离缩小、卖侧放大，反之亦然。
	buyPct := clampPercent(100 - deviation*cfg.QuantitySkewPctPerUnit)
	sellPct := clampPercent(100 + deviation*cfg.QuantitySkewPctPerUnit)

	var quotes []Quote
	for level := 1; level <= cfg.Levels; level++ {
		distance := cfg.QuoteOffset + int64(level-1)*cfg.TickStep
		if allowBuy {
			price := adjustedMid - distance + jitter(cfg, rng)
			if qty := skewQuantity(cfg.BaseQuantity, buyPct); qty > 0 {
				quotes = append(quotes, Quote{Side: SideBuy, Level: level, Price: price, Quantity: qty})
			}
		}
		if allowSell {
			price := adjustedMid + distance + jitter(cfg, rng)
			if qty := skewQuantity(cfg.BaseQuantity, sellPct); qty > 0 {
				quotes = append(quotes, Quote{Side: SideSell, Level: level, Price: price, Quantity: qty})
			}
		}
	}

	if len(quotes) > 0 {
		if _, _, spread := bestQuotes(quotes); spread > cfg.MaxSpread {
			intent.Reason = "spread_too_wide"
			return intent
		}
	}
	intent.Quotes = quotes
	return intent
}

func clampPercent(pct int64) int64 {
	if pct < 0 {
		return 0
	}
	if pct > 200 {
		return 200
	}
	return pct
}

func skewQuantity(base int64, pct int64) int64 {
	qty := base * pct / 100
	if pct > 0 && qty == 0 {
		qty = 1
	}
	return qty
}

// jitter 在配置启用随机种子时产生 {-1,0,+1} 个价位的确定性抖动；否则为 0。
// 随机数消费顺序固定为“每档先买后卖”，保证回放逐档一致。
func jitter(cfg Config, rng *RNG) int64 {
	if cfg.RandomSeed == 0 || rng == nil {
		return 0
	}
	return int64(rng.Next()%3) - 1
}

// bestQuotes 返回最优买价、最优卖价与两者价差。
func bestQuotes(quotes []Quote) (bestBuy int64, bestSell int64, spread int64) {
	bestBuy, bestSell = MinInt64, MaxInt64
	for _, q := range quotes {
		if q.Side == SideBuy && q.Price > bestBuy {
			bestBuy = q.Price
		}
		if q.Side == SideSell && q.Price < bestSell {
			bestSell = q.Price
		}
	}
	if bestBuy != MinInt64 && bestSell != MaxInt64 {
		spread = bestSell - bestBuy
	}
	return bestBuy, bestSell, spread
}

// MidPrice 从行情计算中间价：买卖盘同时有效时取向下取整中值，否则取最新价。
func MidPrice(t *Tick) int64 {
	if t.BidPrice > 0 && t.AskPrice > 0 {
		return (t.BidPrice + t.AskPrice) / 2
	}
	return t.LastPrice
}

// 64 位整数极值，供最优价初值比较使用。
const (
	MinInt64 = -1 << 63
	MaxInt64 = 1<<63 - 1
)
