package report

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
)

func TestJUnitOneFailurePerFinding(t *testing.T) {
	r := Build(42, sampleAuditor(), nil)
	path := filepath.Join(t.TempDir(), "junit.xml")
	if err := r.WriteJUnit(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got junitSuites
	if err := xml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, raw)
	}
	s := got.Suites[0]
	if s.Tests != len(r.Findings) || s.Failures != len(r.Findings) || len(s.Cases) != len(r.Findings) {
		t.Fatalf("tests=%d failures=%d cases=%d, want %d each", s.Tests, s.Failures, len(s.Cases), len(r.Findings))
	}
	first := s.Cases[0]
	if first.Failure == nil || first.Failure.Type != string(audit.High) || !strings.Contains(first.Failure.Body, "pi_A") {
		t.Fatalf("first case should be the HIGH finding naming pi_A: %+v", first)
	}
}

func TestJUnitCleanRunHasPassingCase(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(1, audit.New(nil, 0), nil).EncodeJUnit(&buf); err != nil {
		t.Fatal(err)
	}
	var got junitSuites
	if err := xml.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if s := got.Suites[0]; s.Tests != 1 || s.Failures != 0 || s.Cases[0].Failure != nil {
		t.Fatalf("clean run: %+v", s)
	}
}

func TestMarkdown(t *testing.T) {
	var buf bytes.Buffer
	Build(42, sampleAuditor(), nil).WriteMarkdown(&buf)
	out := buf.String()
	for _, want := range []string{"## paychaos report (seed 42)", "**1 high**", "| HIGH | `" + audit.CodeUnsafeRetry, "pi_A, pi_B", "`drop_after_commit` × 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	Build(1, audit.New(nil, 0), nil).WriteMarkdown(&buf)
	if !strings.Contains(buf.String(), "No retry-safety problems") {
		t.Fatalf("clean run:\n%s", buf.String())
	}
}

func TestMdCellEscapes(t *testing.T) {
	if got := mdCell("a|b\nc"); got != `a\|b c` {
		t.Fatalf("got %q", got)
	}
}
