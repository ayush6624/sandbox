package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/gateway"
)

var (
	gwListen    string
	gwToken     string
	gwTTL       time.Duration
	gwQueueWait time.Duration
	gwQueueMax  int
)

func gatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the multi-host control plane: place sandboxes on, and route requests to, registered hosts",
		Long: `Run the sandbox gateway.

Hosts register by running 'serve --gateway <this url> --gateway-token <token> --listen <addr> --token <addr-token>'.
The gateway exposes the same API as a single server; point the CLI at it with
'--gateway http://<addr> --gateway-token <token>'.`,
		RunE: runGateway,
	}
	cmd.Flags().StringVar(&gwListen, "listen", "", "TCP address to listen on, e.g. 100.64.0.1:9090 (required)")
	cmd.Flags().StringVar(&gwToken, "token", "", "bearer token required on all inbound requests (required)")
	cmd.Flags().DurationVar(&gwTTL, "heartbeat-ttl", 20*time.Second, "drop a host not seen within this window")
	// queue-wait must cover the autoscaler's worst common path: MIG resize →
	// standby VM start → nomad join → serve up + golden build → first warm
	// heartbeat (~2-3 min). 90s was right at the edge and 503'd real bursts.
	cmd.Flags().DurationVar(&gwQueueWait, "queue-wait", 180*time.Second, "how long a create may wait for a free slot before 503 (0 disables queueing)")
	cmd.Flags().IntVar(&gwQueueMax, "queue-max", 512, "max creates waiting at once; beyond this creates 503 immediately")
	return cmd
}

func runGateway(cmd *cobra.Command, args []string) error {
	if gwListen == "" {
		return errors.New("--listen is required")
	}
	if gwToken == "" {
		return errors.New("--token is required (refusing to run an unauthenticated gateway)")
	}

	// The gateway pools many connections per host; don't let the 1024 soft
	// default cap fan-out.
	raiseNoFileLimit()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g := gateway.New(gwToken, gwTTL, gwQueueWait, gwQueueMax)
	return g.Serve(ctx, gwListen)
}
