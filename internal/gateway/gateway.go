// Package gateway 是报价引擎与"交易所"之间的适配层。
//
// 本系统只依赖本地实现：InMemory 网关在进程内存中维护订单，
// 用确定性的 Simulate* 方法模拟成交，不连接任何外部服务。
// 所有操作以 ClientOrderID 幂等，崩溃恢复后重复提交同一报价不会产生重复挂单。
package gateway

import (
	"errors"

	"github.com/guoyyit/market-maker-go/internal/types"
)

// Status 是网关订单的生命周期状态。
type Status int

const (
	// StatusUnknown 未提交或未知。
	StatusUnknown Status = iota
	// StatusOpen 已受理且在挂。
	StatusOpen
	// StatusFilled 已成交（本系统挂单按全部成交处理）。
	StatusFilled
	// StatusCancelled 已撤销。
	StatusCancelled
	// StatusRejected 被网关拒绝（如参数非法）。
	StatusRejected
)

// Report 是网关对一次提交/撤单操作的受理回报。
type Report struct {
	ClientOrderID string
	ExchangeID    string
	Status        Status
	Price         int64
	Qty           int64
	Symbol        string
	Side          types.Side
}

// Fill 是一笔成交回报（由本地撮合模拟产生）。
type Fill = types.Trade

// Gateway 定义引擎所需的订单管理能力。
// 实现必须满足：
//   - Submit 对相同 ClientOrderID 幂等：重复提交不产生第二条挂单；
//   - 若同一 ClientOrderID 此前已成交或已撤销，重复提交返回 ErrOrderFinal；
//   - Cancel 幂等：撤销不存在或已终结的订单返回 ErrNotFound 而非数据损坏；
//   - OpenOrders 与 Fills 的读取与写操作串行化，结果确定。
type Gateway interface {
	// Submit 提交（或重复提交）一条限价单。
	Submit(q types.Quote) (Report, error)
	// Cancel 按 ClientOrderID 撤单。
	Cancel(clientOrderID string) (Report, error)
	// OpenOrders 返回当前在挂订单的快照（key 为 ClientOrderID）。
	OpenOrders() map[string]Report
	// Fills 返回成交回报通道，所有在挂单被撮合时向该通道发送一条 Fill。
	Fills() <-chan Fill
	// Reset 清空网关全部订单与撮合状态（仅用于确定性测试/回放初始化）。
	Reset()
}

// 网关操作的标准错误。
var (
	ErrOrderFinal = errors.New("gateway: order id already in final state")
	ErrNotFound   = errors.New("gateway: order not found")
	ErrBadOrder   = errors.New("gateway: invalid order")
)

// FillSeqSeeder 由能够在重启后对齐成交序号的网关实现。
// 内存网关的成交序号本是进程内自增的，崩溃恢复后必须播种到历史最大值，
// 否则新成交会复用旧成交 ID，被账本错误地当作重复回报。
type FillSeqSeeder interface {
	SeedFillSeq(last uint64)
}
