package admission

import (
	"context"
	"errors"
	"sync"
)

var ErrBusy = errors.New("model backend busy; background work deferred")

type Gate struct {
	mu                     sync.Mutex
	active                 bool
	activeBackgroundCancel context.CancelFunc
	foreground             int
	changed                chan struct{}
}

type Snapshot struct {
	Active            bool `json:"active"`
	ActiveBackground  bool `json:"active_background"`
	ForegroundWaiters int  `json:"foreground_waiters"`
}

func New() *Gate { return &Gate{changed: make(chan struct{})} }

func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{Active: g.active, ActiveBackground: g.activeBackgroundCancel != nil, ForegroundWaiters: g.foreground}
}

func (g *Gate) Acquire(ctx context.Context, background bool) (context.Context, func(), error) {
	g.mu.Lock()
	if !background {
		g.foreground++
		if g.activeBackgroundCancel != nil {
			g.activeBackgroundCancel()
		}
	}
	foregroundWaiting := !background
	for {
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			if foregroundWaiting {
				g.mu.Lock()
				g.foreground--
				g.signal()
				g.mu.Unlock()
			}
			return nil, nil, err
		}
		if background && (g.active || g.foreground > 0) {
			g.mu.Unlock()
			return nil, nil, ErrBusy
		}
		if !g.active {
			g.active = true
			admittedCtx := ctx
			var cancel context.CancelFunc
			if background {
				admittedCtx, cancel = context.WithCancel(ctx)
				g.activeBackgroundCancel = cancel
			}
			g.mu.Unlock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					if cancel != nil {
						cancel()
					}
					g.mu.Lock()
					g.active = false
					if background {
						g.activeBackgroundCancel = nil
					}
					if foregroundWaiting {
						g.foreground--
					}
					g.signal()
					g.mu.Unlock()
				})
			}
			return admittedCtx, release, nil
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
