package review

import (
	"fmt"
	"regexp"
	"strings"
)

// Route thresholds come from a pilot on past pull requests: large or risky diffs gained from high effort,
// small ones did not. Tune them from judged eval results, not by guess.
const (
	routeMaxFiles = 10
	routeMaxAdded = 500
)

var (
	diffFile  = regexp.MustCompile(`(?m)^diff --git a/(\S+)`)
	addedLine = regexp.MustCompile(`(?m)^\+[^+]`)
	riskPath  = regexp.MustCompile(`db/migrate/|cache|lock|statable|/workers/|/jobs/`)
)

type Route struct {
	Effort  string   `json:"effort"`
	Reasons []string `json:"reasons"`
}

// RouteDiff picks high effort for large diffs or ones touching risk areas, low otherwise.
func RouteDiff(diff []byte) Route {
	text := string(diff)
	files := diffFile.FindAllStringSubmatch(text, -1)
	added := len(addedLine.FindAllStringIndex(text, -1))
	risky := 0
	for _, f := range files {
		if riskPath.MatchString(f[1]) && !strings.HasPrefix(f[1], "spec/") {
			risky++
		}
	}
	r := Route{Effort: "low", Reasons: []string{}}
	if len(files) >= routeMaxFiles {
		r.Reasons = append(r.Reasons, fmt.Sprintf("files=%d", len(files)))
	}
	if added >= routeMaxAdded {
		r.Reasons = append(r.Reasons, fmt.Sprintf("added=%d", added))
	}
	if risky > 0 {
		r.Reasons = append(r.Reasons, fmt.Sprintf("risk_paths=%d", risky))
	}
	if len(r.Reasons) > 0 {
		r.Effort = "high"
	}
	return r
}

// shouldEscalate re-reviews a low-effort result at high when it reports a high-severity finding.
func shouldEscalate(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == "high" {
			return true
		}
	}
	return false
}
