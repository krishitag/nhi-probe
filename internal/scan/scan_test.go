package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// makeTree writes some files with fake tokens (built at run time) into a temp folder.
func makeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fake := "ghp_" + strings.Repeat("z", 36)
	for i := range 40 {
		sub := filepath.Join(dir, fmt.Sprintf("d%d", i%5))
		os.MkdirAll(sub, 0o755)
		body := "nothing here\n"
		if i%7 == 0 {
			body = "export GITHUB_TOKEN=" + fake + "\n" // same secret in several files -> deduped
		}
		os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%d.txt", i)), []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 3}, 0o644) // binary: skipped
	return dir
}

func TestScanSameOutputForAnyWorkerCount(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := makeTree(t)

	var first []byte
	for _, w := range []string{"1", "4", "8", "16"} {
		out := filepath.Join(t.TempDir(), "scan.json")
		err := Run(context.Background(), []string{"--agents=false", "--workers", w, "--out", out, dir}, io.Discard)
		if err != nil {
			t.Fatalf("workers=%s: %v", w, err)
		}
		data, _ := os.ReadFile(out)
		if first == nil {
			first = data
		} else if !bytes.Equal(first, data) {
			t.Fatalf("workers=%s gave different output", w)
		}
	}

	var r Report
	if err := json.Unmarshal(first, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Secrets) != 1 || len(r.Secrets[0].Locations) != 6 {
		t.Fatalf("want 1 secret in 6 files, got %d secrets", len(r.Secrets))
	}
	if r.FilesSkipped != 1 {
		t.Errorf("want 1 skipped (binary), got %d", r.FilesSkipped)
	}
	if strings.Contains(string(first), strings.Repeat("z", 36)) {
		t.Fatal("report contains the full secret!")
	}
}

func TestScanCancelWritesPartialReport(t *testing.T) {
	defer goleak.VerifyNone(t)
	dir := makeTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already "Ctrl-C'd"

	out := filepath.Join(t.TempDir(), "scan.json")
	err := Run(ctx, []string{"--agents=false", "--out", out, dir}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	var r Report
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("partial report is not valid JSON: %v", err)
	}
	if !r.Partial {
		t.Error("report should say partial: true")
	}
}
