package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

func printReport(writer io.Writer, report Report) error {
	if _, err := fmt.Fprintf(writer, "Mini-RPC benchmark\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Commit: %s\n", report.Environment.Commit); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Go: %s  OS: %s/%s  CPUs: %d  GOMAXPROCS: %d\n",
		report.Environment.GoVersion,
		report.Environment.GOOS,
		report.Environment.GOARCH,
		report.Environment.NumCPU,
		report.Environment.GOMAXPROCS,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "CPU: %s\n", report.Environment.CPUModel); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Kernel: %s  Host: %s  Memory: %.1f GiB\n\n",
		report.Environment.Kernel,
		report.Environment.Hostname,
		float64(report.Environment.TotalMemory)/(1<<30),
	); err != nil {
		return err
	}

	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "CASE\tREQ\tERROR\tQPS\tP50\tP95\tP99\tCONNS\tCPU_CORES\tHEAP\tRSS\tDISTRIBUTION"); err != nil {
		return err
	}
	for _, result := range report.Results {
		if _, err := fmt.Fprintf(
			table,
			"%s\t%d\t%.3f%%\t%.0f\t%s\t%s\t%s\t%d\t%.1f\t%.1fMiB\t%.1fMiB\t%s\n",
			result.Case,
			result.Requests,
			result.ErrorRate*100,
			result.QPS,
			formatDuration(result.P50),
			formatDuration(result.P95),
			formatDuration(result.P99),
			result.Connections,
			result.CPUCores,
			float64(result.HeapAllocBytes)/(1<<20),
			float64(result.RSSBytes)/(1<<20),
			result.Distribution,
		); err != nil {
			return err
		}
	}
	return table.Flush()
}

func formatDuration(value time.Duration) string {
	if value <= 0 {
		return "-"
	}
	if value < time.Millisecond {
		return fmt.Sprintf("%dus", value.Microseconds())
	}
	return fmt.Sprintf("%.2fms", float64(value)/float64(time.Millisecond))
}
