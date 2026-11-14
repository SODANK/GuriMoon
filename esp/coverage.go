package esp

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gurimoon/rpccov"
)

// RPC coverage panel: every call the client made, what the dispatcher did with it, and every
// client-side traceback that arrived afterwards.
//
// Calls the client can make but never has come from cmd/rpc-coverage, which scans the client
// script decompiles offline and writes expectedPath. That file is optional; without it the page
// shows what was observed and cannot show what is missing.

// expectedPath is where cmd/rpc-coverage writes the client's full RPC surface. It is relative,
// so it resolves against the server's working directory like the coverage ledger.
const expectedPath = "data/rpc-expected.json"

// expectedCall is one entry of the offline scan's output.
type expectedCall struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	// Sites is how many places in the client can make this call, a rough proxy for how many UI
	// surfaces break while it is missing.
	Sites int `json:"sites"`
}

// expectedFile is the document cmd/rpc-coverage writes.
type expectedFile struct {
	GeneratedAt time.Time      `json:"generatedAt"`
	Source      string         `json:"source"`
	Calls       []expectedCall `json:"calls"`
}

// covRow is one rendered ledger row.
type covRow struct {
	Service  string
	Method   string
	Count    int64
	Outcome  string
	Class    string // CSS class driving the row colour
	LastSeen string
}

// covMissing is one rendered "client can call this, we have never seen it" row.
type covMissing struct {
	Service string
	Method  string
	Sites   int
	// Implemented is true when the repo has a handler for the method though no client has
	// exercised it: a coverage gap rather than a server gap.
	Implemented bool
}

// covTrace is one rendered client traceback.
type covTrace struct {
	Culprit  string
	Text     string
	Context  []string
	Count    int64
	LastSeen string
}

// handleCoverage renders the RPC coverage panel.
func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if r.FormValue("action") == "reset" {
			rpccov.Reset()
			_ = rpccov.Flush()
		}
		http.Redirect(w, r, "/coverage", http.StatusSeeOther)
		return
	}

	// "gaps" is the default view: the short list of things to fix, not the long list that works.
	show := r.URL.Query().Get("show")
	if show == "" {
		show = "gaps"
	}

	entries := rpccov.Entries()
	seen := make(map[string]bool, len(entries))

	var rows []covRow
	for _, e := range entries {
		seen[strings.ToLower(e.Service+"."+e.Method)] = true
		isGap := e.Outcome != "handled"
		if show == "gaps" && !isGap {
			continue
		}
		if show == "handled" && isGap {
			continue
		}
		rows = append(rows, covRow{
			Service:  e.Service,
			Method:   e.Method,
			Count:    e.Count,
			Outcome:  e.Outcome,
			Class:    covClass(e.Outcome),
			LastSeen: e.LastSeen.UTC().Format("2006-01-02 15:04"),
		})
	}

	// Never-seen bucket, if the offline scan has run. The "Implemented" hint matches by method name
	// only, because the client reaches many methods through bound objects where the service name
	// on the wire is empty.
	var missing []covMissing
	var expectedAt string
	var expectedTotal int
	if exp, err := loadExpected(expectedPath); err == nil {
		expectedAt = exp.GeneratedAt.UTC().Format("2006-01-02 15:04")
		expectedTotal = len(exp.Calls)
		methodSeen := make(map[string]bool, len(entries))
		for _, e := range entries {
			methodSeen[strings.ToLower(e.Method)] = true
		}
		for _, c := range exp.Calls {
			if seen[strings.ToLower(c.Service+"."+c.Method)] {
				continue
			}
			// A bound-object call is recorded under "(bound)", so a match on
			// method name alone still counts as observed.
			if methodSeen[strings.ToLower(c.Method)] {
				continue
			}
			missing = append(missing, covMissing{Service: c.Service, Method: c.Method, Sites: c.Sites})
		}
		sort.Slice(missing, func(i, j int) bool {
			if missing[i].Sites != missing[j].Sites {
				return missing[i].Sites > missing[j].Sites
			}
			return missing[i].Service+missing[i].Method < missing[j].Service+missing[j].Method
		})
	}

	var traces []covTrace
	for _, t := range rpccov.Traces() {
		traces = append(traces, covTrace{
			Culprit:  t.Culprit,
			Text:     t.Text,
			Context:  t.Context,
			Count:    t.Count,
			LastSeen: t.LastSeen.UTC().Format("2006-01-02 15:04"),
		})
	}

	stats := rpccov.Summary()
	s.render(w, "coverage.html", map[string]any{
		"Rows":          rows,
		"Missing":       missing,
		"Traces":        traces,
		"Stats":         stats,
		"Show":          show,
		"ExpectedAt":    expectedAt,
		"ExpectedTotal": expectedTotal,
		"ExpectedPath":  expectedPath,
		"Page":          "coverage",
	})
}

// handleCoverageExport serves the raw ledger as JSON so cmd/rpc-coverage can
// join against a running server without waiting for the next periodic save.
func (s *Server) handleCoverageExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": rpccov.Entries(),
		"traces":  rpccov.Traces(),
		"stats":   rpccov.Summary(),
	})
}

// loadExpected reads the offline scan's output.
func loadExpected(path string) (*expectedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exp expectedFile
	if err := json.Unmarshal(data, &exp); err != nil {
		return nil, err
	}
	return &exp, nil
}

// covClass maps an outcome to the row's severity class.
func covClass(outcome string) string {
	switch outcome {
	case "unhandled":
		return "cov-bad"
	case "fallthrough", "stub":
		return "cov-silent"
	default:
		return "cov-ok"
	}
}
