package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type handoffShutdown struct {
	mu       sync.Mutex
	draining bool
	active   int
}

// Admission and the active count share a lock so shutdown cannot observe an
// empty journal just before an already admitted release commits its handoff.
func (s *Server) handoffShutdownHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := (r.Method == http.MethodGet || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, "/internal/v1/hibernations/")
		if peer {
			next.ServeHTTP(w, r)
			return
		}
		s.handoffShutdown.mu.Lock()
		if s.handoffShutdown.draining {
			s.handoffShutdown.mu.Unlock()
			http.Error(w, "worker is shutting down", http.StatusServiceUnavailable)
			return
		}
		s.handoffShutdown.active++
		s.handoffShutdown.mu.Unlock()
		defer func() {
			s.handoffShutdown.mu.Lock()
			s.handoffShutdown.active--
			s.handoffShutdown.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) waitHandoffShutdown(ctx context.Context) error {
	s.handoffShutdown.mu.Lock()
	s.handoffShutdown.draining = true
	s.handoffShutdown.mu.Unlock()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.handoffShutdown.mu.Lock()
		active := s.handoffShutdown.active
		s.handoffShutdown.mu.Unlock()
		jobs, err := s.reg.ListHibernationHandoffs(ctx)
		pending := 0
		for _, job := range jobs {
			if !job.Published || !job.BackupComplete {
				pending++
			}
		}
		if err == nil && active == 0 && pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d active requests, %d unfinished handoffs (journal read: %v): %w", active, pending, err, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Server) drainHandoffsBeforeShutdown(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := s.waitHandoffShutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown: peer drain incomplete; retained journals will resume after restart: %v\n", err)
	}
}

func (s *Server) drainUsageBeforeShutdown(parent context.Context) {
	if s.usagePut == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	n, err := s.flushUsage(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: final flush failed, %d intervals may be host-local only: %v\n", n, err)
	}
}
