package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sandbox",
		Short: "Firecracker microVM sandboxes for Node/Python dev",
	}
	root.AddCommand(serveCmd(), gatewayCmd(), upCmd(), downCmd(), listCmd(), doctorCmd(), execCmd(), shellCmd(), readCmd(), writeCmd(), lsCmd(), exposeCmd(), portsCmd(), installAgentCmd(), stopServerCmd())
	return root
}
