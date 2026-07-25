package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/gateway"
	"github.com/ayush6624/sandbox/internal/gcemig"
)

var (
	gwListen        string
	gwToken         string
	gwTTL           time.Duration
	gwQueueWait     time.Duration
	gwQueueMax      int
	gwScaleProject  string
	gwScaleZone     string
	gwScaleMIG      string
	gwScaleMax      int
	gwScaleSlots    int
	gwScaleHeadroom int
	gwReleaseFile   string
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
	// queue-max also bounds what the queue-depth metric can express: overflow
	// beyond it only shows up as the rejected-creates rate. A queued create is
	// one goroutine + one connection, so a large bound is cheap — undersizing
	// it starves the autoscaler signal (a 1000-burst against a 512 queue read
	// as half its real demand).
	cmd.Flags().IntVar(&gwQueueMax, "queue-max", 4096, "max creates waiting at once; beyond this creates 503 immediately")
	cmd.Flags().StringVar(&gwScaleProject, "direct-scale-project", "", "GCE project for queue-triggered direct MIG scale-out (empty disables)")
	cmd.Flags().StringVar(&gwScaleZone, "direct-scale-zone", "", "GCE zone for queue-triggered direct MIG scale-out")
	cmd.Flags().StringVar(&gwScaleMIG, "direct-scale-mig", "", "GCE managed instance group for queue-triggered direct scale-out")
	cmd.Flags().IntVar(&gwScaleMax, "direct-scale-max", 0, "maximum MIG size for direct scale-out")
	cmd.Flags().IntVar(&gwScaleSlots, "direct-scale-slots-per-host", 0, "sandbox slots supplied by each worker")
	cmd.Flags().IntVar(&gwScaleHeadroom, "direct-scale-headroom", 0, "extra slots included in direct scale-out demand")
	cmd.Flags().StringVar(&gwReleaseFile, "worker-release-file", "", "persisted expected worker release used to gate stale allocations")
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
	if gwReleaseFile != "" {
		if err := g.ConfigureWorkerReleaseFile(gwReleaseFile); err != nil {
			return err
		}
	}
	if gwScaleProject != "" || gwScaleZone != "" || gwScaleMIG != "" {
		scaler, err := gcemig.New(gwScaleProject, gwScaleZone, gwScaleMIG, gwScaleMax)
		if err != nil {
			return err
		}
		if err := g.ConfigureDirectScaleOut(scaler, gwScaleSlots, gwScaleHeadroom); err != nil {
			return err
		}
	}
	return g.Serve(ctx, gwListen)
}
