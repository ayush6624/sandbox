package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var metricsLimit int

func metricsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics <id>",
		Short: "Show a sandbox's recent CPU/memory/disk/network utilization",
		Long: "Recent utilization samples for a sandbox: what it is consuming, as\n" +
			"opposed to what it is billed for (see the usage ledger for that).\n\n" +
			"Samples live in the owning worker's memory for a bounded recent window,\n" +
			"so this shows live behavior rather than history. Reading is passive: a\n" +
			"hibernated sandbox keeps its samples, stops producing new ones, and is\n" +
			"not woken by this command.",
		Args: cobra.ExactArgs(1),
		RunE: runMetrics,
	}
	cmd.Flags().IntVar(&metricsLimit, "limit", 20, "show at most this many of the newest samples (0 = as many as the host keeps)")
	addClientFlags(cmd)
	return cmd
}

func runMetrics(cmd *cobra.Command, args []string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	m, err := c.Metrics(context.Background(), args[0], metricsLimit)
	if err != nil {
		return err
	}
	if len(m.Samples) == 0 {
		// Not an error: a just-created sandbox has not been sampled yet, and a
		// worker restart drops the window.
		fmt.Printf("no samples yet (state=%s, sampled every %gs)\n", m.State, m.IntervalSeconds)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tCPU%\tMEM\tDISK\tRX\tTX\tROOTFS")
	for _, s := range m.Samples {
		mem, disk := "-", "-"
		if s.MemTotalBytes > 0 {
			mem = fmt.Sprintf("%s/%s", human(s.MemUsedBytes), human(s.MemTotalBytes))
		} else if s.HostMemBytes > 0 {
			// No guest stats: report what the host is charged, marked so it is
			// not mistaken for the guest's own view.
			mem = human(s.HostMemBytes) + "~"
		}
		if s.DiskTotalBytes > 0 {
			disk = fmt.Sprintf("%s/%s", human(s.DiskUsedBytes), human(s.DiskTotalBytes))
		}
		fmt.Fprintf(tw, "%s\t%.1f\t%s\t%s\t%s\t%s\t%s\n",
			s.Timestamp.Local().Format("15:04:05"), s.CPUUsedPct, mem, disk,
			human(s.NetRxBytes), human(s.NetTxBytes), human(s.RootfsAllocBytes))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nstate=%s  vcpus=%d  interval=%gs  (mem marked ~ is the host's charge, not the guest's view)\n",
		m.State, m.Samples[len(m.Samples)-1].CPUCount, m.IntervalSeconds)
	return nil
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
