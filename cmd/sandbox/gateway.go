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
	"github.com/ayush6624/sandbox/internal/management"
)

var (
	gwListen          string
	gwToken           string
	gwTokenFile       string
	gwWorkerToken     string
	gwWorkerTokenFile string
	gwEdgeToken       string
	gwEdgeTokenFile   string
	gwTransportMode   string
	gwTLSCertFile     string
	gwTLSKeyFile      string
	gwTTL             time.Duration
	gwQueueWait       time.Duration
	gwQueueMax        int
	gwScaleProject    string
	gwScaleZone       string
	gwScaleMIG        string
	gwScaleMax        int
	gwScaleSlots      int
	gwScaleHeadroom   int
	gwReleaseFile     string
	gwIngressBucket   string
	gwRawHost         string
	gwRawMin          int
	gwRawMax          int
)

func gatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run the multi-host control plane: place sandboxes on, and route requests to, registered hosts",
		Long: `Run the sandbox gateway.

Hosts register with separate worker-control and callback credentials:
'serve --gateway <url> --gateway-token <worker-control-token> --listen <addr> --worker-token <callback-token>'.
The gateway exposes the same API as a single server; point the CLI at it with
'SANDBOX_API_URL=https://<addr> SANDBOX_API_KEY=<client-token>' or the
equivalent '--api-url' and '--api-key' client flags.

The public ingress edge is a third trust domain: '--edge-token' gates
'GET /route/{id}' and 'GET /raw-route/{port}', which return a worker's control
token to the caller and must therefore never be reachable with a client key.`,
		RunE: runGateway,
	}
	cmd.Flags().StringVar(&gwListen, "listen", "", "TCP address to listen on, e.g. 100.64.0.1:9090 (required)")
	cmd.Flags().StringVar(&gwToken, "token", "", "public client API bearer credential")
	cmd.Flags().StringVar(&gwTokenFile, "token-file", "", "reloadable newline-delimited client API credentials")
	cmd.Flags().StringVar(&gwWorkerToken, "worker-token", "", "worker registration/control credential (required outside development)")
	cmd.Flags().StringVar(&gwWorkerTokenFile, "worker-token-file", "", "reloadable newline-delimited worker-control credentials")
	// The edge domain is a distinct third credential because /route and
	// /raw-route return a worker's control token to whoever asks. Unset keeps
	// the legacy behavior (client credential accepted there) so shipping this
	// binary cannot take public ingress down before the edge has its own token;
	// the gateway prints a startup WARNING in that state.
	cmd.Flags().StringVar(&gwEdgeToken, "edge-token", "", "public-ingress edge credential for /route and /raw-route (unset: legacy client-credential fallback + startup warning)")
	cmd.Flags().StringVar(&gwEdgeTokenFile, "edge-token-file", "", "reloadable newline-delimited edge credentials")
	cmd.Flags().StringVar(&gwTransportMode, "management-transport", "", "TCP security mode: tls, private_proxy, or explicit development")
	cmd.Flags().StringVar(&gwTLSCertFile, "tls-cert", "", "TLS certificate file (atomically replace to rotate)")
	cmd.Flags().StringVar(&gwTLSKeyFile, "tls-key", "", "TLS private-key file (atomically replace to rotate)")
	cmd.Flags().DurationVar(&gwTTL, "heartbeat-ttl", 20*time.Second, "drop a host not seen within this window")
	// queue-wait must cover the autoscaler's worst common path: MIG resize →
	// standby VM start → nomad join → serve up + golden build → fresh-worker
	// placement quarantine → first eligible heartbeat (~4 min worst case).
	cmd.Flags().DurationVar(&gwQueueWait, "queue-wait", 240*time.Second, "how long a create may wait for a free slot before 503 (0 disables queueing)")
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
	cmd.Flags().StringVar(&gwIngressBucket, "ingress-bucket", "", "GCS bucket for durable raw TCP allocations (empty disables E4)")
	cmd.Flags().StringVar(&gwRawHost, "raw-public-host", "", "public hostname returned for raw TCP allocations")
	cmd.Flags().IntVar(&gwRawMin, "raw-port-min", 20000, "public raw TCP port range start")
	cmd.Flags().IntVar(&gwRawMax, "raw-port-max", 29999, "public raw TCP port range end")
	return cmd
}

func runGateway(cmd *cobra.Command, args []string) error {
	if gwListen == "" {
		return errors.New("--listen is required")
	}
	if gwToken == "" && gwTokenFile == "" {
		return errors.New("--token or --token-file is required")
	}
	if gwTransportMode == "" {
		return errors.New("--management-transport is required")
	}
	if gwWorkerToken == "" && gwWorkerTokenFile == "" {
		if gwTransportMode != string(management.TransportDevelopment) {
			return errors.New("--worker-token or --worker-token-file is required outside development")
		}
		gwWorkerToken = gwToken
	}

	// The gateway pools many connections per host; don't let the 1024 soft
	// default cap fan-out.
	raiseNoFileLimit()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g := gateway.New(gwToken, gwTTL, gwQueueWait, gwQueueMax)
	if err := g.ConfigureSecurity(
		[]string{gwToken}, gwTokenFile,
		[]string{gwWorkerToken}, gwWorkerTokenFile,
		management.Transport{
			Mode:     management.TransportMode(gwTransportMode),
			CertFile: gwTLSCertFile,
			KeyFile:  gwTLSKeyFile,
		},
	); err != nil {
		return err
	}
	// Must follow ConfigureSecurity: the edge/client and edge/worker
	// disjointness checks need the other two sets and the transport mode.
	//
	// Keyed on whether the flags were PASSED, not on whether they hold a value:
	// `--edge-token=` (an unset shell variable expanded into the unit file) would
	// otherwise fall through to the legacy client-credential fallback with only a
	// log line to show for it — silently reinstating the exact disclosure the
	// edge domain exists to close. Fail closed and make the operator fix it.
	edgeRequested := cmd.Flags().Changed("edge-token") || cmd.Flags().Changed("edge-token-file")
	if edgeRequested && gwEdgeToken == "" && gwEdgeTokenFile == "" {
		return errors.New("--edge-token/--edge-token-file was passed empty: supply a credential, or omit both flags to accept the legacy client-credential fallback on /route and /raw-route")
	}
	if gwEdgeToken != "" || gwEdgeTokenFile != "" {
		if err := g.ConfigureEdgeCredentials([]string{gwEdgeToken}, gwEdgeTokenFile); err != nil {
			return err
		}
	}
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
	if gwIngressBucket != "" {
		if err := g.ConfigureRaw(gateway.RawConfig{
			Bucket: gwIngressBucket, PublicHost: gwRawHost,
			PortMin: gwRawMin, PortMax: gwRawMax,
		}); err != nil {
			return err
		}
	}
	return g.Serve(ctx, gwListen)
}
