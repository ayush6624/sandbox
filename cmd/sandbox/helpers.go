package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/config"
)

var (
	cfgPath     string
	socket      string
	gwAddr      string
	gwAddrToken string
)

// addClientFlags registers flags for commands that talk to the server.
func addClientFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	cmd.Flags().StringVar(&socket, "socket", "", "override server socket path (defaults to config.socket_path)")
	cmd.Flags().StringVar(&gwAddr, "gateway", "", "talk to a gateway at this addr (host:port) over TCP instead of the local socket")
	cmd.Flags().StringVar(&gwAddrToken, "gateway-token", "", "bearer token for the gateway (defaults to config.gateway_token)")
}

// dialClient loads the config and returns a Client. With --gateway set it talks
// to that gateway over TCP with a bearer token (defaulting from config); else it
// dials the local Unix socket. We don't auto-default to config.gateway_url so
// host-local flows (e.g. make remote-down) keep using the socket.
func dialClient() (*config.Config, *client.Client, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if gwAddr != "" {
		token := gwAddrToken
		if token == "" {
			token = cfg.GatewayToken
		}
		return cfg, client.NewHTTP(stripScheme(gwAddr), token), nil
	}
	sock := cfg.SocketPath
	if socket != "" {
		sock = socket
	}
	return cfg, client.New(sock), nil
}

// stripScheme turns "http://host:port" into "host:port" so the same value works
// for both `serve --gateway` (a URL) and the client (host:port).
func stripScheme(s string) string {
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimSuffix(s, "/")
}
