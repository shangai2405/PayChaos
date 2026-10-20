package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/chaos"
)

func sampleAuditor() *audit.Auditor {
	a := audit.New(chaos.DefaultMutatingPaths, 0)
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "drop", chaos.FaultDropAfterCommit, 200, true, "pi_A")
	a.Complete(a.Begin("POST", "/v1/payment_intents", "", "fp"), "", chaos.FaultNone, 200, true, "pi_B")
	a.Complete(a.Begin("POST", "/v1/refunds", "k", "fp2"), "429", chaos.FaultRateLimit, 0, false, "")
	return a
}

func TestBuildCountsFaultsAndHigh(t *testing.T) {
	r := Build(42, sampleAuditor(), nil)
	if r.Seed != 42 || len(r.Requests) != 3 {
		t.Fatalf("seed/requests: %d %d", r.Seed, len(r.Requests))
	}
	if r.FaultsApplied[chaos.FaultDropAfterCommit] != 1 || r.FaultsApplied[chaos.FaultRateLimit] != 1 || len(r.FaultsApplied) != 2 {
		t.Fatalf("faults: %v", r.FaultsApplied)
	}
	if !r.HasHigh() {
		t.Fatal("expected HIGH")
	}
	if Build(1, audit.New(nil, 0), nil).HasHigh() {
		t.Fatal("empty run has no HIGH")
	}
}

func TestEmptyFindingsSerialiseAsArray(t *testing.T) {
	b, _ := json.Marshal(Build(1, audit.New(nil, 0), nil))
	if !strings.Contains(string(b), `"findings":[]`) {
		t.Fatalf("findings should be [] not null: %s", b)
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := Build(7, sampleAuditor(), nil).WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Seed != 7 || len(back.Findings) == 0 || back.Findings[0].Severity != audit.High {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if len(back.Findings[0].ObjectIDs) != 2 {
		t.Fatalf("object ids lost: %v", back.Findings[0].ObjectIDs)
	}
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	Build(42, sampleAuditor(), nil).WriteText(&buf)
	out := buf.String()
	for _, want := range []string{"seed 42", "Audited payment requests: 3", "1 high", "[HIGH] " + audit.CodeUnsafeRetry} {
		if !strings.Contains(out, want) {
			t.Fatalf("text report missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	Build(1, audit.New(nil, 0), nil).WriteText(&buf)
	if !strings.Contains(buf.String(), "No problems detected") {
		t.Fatalf("clean run message missing:\n%s", buf.String())
	}
}
