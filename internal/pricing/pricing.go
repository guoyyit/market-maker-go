// Package pricing 根据最新行情、配置与持仓生成两侧挂单报价。
//
// 报价规则（整数价位，全部确定可复现）：
//   - 倾斜后参考中点 fairMid = tick.Mid() - skewShift；
//   - skewShift = SkewCoeff * SkewUnit（整除截断，
//     持仓高于目标时 fairMid 下移，引导买单被成交的概率下降、卖单上升）；
//   - 第 l 档（l 从 0 起）：
//     买价 = fairMid - HalfSpread - l*LevelStep + jitter
//     卖价 = fairMid + HalfSpread + l*LevelStep + jitter；
//   - jitter 为 [-JitterTicks, JitterTicks] 内由固定种子 RNG 逐帧确定性产生，
//     同一帧买卖两侧共享同一个抖动值，抖动不平滑价差；
//   - ClientOrderID = "SYMBOL-SIDE-LEVEL-GEN" 确定性生成；同一档位未成交时
//     GEN 保持不变，行情变化只产生价格层面的增量撤改；一旦该档被成交，
//     GEN 加 1 后重新挂单，因为网关规定终态（已成交）ID 不得复用；
//   - 触及持仓上限时，停止会继续增加该方向敞口的报价
//     （持仓 >= +MaxPosition 时不再挂买单；<= -MaxPosition 时不再挂卖单）。
//   - 数量倾斜（QtySkewCoeff > 0 时生效，与价格倾斜相互独立、同时生效）：
//     deviation = position - InventoryTarget，
//     qtyShift = QtySkewCoeff * trunc(|deviation| / QtySkewDenom)（向 0 取整），
//     持仓高于目标时买侧每档数量 = BaseQty - qtyShift、卖侧 = BaseQty + qtyShift，
//     低于目标时方向相反；某侧数量 <= 0 时该侧停止挂单（已有报价随对账撤掉），
//     数量回升到正数后自动恢复。
package pricing

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/types"
)

// Generator 根据行情与持仓生成报价意图。
type Generator struct {
	cfg config.Config
	rng *rand.Rand

	// gen[sideKey][level] 为该侧该档当前代号，初值为 0。
	gen map[string]map[int]int
	// postedFP[sideKey][level] 记录上一次成功提交时该档的价格指纹；
	// postedGen[sideKey][level] 记录上一次成功提交时该档的代号。
	// 两者共同决定某档是否需要换新代号（终态 ID 不可复用）。
	postedFP  map[string]map[int]int64
	postedGen map[string]map[int]int
	// draws 是 RNG 已被抽取的次数，用于崩溃恢复后对齐抖动序列。
	draws uint64
	// lastDrawSeq/lastJitter 缓存最近一次发生抖动抽取的行情帧。
	// 同一帧内因成交/配置更新触发的重新报价复用该抖动，不再消耗 RNG，
	// 保证"成交后补挂"与"崩溃恢复"算出的价格与该帧首次报价一致。
	lastDrawSeq uint64
	lastJitter  int64
}

// NewGenerator 用初始配置与随机种子创建报价生成器。
// 相同 (RNGSeed, 输入序列) 下，抖动序列逐条一致。
func NewGenerator(cfg config.Config) *Generator {
	return &Generator{
		cfg: cfg,
		rng: rand.New(rand.NewPCG(uint64(cfg.RNGSeed), uint64(cfg.RNGSeed)^0x9E3779B97F4A7C15)),
		gen: map[string]map[int]int{
			types.SideBuy.String():  {},
			types.SideSell.String(): {},
		},
		postedFP: map[string]map[int]int64{
			types.SideBuy.String():  {},
			types.SideSell.String(): {},
		},
		postedGen: map[string]map[int]int{
			types.SideBuy.String():  {},
			types.SideSell.String(): {},
		},
	}
}

// SetConfig 热更新报价参数（RNG 种子不重置，抖动序列保持连续；
// 语义见 docs/configuration.md 中"热更新生效方式"说明）。
func (g *Generator) SetConfig(cfg config.Config) {
	g.cfg = cfg
}

// AdvanceFill 在某档报价被成交后调用，使该档代号加 1。
// 之后 Generate 为该档产生全新的 ClientOrderID。
func (g *Generator) AdvanceFill(side types.Side, level int) {
	levels, ok := g.gen[side.String()]
	if !ok {
		levels = map[int]int{}
		g.gen[side.String()] = levels
	}
	levels[level]++
}

// GenerationOf 返回某侧某档当前代号。
func (g *Generator) GenerationOf(side types.Side, level int) int {
	return g.gen[side.String()][level]
}

// RotateChanged 根据"上一次成功提交记录"与"当前在挂集合"为意图中的报价
// 确定最终 ClientOrderID，对需要换单的档位把代号加 1。
//
// 为什么必须轮换：本地网关规定已撤销/已成交的 ClientOrderID 进入终态、
// 不得复用。变价撤改因此必须表现为"撤旧代号单、挂新代号单"。
//
// 规则：
//   - 当前代号已因成交大于已提交代号：该新代号尚未提交，直接沿用；
//   - 运行时（recovering=false），代号未轮换但该 ID 已不在挂：
//     说明该档曾因减档/触限被撤，重新挂出必须换新代号；
//   - 该 ID 仍在挂但价格/数量指纹变化：换新代号（先撤后挂由 book 表达）；
//   - 该 ID 仍在挂且指纹不变：保留代号，不产生动作；
//   - recovering=true（崩溃恢复，网关在挂为空）：只按指纹比较，
//     未变化档位保留持久化代号，恢复后逐条重建，不凭空换号。
func (g *Generator) RotateChanged(quotes []types.Quote, open map[string]bool, recovering bool) {
	for i := range quotes {
		quote := &quotes[i]
		key := quote.Side.String()
		currentGen := g.gen[key][quote.Level]
		postedGen, genKnown := g.postedGen[key][quote.Level]
		fp := fingerprint(quote.Price, quote.Qty)
		desiredID := quote.ClientOrderID

		switch {
		case !genKnown:
			// 首次为该档位挂单，代号保持 0。
		case currentGen > postedGen:
			// 代号已因成交轮换过：本代号尚未提交，直接沿用，不再轮换。
		case recovering && g.postedFP[key][quote.Level] != fp:
			// 恢复模式：仅按指纹决定是否换号。
			g.AdvanceFill(quote.Side, quote.Level)
			quote.ClientOrderID = g.orderID(quote.Side, quote.Level)
		case !recovering && !open[desiredID]:
			// 运行时该档旧代号单已不在挂（曾被撤销），必须用新代号重挂。
			g.AdvanceFill(quote.Side, quote.Level)
			quote.ClientOrderID = g.orderID(quote.Side, quote.Level)
		case g.postedFP[key][quote.Level] != fp:
			// 代号仍是已提交的那个、但价格或数量变化：换一个新代号。
			g.AdvanceFill(quote.Side, quote.Level)
			quote.ClientOrderID = g.orderID(quote.Side, quote.Level)
		}
	}
}

// MarkPosted 在一次对账成功后调用，固化各档位当前代号与价格指纹。
func (g *Generator) MarkPosted(quotes []types.Quote) {
	for _, quote := range quotes {
		key := quote.Side.String()
		if g.postedFP[key] == nil {
			g.postedFP[key] = map[int]int64{}
		}
		if g.postedGen[key] == nil {
			g.postedGen[key] = map[int]int{}
		}
		g.postedFP[key][quote.Level] = fingerprint(quote.Price, quote.Qty)
		g.postedGen[key][quote.Level] = g.gen[key][quote.Level]
	}
}

// fingerprint 把价格与数量编码为单一可比较整数，数量取低 32 位。
func fingerprint(price, qty int64) int64 {
	return price*1_000_003 + qty
}

// skewShift 返回给定持仓相对目标的中点平移量（正表示中点下移）。
func (g *Generator) skewShift(position int64) int64 {
	if g.cfg.SkewCoeff == 0 {
		return 0
	}
	deviation := position - g.cfg.InventoryTarget
	// 整除截断：正、负偏离都向 0 取整，两侧语义对称且确定。
	if deviation >= 0 {
		return g.cfg.SkewCoeff * (deviation / g.cfg.SkewDenom)
	}
	neg := -deviation
	return -(g.cfg.SkewCoeff * (neg / g.cfg.SkewDenom))
}

// qtyShift 返回数量倾斜的调整幅度（非负）：
// 相对目标每偏离 QtySkewDenom 个数量单位，同向一侧每档数量减少
// QtySkewCoeff 个单位、反向一侧增加相同数量。
// 整除向 0 截断，调整幅度恒为 QtySkewCoeff 的整数倍，确定可复现。
// 系数为 0 时返回 0（保持 BaseQty 不变，旧配置行为）。
func (g *Generator) qtyShift(deviation int64) int64 {
	if g.cfg.QtySkewCoeff == 0 {
		return 0
	}
	d := deviation
	if d < 0 {
		d = -d
	}
	return g.cfg.QtySkewCoeff * (d / g.cfg.QtySkewDenom)
}

// Generate 生成一帧两侧报价。
// position 为当前持仓；返回的报价集合即为该帧的完整报价意图。
// 返回切片按 (买档 0..N, 卖档 0..N) 固定顺序排列，保证确定性。
func (g *Generator) Generate(tick types.Tick, position int64) []types.Quote {
	shift := g.skewShift(position)
	fairMid := tick.Mid() - shift

	// 数量倾斜：与价格倾斜独立计算、同时生效，互不覆盖。
	// deviation > 0（持仓高于目标）：买侧递减、卖侧递增；
	// deviation < 0：方向相反。
	deviation := position - g.cfg.InventoryTarget
	qShift := g.qtyShift(deviation)
	var buyQty, sellQty int64
	switch {
	case deviation > 0:
		buyQty = g.cfg.BaseQty - qShift
		sellQty = g.cfg.BaseQty + qShift
	case deviation < 0:
		buyQty = g.cfg.BaseQty + qShift
		sellQty = g.cfg.BaseQty - qShift
	default:
		buyQty = g.cfg.BaseQty
		sellQty = g.cfg.BaseQty
	}

	// 整帧买卖两侧共享同一个抖动值；同一 tick 帧（Seq 相同）复用缓存值，
	// 只有真正的新行情帧才从 RNG 抽取，抖动只影响价位水平、不改变两侧价差。
	jitter := int64(0)
	if g.cfg.JitterTicks > 0 {
		if tick.Seq != 0 && tick.Seq == g.lastDrawSeq {
			jitter = g.lastJitter
		} else {
			span := uint64(2*g.cfg.JitterTicks + 1)
			jitter = int64(g.rng.Uint64N(span)) - g.cfg.JitterTicks
			g.draws++
			g.lastDrawSeq = tick.Seq
			g.lastJitter = jitter
		}
	}

	// 库存上限与数量倾斜共同决定一侧是否报价：
	//   - 未达上限时数量随偏离递减，达到上限后同向敞口一侧完全停止报价；
	//   - 倾斜后数量 <= 0 的一侧也停止报价（已有报价随对账撤掉），
	//     数量回升到正数后下一次报价自动恢复。
	canBuy := position < g.cfg.MaxPosition && buyQty > 0
	canSell := position > -g.cfg.MaxPosition && sellQty > 0

	quotes := make([]types.Quote, 0, 2*g.cfg.Levels)
	appendSide := func(side types.Side, can bool, sign int64) {
		if !can {
			return
		}
		for level := 0; level < g.cfg.Levels; level++ {
			offset := g.cfg.HalfSpread + int64(level)*g.cfg.LevelStep
			quotes = append(quotes, types.Quote{
				Symbol:        g.cfg.Symbol,
				Side:          side,
				Level:         level,
				Price:         fairMid + sign*offset + jitter,
				Qty:           sideQty(side, buyQty, sellQty),
				ClientOrderID: g.orderID(side, level),
			})
		}
	}
	appendSide(types.SideBuy, canBuy, -1)
	appendSide(types.SideSell, canSell, +1)
	return quotes
}

// sideQty 返回指定侧别的每档挂单数量。
func sideQty(side types.Side, buyQty, sellQty int64) int64 {
	if side == types.SideBuy {
		return buyQty
	}
	return sellQty
}

// ClientOrderID 返回某标的某侧某档某代号的确定性订单标识。
func ClientOrderID(symbol string, side types.Side, level, generation int) string {
	return symbol + "-" + side.String() + "-" +
		strconv.Itoa(level) + "-" + strconv.Itoa(generation)
}

// ParseClientOrderID 从订单标识解析出侧别、档位与代号。
// 约定标识由 "-" 分隔且末两段为 LEVEL-GEN。
func ParseClientOrderID(id string) (symbol string, side types.Side, level, generation int, err error) {
	parts := strings.Split(id, "-")
	if len(parts) < 4 {
		return "", 0, 0, 0, fmt.Errorf("pricing: malformed client order id %q", id)
	}
	level, err = strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("pricing: bad level in %q: %w", id, err)
	}
	generation, err = strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("pricing: bad generation in %q: %w", id, err)
	}
	switch parts[len(parts)-3] {
	case types.SideBuy.String():
		side = types.SideBuy
	case types.SideSell.String():
		side = types.SideSell
	default:
		return "", 0, 0, 0, fmt.Errorf("pricing: unknown side in %q", id)
	}
	symbol = strings.Join(parts[:len(parts)-3], "-")
	return symbol, side, level, generation, nil
}

func (g *Generator) orderID(side types.Side, level int) string {
	return ClientOrderID(g.cfg.Symbol, side, level, g.gen[side.String()][level])
}

// State 是报价生成器需要持久化的状态（代号表与 RNG 抽取计数）。
type State struct {
	BuyGen      map[int]int   `json:"buy_gen"`
	SellGen     map[int]int   `json:"sell_gen"`
	BuyPosted   map[int]Level `json:"buy_posted,omitempty"`
	SellPosted  map[int]Level `json:"sell_posted,omitempty"`
	Draws       uint64        `json:"draws"`
	LastDrawSeq uint64        `json:"last_draw_seq"`
	LastJitter  int64         `json:"last_jitter"`
}

// Level 记录某档已提交的代号与价格指纹，用于崩溃恢复后的换单判定。
type Level struct {
	Gen         int   `json:"gen"`
	Fingerprint int64 `json:"fingerprint"`
}

// Export 导出生成器状态。
func (g *Generator) Export() State {
	return State{
		BuyGen:      cloneGen(g.gen[types.SideBuy.String()]),
		SellGen:     cloneGen(g.gen[types.SideSell.String()]),
		BuyPosted:   clonePosted(g.postedFP[types.SideBuy.String()], g.postedGen[types.SideBuy.String()]),
		SellPosted:  clonePosted(g.postedFP[types.SideSell.String()], g.postedGen[types.SideSell.String()]),
		Draws:       g.draws,
		LastDrawSeq: g.lastDrawSeq,
		LastJitter:  g.lastJitter,
	}
}

// Restore 恢复代号表并通过空转 RNG 对齐抖动序列。
func (g *Generator) Restore(s State) {
	g.gen = map[string]map[int]int{
		types.SideBuy.String():  cloneGen(s.BuyGen),
		types.SideSell.String(): cloneGen(s.SellGen),
	}
	g.postedFP = map[string]map[int]int64{
		types.SideBuy.String():  restoreFP(s.BuyPosted),
		types.SideSell.String(): restoreFP(s.SellPosted),
	}
	g.postedGen = map[string]map[int]int{
		types.SideBuy.String():  restoreGen(s.BuyPosted),
		types.SideSell.String(): restoreGen(s.SellPosted),
	}
	for g.draws < s.Draws {
		g.rng.Uint64()
		g.draws++
	}
	g.lastDrawSeq = s.LastDrawSeq
	g.lastJitter = s.LastJitter
}

func cloneGen(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func clonePosted(fp map[int]int64, gen map[int]int) map[int]Level {
	out := make(map[int]Level, len(gen))
	for level, gv := range gen {
		out[level] = Level{Gen: gv, Fingerprint: fp[level]}
	}
	return out
}

func restoreFP(in map[int]Level) map[int]int64 {
	out := make(map[int]int64, len(in))
	for level, v := range in {
		out[level] = v.Fingerprint
	}
	return out
}

func restoreGen(in map[int]Level) map[int]int {
	out := make(map[int]int, len(in))
	for level, v := range in {
		out[level] = v.Gen
	}
	return out
}
