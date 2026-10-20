package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
)

// Load reads a JSON report written by WriteJSON.
func Load(path string) (Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return r, nil
}

// Bucket counts findings that share a code, severity and path. Request
// numbers and object ids change between runs, so findings are compared by
// bucket rather than one by one.
type Bucket struct {
	Code     string         `json:"code"`
	Severity audit.Severity `json:"severity"`
	Path     string         `json:"path"`
	Baseline int            `json:"baseline"`
	Current  int            `json:"current"`
}

// Diff is the result of comparing a run against a baseline.
type Diff struct {
	BaselineSeed uint64 `json:"baseline_seed"`
	CurrentSeed  uint64 `json:"current_seed"`
	// New lists buckets with more findings than the baseline.
	New []Bucket `json:"new"`
	// Fixed lists buckets with fewer findings than the baseline.
	Fixed []Bucket `json:"fixed"`
}

// Compare buckets both reports' findings and returns what got worse or better.
// It is only meaningful when both runs used the same seed, scenario and
// workload; otherwise different faults fire and the counts are not comparable.
func Compare(baseline, current Report) Diff {
	type key struct {
		code string
		sev  audit.Severity
		path string
	}
	buckets := map[key]*Bucket{}
	get := func(f audit.Finding) *Bucket {
		k := key{f.Code, f.Severity, f.Path}
		if buckets[k] == nil {
			buckets[k] = &Bucket{Code: f.Code, Severity: f.Severity, Path: f.Path}
		}
		return buckets[k]
	}
	for _, f := range baseline.Findings {
		get(f).Baseline++
	}
	for _, f := range current.Findings {
		get(f).Current++
	}

	d := Diff{BaselineSeed: baseline.Seed, CurrentSeed: current.Seed}
	for _, b := range buckets {
		switch {
		case b.Current > b.Baseline:
			d.New = append(d.New, *b)
		case b.Current < b.Baseline:
			d.Fixed = append(d.Fixed, *b)
		}
	}
	sortBuckets(d.New)
	sortBuckets(d.Fixed)
	return d
}

var severityRank = map[audit.Severity]int{audit.High: 0, audit.Medium: 1, audit.Low: 2}

func sortBuckets(bs []Bucket) {
	sort.Slice(bs, func(i, j int) bool {
		if severityRank[bs[i].Severity] != severityRank[bs[j].Severity] {
			return severityRank[bs[i].Severity] < severityRank[bs[j].Severity]
		}
		if bs[i].Code != bs[j].Code {
			return bs[i].Code < bs[j].Code
		}
		return bs[i].Path < bs[j].Path
	})
}

// Regressed reports whether any new finding is at least as severe as min.
func (d Diff) Regressed(min audit.Severity) bool {
	for _, b := range d.New {
		if severityRank[b.Severity] <= severityRank[min] {
			return true
		}
	}
	return false
}

// WriteText prints the comparison.
func (d Diff) WriteText(w io.Writer) {
	if d.BaselineSeed != d.CurrentSeed {
		fmt.Fprintf(w, "warning: baseline seed %d differs from current seed %d; counts may not be comparable\n\n",
			d.BaselineSeed, d.CurrentSeed)
	}
	if len(d.New) == 0 && len(d.Fixed) == 0 {
		fmt.Fprintln(w, "No change in findings against the baseline.")
		return
	}
	for _, b := range d.New {
		fmt.Fprintf(w, "+ [%s] %s %s: %d -> %d\n", b.Severity, b.Code, b.Path, b.Baseline, b.Current)
	}
	for _, b := range d.Fixed {
		fmt.Fprintf(w, "- [%s] %s %s: %d -> %d\n", b.Severity, b.Code, b.Path, b.Baseline, b.Current)
	}
}
