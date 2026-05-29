// Command kubectl-nm is the klusterOne CLI. When placed on PATH as
// `kubectl-nm`, kubectl discovers it as the `kubectl nm ...` plugin.
//
// Subcommands:
//
//	kubectl nm create <name> [flags]            build & apply a NodeMaintenance
//	kubectl nm attach <name> <script>           patch spec.script.inline on an existing NM
//	kubectl nm pause  <name> [--reason TEXT]    pause an in-flight NM
//	kubectl nm run    <name>                    unpause an existing NM
//	kubectl nm status <name>                    pretty-print phase + per-node table
//	kubectl nm logs   <name> [--node X]         stream runner-pod logs
//	kubectl nm push <local> <remote> [targets]  copy a local file onto nodes
//	kubectl nm pull <remote> <local> --node X   copy a node file back to local
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/AnirudhKabra/klusterOne/internal/cli"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	sub := os.Args[1]
	args := os.Args[2:]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var err error
	switch sub {
	case "create":
		err = cli.RunCreate(ctx, args)
	case "attach":
		err = cli.RunAttach(ctx, args)
	case "pause":
		err = cli.RunPause(ctx, args)
	case "run":
		err = cli.RunRun(ctx, args)
	case "status":
		err = cli.RunStatus(ctx, args)
	case "logs":
		err = cli.RunLogs(ctx, args)
	case "push":
		err = cli.RunPush(ctx, args)
	case "pull":
		err = cli.RunPull(ctx, args)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `kubectl nm — manage NodeMaintenance (ko.io) resources

Usage:
  kubectl nm create <name> [flags]
  kubectl nm attach <name> <script-path>
  kubectl nm pause  <name> [--reason <text>]
  kubectl nm run    <name>
  kubectl nm status <name>
  kubectl nm logs   <name> [--node <node>] [-f]
  kubectl nm push   <local-path> <remote-path> [--all-nodes|--selector|--nodes]
  kubectl nm pull   <remote-path> <local-path> --node <node>

Run "kubectl nm <subcommand> -h" for flag details.`)
}
