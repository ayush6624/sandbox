package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ayush6624/sandbox/services/sandbox-edge/internal/edge"
)

func main() {
	var cfg edge.Config
	flag.StringVar(&cfg.ListenAddr, "listen", ":443", "public TLS listener")
	flag.StringVar(&cfg.HTTPAddr, "http-listen", ":80", "plain HTTP redirect listener (empty disables)")
	flag.StringVar(&cfg.MetricsAddr, "metrics-listen", "127.0.0.1:9091", "private metrics listener")
	flag.StringVar(&cfg.Domain, "domain", "", "wildcard ingress suffix, e.g. sb.example.com")
	flag.StringVar(&cfg.CertFile, "cert-file", "", "wildcard TLS certificate PEM")
	flag.StringVar(&cfg.KeyFile, "key-file", "", "wildcard TLS private key PEM")
	flag.BoolVar(&cfg.PlainHTTP, "plain-http", false, "serve plain HTTP on --listen (local development only)")
	flag.StringVar(&cfg.GatewayURL, "gateway", "", "sandbox gateway base URL")
	flag.StringVar(&cfg.GatewayToken, "gateway-token", "", "gateway bearer token")
	flag.DurationVar(&cfg.CacheTTL, "route-ttl", 5*time.Second, "maximum positive route cache TTL")
	flag.DurationVar(&cfg.NegativeTTL, "negative-ttl", time.Second, "unknown-sandbox cache TTL")
	flag.DurationVar(&cfg.DialTimeout, "dial-timeout", 95*time.Second, "route + wake + worker dial timeout")
	flag.DurationVar(&cfg.DrainTimeout, "drain-timeout", 5*time.Minute, "maximum graceful shutdown wait for active tunnels")
	flag.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 10*time.Second, "maximum TLS handshake time")
	flag.IntVar(&cfg.MaxConnections, "max-connections", 100000, "maximum concurrent HTTPS and raw TCP connections")
	flag.StringVar(&cfg.RawListenIP, "raw-listen-ip", "0.0.0.0", "bind IP for raw TCP listeners")
	flag.IntVar(&cfg.RawPortMin, "raw-port-min", 0, "raw TCP range start (0 disables raw ingress)")
	flag.IntVar(&cfg.RawPortMax, "raw-port-max", 0, "raw TCP range end")
	flag.IntVar(&cfg.FirstHitRate, "first-hit-rate", 20, "maximum new HTTPS and raw connections per source IP per second")
	flag.Parse()

	svc, err := edge.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-edge:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := svc.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-edge:", err)
		os.Exit(1)
	}
}
