package main

import (
	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/config"
)

var (
	cfgPath string
	socket  string
)

// addClientFlags registers flags for commands that talk to the server.
func addClientFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	cmd.Flags().StringVar(&socket, "socket", "", "override server socket path (defaults to config.socket_path)")
}

// dialClient loads the config and returns a Client pointing at the configured socket.
func dialClient() (*config.Config, *client.Client, error) {
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
