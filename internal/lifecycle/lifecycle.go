// Package lifecycle handles Ctrl-C: cancel everything, then exit with code 17.
package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ExitInterrupted is the exit code the brief asks for after Ctrl-C.
const ExitInterrupted = 17

// hardLimit: if a clean shutdown takes longer than this, force exit.
// The brief allows 2 seconds; 1.5 s leaves a safety margin.
const hardLimit = 1500 * time.Millisecond

// Context returns a context that is cancelled when the user presses Ctrl-C (SIGINT)
// or the process gets SIGTERM. Call stop() when the program finishes normally.
//
// It starts one goroutine (the "signal watcher"). On a signal it cancels the context,
// then waits hardLimit; if the program is still running, it force-exits with code 17.
func Context() (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-sigs:
			fmt.Fprintln(os.Stderr, "nhi-probe: interrupted, stopping...")
			cancel()
			time.Sleep(hardLimit)
			fmt.Fprintln(os.Stderr, "nhi-probe: shutdown too slow, forcing exit")
			os.Exit(ExitInterrupted)
		case <-ctx.Done():
			// normal finish: stop() was called by main
		}
	}()

	return ctx, func() {
		signal.Stop(sigs)
		cancel()
	}
}
