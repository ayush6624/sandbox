package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

func execCmd() *cobra.Command {
	var cwd string
	var timeoutSec int
	var stream bool
	cmd := &cobra.Command{
		Use:   "exec <id> -- <command...>",
		Short: "Run a shell command inside a sandbox",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			req := agentapi.ExecRequest{
				Cmd:        strings.Join(args[1:], " "),
				Cwd:        cwd,
				TimeoutSec: timeoutSec,
			}

			var res agentapi.ExecResult
			if stream {
				// Output is printed as it arrives; the accumulated copy in
				// res is ignored.
				res, err = c.ExecStream(context.Background(), args[0], req, func(ev agentapi.ExecEvent) {
					switch ev.Type {
					case agentapi.EventStdout:
						fmt.Fprint(os.Stdout, ev.Data)
					case agentapi.EventStderr:
						fmt.Fprint(os.Stderr, ev.Data)
					}
				})
				if err != nil {
					return err
				}
			} else {
				res, err = c.Exec(context.Background(), args[0], req)
				if err != nil {
					return err
				}
				fmt.Fprint(os.Stdout, res.Stdout)
				fmt.Fprint(os.Stderr, res.Stderr)
			}

			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "(command timed out)")
			}
			if res.ExitCode != 0 {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				os.Exit(res.ExitCode)
			}
			return nil
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory inside the guest (default /home/sandbox/app)")
	cmd.Flags().IntVar(&timeoutSec, "timeout", 0, "command timeout in seconds (default 60)")
	cmd.Flags().BoolVar(&stream, "stream", false, "stream output as it arrives instead of waiting for completion")
	return cmd
}
