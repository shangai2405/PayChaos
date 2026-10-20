package report

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
)

func finding(code string, sev audit.Severity, path string) audit.Finding {
	return audit.Finding{Code: code, Severity: sev, Path: path}
}

func TestCompareNewAndFixed(t *testing.T) {
	base := Report{Seed: 7, Findings: []audit.Finding{
		finding(audit.CodeMissingKey, audit.Medium, "/v1/refunds"),
		finding(audit.CodeNoBackoff, audit.Low, "/v1/payment_intents"),
		finding(audit.CodeNoBackoff, audit.Low, "/v1/payment_intents"),
	}}
	cur := Report{Seed: 7, Findings: []audit.Finding{
		finding(audit.CodeUnsafeRetry, audit.High, "/v1/payment_intents"),
		finding(audit.CodeMissingKey, audit.Medium, "/v1/refunds"),
		finding(audit.CodeNoBackoff, audit.Low, "/v1/payment_intents"),
	}}
	d := Compare(base, cur)
	if len(d.New) != 1 || d.New[0].Code != audit.CodeUnsafeRetry || d.New[0].Current != 1 {
		t.Fatalf("new: %+v", d.New)
	}
	if len(d.Fixed) != 1 || d.Fixed[0].Code != audit.CodeNoBackoff || d.Fixed[0].Baseline != 2 || d.Fixed[0].Current != 1 {
		t.Fatalf("fixed: %+v", d.Fixed)
	}
	if !d.Regressed(audit.High) || !d.Regressed(audit.Low) {
		t.Fatal("a new HIGH regresses at every threshold")
	}

	onlyLow := Compare(Report{}, Report{Findings: []audit.Finding{finding(audit.CodeNoBackoff, audit.Low, "/v1/charges")}})
	if onlyLow.Regressed(audit.High) || onlyLow.Regressed(audit.Medium) || !onlyLow.Regressed(audit.Low) {
		t.Fatalf("threshold handling: %+v", onlyLow)
	}
}

func TestCompareIdentical(t *testing.T) {
	r := Build(42, sampleAuditor(), nil)
	d := Compare(r, r)
	if len(d.New) != 0 || len(d.Fixed) != 0 || d.Regressed(audit.Low) {
		t.Fatalf("identical reports differ: %+v", d)
	}
	var buf bytes.Buffer
	d.WriteText(&buf)
	if !strings.Contains(buf.String(), "No change") {
		t.Fatal(buf.String())
	}
}

func TestCompareWarnsOnSeedMismatchAndLoads(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := Build(1, sampleAuditor(), nil).WriteJSON(a); err != nil {
		t.Fatal(err)
	}
	if err := Build(2, audit.New(nil, 0), nil).WriteJSON(b); err != nil {
		t.Fatal(err)
	}
	base, err := Load(a)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	Compare(base, cur).WriteText(&buf)
	out := buf.String()
	if !strings.Contains(out, "warning: baseline seed 1 differs") || !strings.Contains(out, "- [HIGH] "+audit.CodeUnsafeRetry) {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
