// Package detect finds secrets in text, using our own signatures.
// Both proxy and scan use it.
package detect

import (
	"regexp"
	"strings"
)

// Finding is one secret we found.
type Finding struct {
	Provider string // e.g. "github", or "unknown" if found only by its name
	Secret   string // the full secret: never print this, fingerprint it
}

// Detector finds secrets of one provider in a piece of text.
// One small interface, so adding a provider = adding one Detector.
type Detector interface {
	Detect(text string) []Finding
}

// regexDetector is a Detector built from one regular expression, plus the fixed
// prefixes every match must contain.
type regexDetector struct {
	provider string
	prefixes []string // fast check first: no prefix in the text -> no regex at all
	re       *regexp.Regexp
}

// Detect: SCALING fix for scan. The regex engine was 73% of CPU on 10.9 GB, running all
// 8 regexes over every file. strings.Contains is a very fast (SIMD) substring search,
// and almost no file contains "ghp_" or "AKIA", so most files now skip every regex.
func (d regexDetector) Detect(text string) []Finding {
	hit := false
	for _, p := range d.prefixes {
		if strings.Contains(text, p) {
			hit = true
			break
		}
	}
	if !hit {
		return nil
	}
	var out []Finding
	for _, m := range d.re.FindAllString(text, -1) {
		out = append(out, Finding{Provider: d.provider, Secret: m})
	}
	return out
}

// detectors is the list of all known signatures: 8 providers agents commonly use.
// Each one: a fixed prefix (fast to spot, few false alarms) + the expected length/charset.
// Verify endpoints for each are listed in DOSSIER.md (scan never calls them).
var detectors = []Detector{
	// fine-grained PAT: github_pat_ + 82; classic: ghp_/gho_/ghu_/ghs_/ghr_ + 36
	regexDetector{"github", []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"},
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}|gh[pousr]_[A-Za-z0-9]{36}`)},
	// access key id: AKIA (long-term) or ASIA (temporary) + 16 upper/digits
	regexDetector{"aws", []string{"AKIA", "ASIA"}, regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	// bot/user/app tokens: xoxb-, xoxp-, xoxa-, xoxs-, xoxr-, xoxo-
	regexDetector{"slack", []string{"xox"}, regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`)},
	// new integration tokens ntn_..., older secret_... (43 chars)
	regexDetector{"notion", []string{"ntn_", "secret_"}, regexp.MustCompile(`\bntn_[A-Za-z0-9]{40,}|\bsecret_[A-Za-z0-9]{43}\b`)},
	// project / service-account / admin keys
	regexDetector{"openai", []string{"sk-proj-", "sk-svcacct-", "sk-admin-"}, regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}`)},
	// api / admin keys: sk-ant-api03-..., sk-ant-admin01-...
	regexDetector{"anthropic", []string{"sk-ant-"}, regexp.MustCompile(`sk-ant-(?:api|admin)\d{2}-[A-Za-z0-9_-]{80,}`)},
	// Google API key: AIza + 35
	regexDetector{"google", []string{"AIza"}, regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	// live secret / restricted keys
	regexDetector{"stripe", []string{"sk_live_", "rk_live_"}, regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{24,}`)},
}

// Scan runs every detector over the text.
func Scan(text string) []Finding {
	var out []Finding
	for _, d := range detectors {
		out = append(out, d.Detect(text)...)
	}
	return out
}

// key
// secretWords: an env var NAME containing one of these probably holds a secret.
var secretWords = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "CREDENTIAL", "PRIVATE_KEY"}

// backup check
// IsSecretName reports whether an env var name looks like it holds a secret.
// "_KEY" only counts at the end (AWS_ACCESS_KEY), so KEYBOARD_LAYOUT does not match.
func IsSecretName(name string) bool {
	up := strings.ToUpper(name)
	for _, w := range secretWords {
		if strings.Contains(up, w) {
			return true
		}
	}
	return strings.HasSuffix(up, "_KEY")
}
