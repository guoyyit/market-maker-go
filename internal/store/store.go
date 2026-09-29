// Package store 负责崩溃恢复所需的本地持久化：
//
//   - state.json：持仓成交集合、最近报价意图、风控状态、配置版本等快照，
//     采用"写临时文件 + fsync + 原子 rename"方式更新，崩溃后只会读到
//     完整的上一版或当前版，不会读到半写文件；
//   - risk.log：保护状态进入/退出的追加式 JSONL 审计日志，只增不改。
//
// 全部为本地文件操作，不依赖任何外部服务。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/pricing"
	"github.com/guoyyit/market-maker-go/internal/risk"
	"github.com/guoyyit/market-maker-go/internal/types"
)

const (
	snapshotFile = "state.json"
	tmpSuffix    = ".tmp"
	auditLogFile = "risk.log"
	stateVersion = 1
)

// State 是持久化的完整引擎状态快照。
type State struct {
	Version int `json:"version"`

	// Symbol 标的。
	Symbol string `json:"symbol"`

	// Config 最近一次生效的配置（含热更新结果），重启后继续使用。
	Config config.Config `json:"config"`

	// Trades 已生效成交回报全集（重算持仓的唯一事实来源）。
	Trades []types.Trade `json:"trades"`

	// Position 为冗余存储的持仓，恢复时必须与按 Trades 重算结果一致，
	// 不一致以重算结果为准并记录（见 Recover 校验逻辑）。
	Position int64 `json:"position"`

	// LastIntent 最近一次提交成功的报价意图；nil 表示空仓无在挂意图。
	LastIntent *types.Intent `json:"last_intent,omitempty"`

	// LastTick 最近一帧有效行情；nil 表示尚未收到行情。
	LastTick *types.Tick `json:"last_tick,omitempty"`

	// Risk 风控状态快照。
	Risk risk.Snapshot `json:"risk"`

	// Pricer 报价生成器状态（档位代号与 RNG 计数）。
	Pricer pricing.State `json:"pricer"`

	// LastTickSeq / LastTradeSeq 已处理输入的最大序号，用于乱序/重复过滤。
	LastTickSeq  uint64 `json:"last_tick_seq"`
	LastTradeSeq uint64 `json:"last_trade_seq"`
}

// Store 是目录内的持久化存储。
type Store struct {
	dir      string
	snapPath string
	logPath  string
	mu       sync.Mutex
}

// New 在 dir 下创建/打开存储（目录不存在时创建）。
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	s := &Store{
		dir:      dir,
		snapPath: filepath.Join(dir, snapshotFile),
		logPath:  filepath.Join(dir, auditLogFile),
	}
	return s, nil
}

// Save 原子写入一份完整状态快照。
func (s *Store) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st.Version = stateVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal snapshot: %w", err)
	}

	tmp := s.snapPath + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: open temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close temp: %w", err)
	}
	if err := os.Rename(tmp, s.snapPath); err != nil {
		return fmt.Errorf("store: rename snapshot: %w", err)
	}
	return nil
}

// Load 读取最近一次完整快照；没有快照时返回 ErrNoSnapshot。
func (s *Store) Load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.snapPath)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNoSnapshot
	}
	if err != nil {
		return State{}, fmt.Errorf("store: read snapshot: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("store: parse snapshot: %w", err)
	}
	if st.Version != stateVersion {
		return State{}, fmt.Errorf("store: unsupported snapshot version %d", st.Version)
	}
	return st, nil
}

// AppendTransition 向审计日志追加一条保护状态流转记录。
func (s *Store) AppendTransition(tr risk.Transition) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("store: open audit log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(tr)
	if err != nil {
		return fmt.Errorf("store: marshal transition: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("store: write audit log: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: fsync audit log: %w", err)
	}
	return nil
}

// ReadTransitions 读取全部审计记录（按写入顺序）。
func (s *Store) ReadTransitions() ([]risk.Transition, error) {
	s.mu.Lock()
	path := s.logPath
	s.mu.Unlock()

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: open audit log: %w", err)
	}
	defer f.Close()

	var out []risk.Transition
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var tr risk.Transition
			if jerr := json.Unmarshal(line, &tr); jerr != nil {
				return nil, fmt.Errorf("store: parse audit line: %w", jerr)
			}
			out = append(out, tr)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("store: read audit log: %w", err)
		}
	}
	return out, nil
}

// ErrNoSnapshot 表示尚不存在任何状态快照（首次启动属正常情况）。
var ErrNoSnapshot = errors.New("store: no snapshot exists")
