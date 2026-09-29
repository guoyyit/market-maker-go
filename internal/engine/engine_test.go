package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/guoyyit/market-maker-go/internal/config"
	"github.com/guoyyit/market-maker-go/internal/gateway"
)

const testSymbol = "SYM"

func testConfig() config.Config {
	return config.Config{
		Symbol:           testSymbol,
		Levels:           2,
		BaseQty:          10,
		HalfSpread:       2,
		LevelStep:        1,
		MaxSpread:        20,
		MaxPosition:      30,
		InventoryTarget:  0,
		SkewCoeff:        0,
		SkewDenom:        10,
		HeartbeatTimeout: 100 * time.Millisecond,
		MaxPriceJump:     5,
		JitterTicks:      0,
		RNGSeed:          7,
	}
}

type harness struct {
	t        *testing.T
	eng      *Engine
	gw       *gateway.InMemory
	storeDir string
	cancel   context.CancelFunc
	done     chan struct{}
	runErr   chan error
	now      *time.Time
}

func newHarness(t *testing.T, cfg config.Config) *harness {
	t.Helper()
	gw := gateway.NewInMemory(64)
	start := time.Unix(1_000_000, 0)
	storeDir := filepath.Join(t.TempDir(), "state")

	opts := Options{
		Symbol:   testSymbol,
		Initial:  cfg,
		Gateway:  gw,
		StoreDir: storeDir,
		Now: func() time.Time {
			return start
		},
	}
	eng, err := New(opts)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := eng.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	done := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- eng.Run(ctx)
		close(done)
	}()
	h := &harness{t: t, eng: eng, gw: gw, storeDir: storeDir,
		cancel: cancel, done: done, runErr: runErr, now: &start}
	return h
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

func (h *harness) send(ev Event) Observation {
	h.t.Helper()
	select {
	case err := <-h.runErr:
		h.t.Fatalf("engine run exited before processing: %v", err)
	default:
	}
	h.eng.Input() <- ev
	select {
	case obs := <-h.eng.Observations():
		return obs
	case err := <-h.runErr:
		h.t.Fatalf("engine run exited while processing: %v", err)
		return Observation{}
	}
}

func (h *harness) openCount() int {
	return len(h.gw.OpenOrders())
}
