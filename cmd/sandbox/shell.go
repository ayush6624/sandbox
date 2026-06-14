package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/client"
)

func shellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell <id>",
		Short: "Open an interactive shell (PTY) inside the sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			return runShell(c, args[0])
		},
	}
	addClientFlags(cmd)
	return cmd
}

// runShell attaches the local terminal to a guest pty over the shell WebSocket.
// It puts stdin in raw mode so keystrokes (including Ctrl-C) flow to the remote
// shell, forwards SIGWINCH as resize control messages, and exits with the
// remote shell's exit code.
func runShell(c *client.Client, id string) error {
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("shell requires an interactive terminal (stdin is not a tty)")
	}

	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		cols, rows = 80, 24
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := c.DialShell(ctx, id, uint16(cols), uint16(rows))
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	// Restore on every exit path, including the os.Exit below (which skips defers).
	restore := func() { _ = term.Restore(stdinFd, oldState) }
	defer restore()

	// Relay terminal resizes.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			cols, rows, err := term.GetSize(stdinFd)
			if err != nil {
				continue
			}
			ctl, _ := json.Marshal(agentapi.ShellControl{
				Type: agentapi.ShellResize,
				Cols: uint16(cols),
				Rows: uint16(rows),
			})
			_ = conn.Write(ctx, websocket.MessageText, ctl)
		}
	}()

	// stdin → guest pty.
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// guest pty → stdout, until the shell exits and the socket closes.
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			var ce websocket.CloseError
			if errors.As(rerr, &ce) && strings.HasPrefix(ce.Reason, agentapi.ShellExitPrefix) {
				code, _ := strconv.Atoi(strings.TrimPrefix(ce.Reason, agentapi.ShellExitPrefix))
				restore()
				os.Exit(code)
			}
			// Local stdin closed / connection dropped: a clean end, not an error.
			return nil
		}
		if typ == websocket.MessageBinary {
			_, _ = os.Stdout.Write(data)
		}
	}
}
