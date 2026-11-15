package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/krishitag/nhi-probe/internal/hook"
	"github.com/krishitag/nhi-probe/internal/lifecycle"
	"github.com/krishitag/nhi-probe/internal/proxy"
	"github.com/krishitag/nhi-probe/internal/scan"
	"github.com/krishitag/nhi-probe/internal/trace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: nhi-probe <scan|proxy|trace|hook> ...")
		os.Exit(2)
	}

	// hook runs on every Claude Code hook event. It must be quick and must never
	// block the agent, so it skips the Ctrl-C machinery and always exits 0.
	if os.Args[1] == "hook" {
		if err := hook.Run(os.Args[2:], os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "nhi-probe hook:", err)
		}
		os.Exit(0)
	}

	// Soft memory limit: the garbage collector works harder as the heap nears 450 MB,
	// so peak memory stays under the brief's 512 MB. GOMEMLIMIT in the env overrides it.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(450 << 20)
	}

	// ctx is cancelled on Ctrl-C. Every command gets it.
	ctx, stop := lifecycle.Context()

	var err error
	switch os.Args[1] {
	case "proxy":
		err = proxy.Run(ctx, os.Args[2:], os.Stdin, os.Stdout)
	case "scan":
		err = scan.Run(ctx, os.Args[2:], os.Stdout)
	case "trace":
		err = trace.Run(ctx, os.Args[2:], os.Stdout)
	default:
		stop()
		fmt.Fprintln(os.Stderr, "unknown command:", os.Args[1])
		os.Exit(2)
	}

	interrupted := ctx.Err() != nil // check BEFORE stop(), because stop() also cancels ctx
	stop()

	if interrupted {
		fmt.Fprintln(os.Stderr, "nhi-probe: stopped by Ctrl-C, partial output saved")
		os.Exit(lifecycle.ExitInterrupted)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
