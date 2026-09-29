package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Store 是持久化接口：快照记录持仓/意图/保护状态，事件日志记录全部输入事件。
// 恢复时先载入最近快照，再重放其后的事件日志：
// 持仓以成交回报重算为真相来源核对，意图版本号单调递增保证不重复挂单。
type Store interface {
	// SaveSnapshot 以原子方式持久化快照（写临时文件后 rename）。
	SaveSnapshot(snap Snapshot) error
	// LoadSnapshot 读取最近一次快照；无快照时返回 os.ErrNotExist。
	LoadSnapshot() (Snapshot, error)
	// AppendEvent 追加一条输入事件日志（先持久化后生效）。
	AppendEvent(ctx context.Context, env Envelope) error
	// ReadEvents 按持久化顺序读取全部事件。
	ReadEvents(ctx context.Context) ([]Envelope, error)
	Close() error
}

const (
	snapshotFile = "snapshot.json"
	eventsFile   = "events.jsonl"
)

// FileStore 是基于本地文件的 Store 实现，全部数据落在本地目录，无外部服务。
type FileStore struct {
	dir string

	mu      sync.Mutex
	eventF  *os.File
	encoder *json.Encoder
}

// NewFileStore 打开（或创建）目录 dir 下的持久化文件。
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	path := filepath.Join(dir, eventsFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	return &FileStore{
		dir:     dir,
		eventF:  f,
		encoder: json.NewEncoder(f),
	}, nil
}

// SaveSnapshot 以“写临时文件 + fsync + rename”方式原子替换快照。
func (s *FileStore) SaveSnapshot(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmp := filepath.Join(s.dir, snapshotFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(snap); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, snapshotFile)); err != nil {
		return err
	}
	return nil
}

// LoadSnapshot 读取最近一次快照。
func (s *FileStore) LoadSnapshot() (Snapshot, error) {
	var snap Snapshot
	data, err := os.ReadFile(filepath.Join(s.dir, snapshotFile))
	if err != nil {
		return Snapshot{}, err
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	return snap, nil
}

// AppendEvent 在事件生效前同步追加一行 JSON 并 fsync。
func (s *FileStore) AppendEvent(ctx context.Context, env Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.encoder.Encode(env); err != nil {
		return err
	}
	return s.eventF.Sync()
}

// ReadEvents 按日志顺序读取全部事件；日志文件不存在时返回空切片。
func (s *FileStore) ReadEvents(ctx context.Context) ([]Envelope, error) {
	path := filepath.Join(s.dir, eventsFile)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Envelope
	reader := bufio.NewReader(f)
	dec := json.NewDecoder(reader)
	for {
		var env Envelope
		if err := dec.Decode(&env); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode event log: %w", err)
		}
		events = append(events, env)
	}
	return events, nil
}

// Close 关闭事件日志文件。
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventF == nil {
		return nil
	}
	err := s.eventF.Close()
	s.eventF = nil
	return err
}

// MemoryStore 是 Store 的进程内实现，供测试与确定性回放使用。
type MemoryStore struct {
	mu     sync.Mutex
	snap   *Snapshot
	events []Envelope
}

// NewMemoryStore 构造内存持久化。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) SaveSnapshot(snap Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snap = &snap
	return nil
}

func (m *MemoryStore) LoadSnapshot() (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil {
		return Snapshot{}, os.ErrNotExist
	}
	return *m.snap, nil
}

func (m *MemoryStore) AppendEvent(ctx context.Context, env Envelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, env)
	return nil
}

func (m *MemoryStore) ReadEvents(ctx context.Context) ([]Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Envelope, len(m.events))
	copy(out, m.events)
	return out, nil
}

func (m *MemoryStore) Close() error { return nil }
