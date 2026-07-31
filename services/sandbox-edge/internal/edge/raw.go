package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rawRoute struct {
	SandboxID string `json:"sandbox_id"`
	GuestPort int    `json:"guest_port"`
	HostAddr  string `json:"host_addr"`
	Token     string `json:"token"`
	TTL       int    `json:"ttl"`
}

type rawCacheEntry struct {
	route  rawRoute
	err    error
	expiry time.Time
}

type rawResolveFlight struct {
	done  chan struct{}
	route rawRoute
	err   error
}

type rateWindow struct {
	second int64
	count  int
}

func (s *Service) openRawListeners(ctx context.Context, errc chan<- error) ([]net.Listener, error) {
	if s.cfg.RawPortMin == 0 {
		return nil, nil
	}
	listeners := make([]net.Listener, 0, s.cfg.RawPortMax-s.cfg.RawPortMin+1)
	for port := s.cfg.RawPortMin; port <= s.cfg.RawPortMax; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.RawListenIP, strconv.Itoa(port)))
		if err != nil {
			closeListeners(listeners)
			return nil, fmt.Errorf("listen raw port %d: %w", port, err)
		}
		listeners = append(listeners, ln)
		s.acceptWG.Add(1)
		go func(publicPort int, listener net.Listener) {
			defer s.acceptWG.Done()
			s.acceptRawLoop(ctx, publicPort, listener, errc)
		}(port, ln)
	}
	return listeners, nil
}

func closeListeners(listeners []net.Listener) {
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

func (s *Service) acceptRawLoop(ctx context.Context, publicPort int, ln net.Listener, errc chan<- error) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case errc <- fmt.Errorf("accept raw port %d: %w", publicPort, err):
			default:
			}
			return
		}
		if !s.tryAcquireConn() {
			s.met.rawError.Add(1)
			_ = conn.Close()
			continue
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer s.releaseConn()
			s.handleRawConn(ctx, publicPort, conn)
		}()
	}
}

func (s *Service) allowSource(remote net.Addr) bool {
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		host = remote.String()
	}
	now := time.Now().Unix()
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	window := s.rate[host]
	if window.second != now {
		window = rateWindow{second: now}
	}
	window.count++
	s.rate[host] = window
	// Opportunistic pruning keeps scanner cardinality bounded.
	if len(s.rate) > 10000 {
		for key, value := range s.rate {
			if value.second < now-2 {
				delete(s.rate, key)
			}
		}
	}
	return window.count <= s.cfg.FirstHitRate
}

func (s *Service) handleRawConn(parent context.Context, publicPort int, client net.Conn) {
	defer client.Close()
	s.met.rawOpen.Add(1)
	defer s.met.rawOpen.Add(-1)
	if !s.allowSource(client.RemoteAddr()) {
		s.met.rawRateLimited.Add(1)
		s.met.rawError.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.DialTimeout)
	defer cancel()
	rt, err := s.resolveRaw(ctx, publicPort, false)
	if err != nil {
		s.met.rawError.Add(1)
		return
	}
	backend, status, err := s.connectWorker(ctx, route{
		HostAddr: rt.HostAddr, Token: rt.Token,
	}, rt.SandboxID, rt.GuestPort)
	if err != nil && (status == 0 || status == http.StatusNotFound) {
		s.invalidateRaw(publicPort)
		if fresh, resolveErr := s.resolveRaw(ctx, publicPort, true); resolveErr == nil {
			rt = fresh
			backend, _, err = s.connectWorker(ctx, route{
				HostAddr: rt.HostAddr, Token: rt.Token,
			}, rt.SandboxID, rt.GuestPort)
		}
	}
	if err != nil {
		s.met.rawError.Add(1)
		return
	}
	defer backend.Close()
	s.met.rawOK.Add(1)
	s.pipeRaw(client, backend)
}

func (s *Service) resolveRaw(ctx context.Context, publicPort int, force bool) (rawRoute, error) {
	now := time.Now()
	s.rawMu.Lock()
	if !force {
		if ent, ok := s.rawCache[publicPort]; ok && now.Before(ent.expiry) {
			s.met.rawResolveHit.Add(1)
			s.rawMu.Unlock()
			return ent.route, ent.err
		}
	}
	if fl := s.rawFlight[publicPort]; fl != nil {
		s.rawMu.Unlock()
		select {
		case <-ctx.Done():
			return rawRoute{}, ctx.Err()
		case <-fl.done:
			return fl.route, fl.err
		}
	}
	fl := &rawResolveFlight{done: make(chan struct{})}
	s.rawFlight[publicPort] = fl
	s.met.rawResolveMiss.Add(1)
	s.rawMu.Unlock()

	fl.route, fl.err = s.fetchRawRoute(ctx, publicPort)
	ttl := s.cfg.CacheTTL
	if fl.route.TTL > 0 && time.Duration(fl.route.TTL)*time.Second < ttl {
		ttl = time.Duration(fl.route.TTL) * time.Second
	}
	if fl.err != nil {
		ttl = s.cfg.NegativeTTL
	}
	s.rawMu.Lock()
	s.rawCache[publicPort] = rawCacheEntry{route: fl.route, err: fl.err, expiry: time.Now().Add(ttl)}
	delete(s.rawFlight, publicPort)
	close(fl.done)
	s.rawMu.Unlock()
	return fl.route, fl.err
}

func (s *Service) fetchRawRoute(ctx context.Context, publicPort int) (rawRoute, error) {
	u := strings.TrimRight(s.cfg.GatewayURL, "/") + "/raw-route/" + url.PathEscape(strconv.Itoa(publicPort))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return rawRoute{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.GatewayToken)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return rawRoute{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return rawRoute{}, fmt.Errorf("resolve raw port: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var rt rawRoute
	if err := json.NewDecoder(resp.Body).Decode(&rt); err != nil {
		return rawRoute{}, err
	}
	if rt.SandboxID == "" || rt.GuestPort < 1 || rt.HostAddr == "" || rt.Token == "" ||
		strings.ContainsAny(rt.HostAddr+rt.Token+rt.SandboxID, "\r\n") {
		return rawRoute{}, errors.New("gateway returned an invalid raw route")
	}
	return rt, nil
}

func (s *Service) invalidateRaw(publicPort int) {
	s.rawMu.Lock()
	delete(s.rawCache, publicPort)
	s.rawMu.Unlock()
}

func (s *Service) pipeRaw(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(backend, client)
		s.met.rawBytesIn.Add(n)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, backend)
		s.met.rawBytesOut.Add(n)
		closeWrite(client)
	}()
	wg.Wait()
}
