// Package replay 提供离线确定性回放：
// 给定固定随机种子的配置与同一输入事件序列，报价/撤单/持仓变化序列逐条一致，
// 可用于复现历史场景与回归测试。
//
// 回放不使用真实时钟：定时器事件由输入序列显式给出，
// 所有撮合由本地内存网关的确定性模拟方法驱动，不访问任何外部服务。
package replay

import (
	"context"
	"strconv"

	"github.com/guoyyit/market-maker-go/internal/engine"
)

// Step 是回放中一个事件处理完毕后的确定性输出快照。
type Step struct {
	Index       int
	Position    int64
	Risk        string
	Skipped     bool
	Transitions []string
	OpenQuotes  []string
}

// Recorder 记录引擎处理过程中的确定性输出（持仓、保护流转、在挂报价），
// 用于逐条比对两次回放。
type Recorder struct {
	Steps []Step
}

// NewRecorder 创建输出记录器。
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Run 按顺序向引擎注入事件、排空观测通道并运行到序列结束。
// 调用方负责引擎的 Recover 与 ctx 生命周期。两次调用在相同种子与相同
// 事件序列下产出的 Steps 必须逐条一致。
func Run(ctx context.Context, eng *engine.Engine, events []engine.Event, rec *Recorder) error {
	if rec == nil {
		rec = NewRecorder()
	}

	for i, ev := range events {
		eng.Input() <- ev
		obs := <-eng.Observations()
		rec.Steps = append(rec.Steps, StepFromObs(i, obs))
	}
	return nil
}

// StepFromObs 把一条引擎观测转换为可比对的确定性记录条目。
// 当场景中包含由本地撮合驱动的成交时（先 SimulateFill 再注入 Trade 事件），
// 调用方可用本函数把成交事件对应的观测也纳入记录。
func StepFromObs(index int, obs engine.Observation) Step {
	step := Step{
		Index:    index,
		Position: obs.Position,
		Risk:     obs.Risk.String(),
		Skipped:  obs.Skipped,
	}
	for _, tr := range obs.Transitions {
		step.Transitions = append(step.Transitions,
			string(tr.Reason)+":"+tr.From.String()+"->"+tr.To.String())
	}
	for _, q := range obs.OpenQuotes {
		step.OpenQuotes = append(step.OpenQuotes,
			q.ClientOrderID+"@"+itoa(q.Price)+"x"+itoa(q.Qty))
	}
	return step
}

// Equal 报告两次记录是否逐条一致。
func (a *Recorder) Equal(b *Recorder) bool {
	if len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		if !stepEqual(a.Steps[i], b.Steps[i]) {
			return false
		}
	}
	return true
}

func stepEqual(a, b Step) bool {
	if a.Index != b.Index || a.Position != b.Position ||
		a.Risk != b.Risk || a.Skipped != b.Skipped {
		return false
	}
	if len(a.Transitions) != len(b.Transitions) || len(a.OpenQuotes) != len(b.OpenQuotes) {
		return false
	}
	for i := range a.Transitions {
		if a.Transitions[i] != b.Transitions[i] {
			return false
		}
	}
	for i := range a.OpenQuotes {
		if a.OpenQuotes[i] != b.OpenQuotes[i] {
			return false
		}
	}
	return true
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
