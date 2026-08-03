package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/config"
)

var (
	cfgPath string
	socket  string
	apiURL  string
	apiKey  string
)

// addClientFlags registers flags for commands that talk to the server.
func addClientFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	cmd.Flags().StringVar(&socket, "socket", "", "override server socket path (defaults to config.socket_path)")
	cmd.Flags().StringVar(&apiURL, "api-url", os.Getenv("SANDBOX_API_URL"), "sandbox API URL (defaults to SANDBOX_API_URL)")
	cmd.Flags().StringVar(&apiKey, "api-key", os.Getenv("SANDBOX_API_KEY"), "sandbox API key (defaults to SANDBOX_API_KEY)")
}

// dialClient uses the same SANDBOX_API_URL/SANDBOX_API_KEY environment as the
// SDK. With no API URL it retains the host-operator Unix socket path.
func dialClient() (*config.Config, *client.Client, error) {
	if apiURL != "" {
		return nil, client.NewHTTP(apiURL, apiKey), nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	sock := cfg.SocketPath
	if socket != "" {
		sock = socket
	}
	return cfg, client.New(sock), nil
}
