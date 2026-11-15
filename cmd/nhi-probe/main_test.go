package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestCtrlCExits17 builds the real binary, starts long-running commands, sends
// SIGINT (what Ctrl-C does) and checks: exit code 17, within 2 seconds.
func TestCtrlCExits17(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "nhi-probe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	tests := []struct {
		name   string
		events string
		args   []string
	}{
		{"proxy stdio", "e1.jsonl", []string{"cat"}},
		{"proxy http", "e2.jsonl", []string{"--http", "127.0.0.1:0", "--upstream", "http://127.0.0.1:1/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := filepath.Join(dir, tt.events)
			args := append([]string{"proxy", "--events", events}, tt.args...)
			cmd := exec.Command(bin, args...)
			stdin, _ := cmd.StdinPipe() // keep stdin open, like an agent would
			defer stdin.Close()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Ready = the events file exists. The proxy creates it AFTER installing its
			// Ctrl-C handler. (A fixed sleep failed: macOS scans a new binary on first run,
			// so a signal sent too early hit the default handler and killed the process.)
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(events); err == nil {
					break
				}
				if time.Now().After(deadline) {
					cmd.Process.Kill()
					t.Fatal("proxy never created its events file")
				}
			}

			start := time.Now()
			cmd.Process.Signal(syscall.SIGINT)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 17 {
					t.Fatalf("want exit code 17, got %v", err)
				}
				if took := time.Since(start); took > 2*time.Second {
					t.Errorf("took %v, want < 2s", took)
				}
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				t.Fatal("did not exit within 3s of SIGINT")
			}
		})
	}
}
