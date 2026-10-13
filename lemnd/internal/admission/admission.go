package admission

import (
	"context"
	"errors"
	"sync"
)

var ErrBusy = errors.New("model backend busy; background work deferred")

type Gate struct {
	mu         sync.Mutex
	active     bool
	foreground int
	changed    chan struct{}
}

func New() *Gate { return &Gate{changed: make(chan struct{})} }

func (g *Gate) Acquire(ctx context.Context, background bool) (func(), error) {
	g.mu.Lock()
	if !background {
		g.foreground++
		defer func() { g.mu.Lock(); g.foreground--; g.signal(); g.mu.Unlock() }()
	}
	for {
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if background && (g.active || g.foreground > 0) {
			g.mu.Unlock()
			return nil, ErrBusy
		}
		if !g.active {
			g.active = true
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); g.active = false; g.signal(); g.mu.Unlock() }) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
	}
}

func (g *Gate) signal() { close(g.changed); g.changed = make(chan struct{}) }
