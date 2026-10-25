// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/xxyyue/llm-observer-proxy-go/internal/trajectory"
)

func runExport(args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	events := flags.String("events", "", "source events.jsonl (required)")
	output := flags.String("output", "", "destination JSON file (required)")
	options := trajectory.Options{}
	flags.StringVar(&options.Format, "format", "atif", "export format")
	flags.StringVar(&options.Version, "format-version", trajectory.Version, "ATIF version")
	flags.StringVar(&options.RunID, "run-id", "", "run ID (default: from source events)")
	flags.StringVar(&options.Metadata.Name, "agent-name", "", "agent name (default: unknown)")
	flags.StringVar(&options.Metadata.Version, "agent-version", "", "agent version (default: unknown)")
	flags.BoolVar(&options.Strict, "strict", false, "reject incomplete or ambiguous reconstruction")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *events == "" || *output == "" {
		return fmt.Errorf("--events and --output are required")
	}
	document, err := trajectory.ExportFile(*events, options)
	if err != nil {
		return err
	}
	if err := trajectory.WriteFile(*output, *events, document); err != nil {
		return err
	}
	if meta, ok := document.Extra[trajectory.Namespace].(map[string]any); ok {
		if diagnostics, ok := meta["diagnostics"].([]trajectory.Diagnostic); ok && len(diagnostics) > 0 {
			fmt.Fprintf(os.Stderr, "Export contains %d reconstruction warning(s); see extra.llm_observer_proxy.diagnostics or use --strict.\n", len(diagnostics))
		}
	}
	fmt.Fprintln(os.Stderr, "ATIF trajectory written to", *output)
	return nil
}
