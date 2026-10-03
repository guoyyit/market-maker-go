package config

import (
	"testing"
	"time"
)

func validCfg() Config {
	return Config{
		Symbol:           "SYM",
		Levels:           2,
		BaseQty:          5,
		HalfSpread:       2,
		LevelStep:        1,
		MaxSpread:        10,
		MaxPosition:      50,
		InventoryTarget:  0,
		SkewCoeff:        1,
		SkewDenom:        10,
		HeartbeatTimeout: time.Second,
		MaxPriceJump:     20,
		JitterTicks:      1,
		RNGSeed:          1,
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := validCfg().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateRejectsBadFields(t *testing.T) {
	cases := map[string]func(Config) Config{
		"empty symbol":     func(c Config) Config { c.Symbol = ""; return c },
		"zero levels":      func(c Config) Config { c.Levels = 0; return c },
		"zero base qty":    func(c Config) Config { c.BaseQty = 0; return c },
		"zero half spread": func(c Config) Config { c.HalfSpread = 0; return c },
		"zero level step":  func(c Config) Config { c.LevelStep = 0; return c },
		"wide half spread": func(c Config) Config { c.HalfSpread = 100; return c },
		"negative max pos": func(c Config) Config { c.MaxPosition = -1; return c },
		"zero skew denom":  func(c Config) Config { c.SkewDenom = 0; return c },
		"negative qty skew": func(c Config) Config {
			c.QtySkewCoeff = -1
			return c
		},
		"zero timeout":    func(c Config) Config { c.HeartbeatTimeout = 0; return c },
		"zero price jump": func(c Config) Config { c.MaxPriceJump = 0; return c },
		"negative jitter": func(c Config) Config { c.JitterTicks = -1; return c },
	}
	for name, mutate := range cases {
		if err := mutate(validCfg()).Validate(); err == nil {
			t.Errorf("case %s: expected validation error", name)
		}
	}
}

func TestManagerHotUpdate(t *testing.T) {
	m := NewManager(validCfg())

	next := validCfg()
	next.Levels = 3
	if err := m.Update(next); err != nil {
		t.Fatal(err)
	}
	if m.Current().Levels != 3 {
		t.Fatalf("levels = %d, want 3", m.Current().Levels)
	}

	select {
	case <-m.Updates():
	default:
		t.Fatal("update signal not delivered")
	}

	bad := validCfg()
	bad.Levels = 0
	if err := m.Update(bad); err == nil {
		t.Fatal("invalid update must be rejected")
	}
	if m.Current().Levels != 3 {
		t.Fatalf("rejected update must keep old config, levels = %d",
			m.Current().Levels)
	}
}
