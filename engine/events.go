package engine

import "time"

// Envelope 是注入引擎的统一输入事件。三类字段互斥：
// Tick 非空表示行情；Fill 非空表示成交回报；Timer 为 true 表示仅推进事件时钟。
type Envelope struct {
	Tick   *Tick
	Fill   *Fill
	Update *ConfigUpdate
	Timer  bool
	// WallTime 是本事件的事件时间（确定性回放时取数据中的时间戳）。
	WallTime time.Time
}

// Feed 是本地内存管道，行情/成交/时钟事件通过它注入引擎，不经过任何外部服务。
type Feed struct {
	ch     chan Envelope
	closed bool
}

// NewFeed 构造内存管道，buffer 为通道缓冲大小。
func NewFeed(buffer int) *Feed {
	if buffer < 0 {
		buffer = 0
	}
	return &Feed{ch: make(chan Envelope, buffer)}
}

// PostTick 注入一笔行情。
func (f *Feed) PostTick(t Tick) {
	f.ch <- Envelope{Tick: &t, WallTime: t.Time}
}

// PostFill 注入一笔成交回报。
func (f *Feed) PostFill(fill Fill) {
	f.ch <- Envelope{Fill: &fill, WallTime: fill.Time}
}

// PostTimer 注入一次时钟推进。
func (f *Feed) PostTimer(now time.Time) {
	f.ch <- Envelope{Timer: true, WallTime: now}
}

// Post 注入一个已经组装好的事件。
func (f *Feed) Post(env Envelope) {
	f.ch <- env
}

// PostConfigUpdate 注入一次运行时配置热更新。
func (f *Feed) PostConfigUpdate(u ConfigUpdate, now time.Time) {
	f.ch <- Envelope{Update: &u, WallTime: now}
}

// C 返回底层只读通道。
func (f *Feed) C() <-chan Envelope { return f.ch }

// Close 关闭管道。
func (f *Feed) Close() {
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
}
