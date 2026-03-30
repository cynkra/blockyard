package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// jsonFlag extracts the --json flag from the command.
func jsonFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	return v
}

// printJSON encodes v as indented JSON to stdout.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// printRawJSON writes raw JSON bytes (from API passthrough) to stdout.
func printRawJSON(data []byte) {
	// Pretty-print if it's valid JSON.
	var v any
	if json.Unmarshal(data, &v) == nil {
		printJSON(v)
		return
	}
	_, _ = os.Stdout.Write(data)
	_, _ = os.Stdout.Write([]byte("\n"))
}

// exitError prints an error message and exits.
func exitError(jsonOutput bool, err error) {
	if jsonOutput {
		printJSON(map[string]string{
			"error":   "error",
			"message": err.Error(),
		})
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
	}
	os.Exit(1)
}

// exitErrorf prints a formatted error message and exits.
func exitErrorf(jsonOutput bool, format string, args ...any) {
	exitError(jsonOutput, fmt.Errorf(format, args...))
}

// newTabWriter creates a tabwriter for aligned table output.
func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// printKeyValue prints key-value pairs aligned with padding.
func printKeyValue(pairs [][2]string) {
	w := newTabWriter()
	for _, p := range pairs {
		fmt.Fprintf(w, "  %s:\t%s\n", p[0], p[1])
	}
	_ = w.Flush()
}

// streamResponse reads a streaming HTTP response (chunked text) and
// writes lines to the given writer as they arrive.
func streamResponse(resp io.ReadCloser, w io.Writer) error {
	buf := make([]byte, 4096)
	for {
		n, err := resp.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// derefStr dereferences a string pointer, returning a default if nil.
func derefStr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// derefFloat dereferences a float64 pointer, returning a default if nil.
func derefFloat(p *float64, def float64) string {
	if p == nil {
		return fmt.Sprintf("%.1f", def)
	}
	return fmt.Sprintf("%.1f", *p)
}

// joinOr joins strings with ", " and "or" before the last.
func joinOr(items []string) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// truncate shortens a string to max length with "..." suffix.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
