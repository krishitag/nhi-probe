package detect

import (
	"strings"
	"testing"
)

// Fake tokens are built at run time, so no token-looking string sits in the code.
var (
	fakeFineGrained = "github_pat_" + strings.Repeat("a", 82)
	fakeClassic     = "ghp_" + strings.Repeat("b", 36)
)

func TestScan(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int // how many findings
	}{
		{"fine-grained PAT", "token=" + fakeFineGrained, 1},
		{"classic PAT", fakeClassic, 1},
		{"both", fakeFineGrained + " " + fakeClassic, 2},
		{"too short", "ghp_" + strings.Repeat("b", 10), 0},
		{"nothing", "hello world", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scan(tt.text)
			if len(got) != tt.want {
				t.Fatalf("Scan found %d, want %d", len(got), tt.want)
			}
			for _, f := range got {
				if f.Provider != "github" {
					t.Errorf("provider = %q, want github", f.Provider)
				}
			}
		})
	}
}

// One fake (built at run time, never valid) per provider.
func TestScanProviders(t *testing.T) {
	r := strings.Repeat
	tests := []struct {
		provider string
		text     string
	}{
		{"github", "ghp_" + r("x", 36)},
		{"aws", "id=AKIA" + r("Q", 16) + " "},
		{"slack", "xoxb-" + r("1", 12)},
		{"notion", "ntn_" + r("n", 46)},
		{"openai", "sk-proj-" + r("o", 40)},
		{"anthropic", "sk-ant-api03-" + r("a", 90)},
		{"google", "AIza" + r("g", 35)},
		{"stripe", "sk_live_" + r("s", 30)},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := Scan(tt.text)
			if len(got) != 1 || got[0].Provider != tt.provider {
				t.Fatalf("Scan(%s fake) = %+v, want one %s finding", tt.provider, got, tt.provider)
			}
		})
	}
}

// Fuzz test: Scan must never panic, and every finding must really be in the input.
func FuzzScan(f *testing.F) {
	f.Add("ghp_" + strings.Repeat("x", 36))
	f.Add("AKIA" + strings.Repeat("Q", 16))
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		for _, fd := range Scan(text) {
			if !strings.Contains(text, fd.Secret) {
				t.Fatalf("finding %q not in input", fd.Secret)
			}
		}
	})
}

func TestIsSecretName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"GITHUB_PERSONAL_ACCESS_TOKEN", true},
		{"NOTION_API_KEY", true},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"STRIPE_KEY", true},
		{"db_password", true},
		{"KEYBOARD_LAYOUT", false},
		{"PATH", false},
		{"HOME", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSecretName(tt.name); got != tt.want {
				t.Errorf("IsSecretName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
