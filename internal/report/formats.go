package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/shangai2405/Chaos-tester-for-payment-integrations/internal/audit"
)

// JUnit output follows the de facto schema most CI systems (GitHub Actions
// reporters, GitLab, Jenkins, CircleCI) read: one test case per finding, each
// marked as a failure, so findings show up next to ordinary test results.
type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Cases     []junitCase `xml:"testcase"`
	SystemOut string      `xml:"system-out,omitempty"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// WriteJUnit writes the findings as JUnit XML to path. A run with no findings
// produces a single passing test case so the suite is never empty.
func (r Report) WriteJUnit(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := r.EncodeJUnit(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// EncodeJUnit writes the JUnit XML document to w.
func (r Report) EncodeJUnit(w io.Writer) error {
	s := junitSuite{
		Name:      fmt.Sprintf("paychaos (seed %d)", r.Seed),
		Timestamp: r.GeneratedAt.Format("2006-01-02T15:04:05"),
	}
	for i, f := range r.Findings {
		body := f.Message
		if len(f.ObjectIDs) > 0 {
			body += "\nobjects: " + strings.Join(f.ObjectIDs, ", ")
		}
		if len(f.Requests) > 0 {
			body += "\nrequests: " + joinInts(f.Requests)
		}
		s.Cases = append(s.Cases, junitCase{
			Name:      fmt.Sprintf("%03d %s %s", i+1, f.Code, f.Path),
			Classname: "paychaos." + strings.ToLower(string(f.Severity)),
			Failure:   &junitFailure{Message: f.Code, Type: string(f.Severity), Body: body},
		})
		s.Failures++
	}
	if len(s.Cases) == 0 {
		s.Cases = []junitCase{{Name: "no findings", Classname: "paychaos"}}
	}
	s.Tests = len(s.Cases)
	if len(r.WebhookNotes) > 0 {
		s.SystemOut = "webhook notes:\n- " + strings.Join(r.WebhookNotes, "\n- ")
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(junitSuites{Suites: []junitSuite{s}}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteMarkdown writes a summary suited to a pull request comment or a CI job
// summary (e.g. append it to $GITHUB_STEP_SUMMARY).
func (r Report) WriteMarkdown(w io.Writer) {
	counts := map[audit.Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	fmt.Fprintf(w, "## paychaos report (seed %d)\n\n", r.Seed)
	fmt.Fprintf(w, "**%d** audited payment requests · **%d high**, %d medium, %d low findings\n\n",
		len(r.Requests), counts[audit.High], counts[audit.Medium], counts[audit.Low])

	if len(r.FaultsApplied) > 0 {
		faults := make([]string, 0, len(r.FaultsApplied))
		for f, n := range r.FaultsApplied {
			faults = append(faults, fmt.Sprintf("`%s` × %d", f, n))
		}
		sort.Strings(faults)
		fmt.Fprintf(w, "Faults injected: %s\n\n", strings.Join(faults, ", "))
	}

	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "No retry-safety problems detected in this run.")
	} else {
		fmt.Fprintln(w, "| Severity | Code | Path | Requests | Objects | Detail |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|")
		for _, f := range r.Findings {
			fmt.Fprintf(w, "| %s | `%s` | `%s` | %s | %s | %s |\n",
				f.Severity, f.Code, f.Path, joinInts(f.Requests), mdCell(strings.Join(f.ObjectIDs, ", ")), mdCell(f.Message))
		}
	}

	if len(r.WebhookNotes) > 0 {
		fmt.Fprintf(w, "\n### Webhooks (%d events)\n\n", len(r.Webhooks))
		for _, n := range r.WebhookNotes {
			fmt.Fprintf(w, "- %s\n", n)
		}
	}
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprintf("#%d", x)
	}
	return strings.Join(s, ", ")
}

// mdCell keeps a value inside one Markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}
