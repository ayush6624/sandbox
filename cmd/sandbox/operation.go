package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/spf13/cobra"
)

func operationCmd() *cobra.Command {
	root := &cobra.Command{Use: "operation", Short: "Inspect or wait for durable sandbox creation"}
	for _, wait := range []bool{false, true} {
		name := "get"
		if wait {
			name = "wait"
		}
		cmd := &cobra.Command{
			SilenceUsage: true,
			Use:          name + " ID", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				cmd.SetContext(ctx)
				_, c, err := dialClient()
				if err != nil {
					return err
				}
				var op client.Operation
				if wait {
					op, err = c.WaitOperation(cmd.Context(), args[0], 500*time.Millisecond, operationObserver(cmd.ErrOrStderr()))
				} else {
					op, err = c.GetOperation(cmd.Context(), args[0])
				}
				if err != nil {
					return fmt.Errorf("operation %s: %w; accepted work continues; resume with sandbox operation wait %s", args[0], err, args[0])
				}
				if err := writeOperationJSON(cmd.OutOrStdout(), op); err != nil {
					return err
				}
				if wait {
					return operationFailure(op)
				}
				return nil
			},
		}
		addClientFlags(cmd)
		root.AddCommand(cmd)
	}
	return root
}

func operationFailure(op client.Operation) error {
	if op.Failed > 0 || op.Status == "failed" || op.Status == "partially_succeeded" {
		details := []string{}
		for _, result := range op.Results {
			if len(result.Error) > 0 && string(result.Error) != "null" {
				details = append(details, fmt.Sprintf("member %d: %s", result.Index, result.Error))
			}
		}
		return fmt.Errorf("operation %s ended with %d failed member(s): %s", op.ID, op.Failed, strings.Join(details, "; "))
	}
	return nil
}
func writeOperationJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
func operationObserver(w io.Writer) func(client.Operation) {
	last := ""
	return func(op client.Operation) {
		parts := []string{fmt.Sprintf("operation %s %s (%d/%d succeeded, %d failed)", op.ID, op.Status, op.Succeeded, op.Requested, op.Failed)}
		for _, result := range op.Results {
			if p := result.Progress; p != nil {
				part := fmt.Sprintf("member %d %s", result.Index, p.Coordination.Phase)
				if p.Worker != nil {
					part += fmt.Sprintf(", attempt %d %s %s", p.Worker.Attempt, p.Worker.Current.Stage, p.Worker.Condition)
					if p.Worker.LastCompleted != nil {
						part += ", last completed " + p.Worker.LastCompleted.Stage
					}
				}
				parts = append(parts, part)
			}
		}
		line := strings.Join(parts, "; ")
		if line != last {
			fmt.Fprintln(w, line)
			last = line
		}
	}
}
func runDurableUp(cmd *cobra.Command, key string, req client.CreateRequest, async bool) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "idempotency key: %s\nIf acceptance is interrupted, replay the same options with --idempotency-key %s; accepted work continues.\n", key, key)
	op, err := c.CreateAsync(cmd.Context(), key, req)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) {
			return fmt.Errorf("create with idempotency key %s: %w; the server may not support durable creation: upgrade the server or use sandbox up --legacy without --async, --progress, or --idempotency-key", key, err)
		}
		return fmt.Errorf("create with idempotency key %s: %w", key, err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "operation %s accepted; resume with sandbox operation wait %s\n", op.ID, op.ID)
	if async {
		return writeOperationJSON(cmd.OutOrStdout(), op)
	}
	operationID := op.ID
	op, err = c.WaitOperation(cmd.Context(), operationID, 500*time.Millisecond, operationObserver(cmd.ErrOrStderr()))
	if err != nil {
		return fmt.Errorf("operation %s: %w; accepted work continues; resume with sandbox operation wait %s", operationID, err, operationID)
	}
	if err := operationFailure(op); err != nil {
		return err
	}
	var sandbox struct {
		ID string `json:"id"`
	}
	if len(op.Results) != 1 || json.Unmarshal(op.Results[0].Sandbox, &sandbox) != nil || sandbox.ID == "" {
		return fmt.Errorf("operation %s succeeded but returned no sandbox ID; inspect with sandbox operation get %s", op.ID, op.ID)
	}
	sb, err := c.Get(cmd.Context(), sandbox.ID)
	if err != nil {
		return fmt.Errorf("sandbox %s succeeded in operation %s, but fetching ready details failed: %w", sandbox.ID, op.ID, err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "sandbox %s ready\n", sb.ID)
	return writeOperationJSON(cmd.OutOrStdout(), sb)
}
