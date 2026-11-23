package httpserver

import (
	"net/http"
	"sync"
)

// IngressGate 跟踪共享入口已准入的处理器。零值可用，封口后不能重新开放。
type IngressGate struct {
	mu     sync.Mutex
	active int
	done   chan struct{}
}

// Seal 拦住后续准入，返回的通道只在所有已准入处理器返回后关闭。
func (g *IngressGate) Seal() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done == nil {
		g.done = make(chan struct{})
		if g.active == 0 {
			close(g.done)
		}
	}
	return g.done
}

func (g *IngressGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done != nil {
		return false
	}
	g.active++
	return true
}

func (g *IngressGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 && g.done != nil {
		close(g.done)
	}
}

func (g *IngressGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.enter() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		defer g.leave()
		next.ServeHTTP(w, r)
	})
}
