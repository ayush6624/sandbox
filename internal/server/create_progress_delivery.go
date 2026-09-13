package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

const createProgressPageSize = 32

func (s *Server) runCreateProgressDelivery(ctx context.Context, local createops.Store) {
	client := &http.Client{Timeout: 3 * time.Second}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var cursor, retentionCursor string
	var lastErrorLog time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		next, err := s.deliverCreateProgressPage(ctx, local, client, cursor)
		cursor = next
		nextRetention, retentionErr := s.reg.CompactCreateProgress(ctx, retentionCursor, createProgressPageSize)
		retentionCursor = nextRetention
		if retentionErr != nil {
			err = errors.Join(err, fmt.Errorf("compact create progress: %w", retentionErr))
		}
		if err != nil && ctx.Err() == nil && time.Since(lastErrorLog) >= 30*time.Second {
			log.Printf("create progress pending: %v", err)
			lastErrorLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) deliverCreateProgressPage(ctx context.Context, local createops.Store, client *http.Client, afterID string) (string, error) {
	page, err := s.reg.PendingOwnedCreateProgress(ctx, afterID, createProgressPageSize)
	if err != nil {
		return afterID, err
	}
	if len(page) == 0 {
		return "", nil
	}
	errs := make([]error, len(page))
	runBounded(4, len(page), func(i int) {
		deliveryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		errs[i] = s.deliverCreateProgress(deliveryCtx, local, client, page[i])
	})
	return page[len(page)-1].ID, errors.Join(errs...)
}

func (s *Server) deliverCreateProgress(ctx context.Context, local createops.Store, client *http.Client, p registry.CreateProgress) error {
	worker := createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID()}
	var sequence int64
	switch p.Target {
	case registry.CreateProgressLocal:
		var err error
		sequence, err = local.IngestProgress(ctx, worker, p)
		if err != nil {
			return err
		}
	case registry.CreateProgressGateway:
		if s.cfg.GatewayURL == "" || s.gatewayCredentials == nil {
			return errors.New("gateway progress delivery is unconfigured")
		}
		body, err := json.Marshal(createops.ProgressUpdate{Worker: worker, Progress: p})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.GatewayURL, "/")+"/internal/v1/create-progress", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.gatewayCredentials.Outbound())
		req.Header.Set("Content-Type", "application/json")
		deliveryClient := *client
		deliveryClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		response, err := deliveryClient.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			return fmt.Errorf("gateway progress returned HTTP %d", response.StatusCode)
		}
		var ack createops.ProgressAcknowledgement
		decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&ack); err != nil {
			return err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("gateway progress acknowledgement has trailing data")
		}
		sequence = ack.Sequence
	default:
		return errors.New("create progress has no delivery target")
	}
	if sequence != p.Sequence {
		return errors.New("create progress acknowledgement does not match the delivered sequence")
	}
	return s.reg.AcknowledgeCreateProgress(ctx, p.ID, sequence)
}
