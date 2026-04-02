package preflight

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Severity classifies the urgency of a check result.
// Values are ordered so that higher severity sorts first in descending order.
type Severity int

const (
	SeverityOK      Severity = iota // check passed
	SeverityInfo                    // informational (operator awareness)
	SeverityWarning                 // security hazard or likely misconfiguration
	SeverityError                   // blocks startup
)

// String returns the lowercase name of the severity.
func (s Severity) String() string {
	switch s {
	case SeverityOK:
		return "ok"
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return "unknown"
	}
}

// MarshalJSON serializes Severity as a lowercase JSON string.
func (s Severity) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON parses a Severity from a JSON string.
func (s *Severity) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	switch str {
	case "ok":
		*s = SeverityOK
	case "info":
		*s = SeverityInfo
	case "warning":
		*s = SeverityWarning
	case "error":
		*s = SeverityError
	default:
		return fmt.Errorf("unknown severity: %q", str)
	}
	return nil
}

// Result describes the outcome of a single check.
type Result struct {
	Name     string   `json:"name"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Category string   `json:"category"` // "config", "docker", "runtime"
}

// Summary counts results by severity.
type Summary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
	OK       int `json:"ok"`
}

// Report collects all check results.
type Report struct {
	RanAt   time.Time `json:"ran_at"`
	Results []Result  `json:"results"`
	Summary Summary   `json:"summary"`
}

// HasErrors returns true if any result has SeverityError.
func (r *Report) HasErrors() bool {
	return r.Summary.Errors > 0
}

// HasWarnings returns true if any result has SeverityWarning or above.
func (r *Report) HasWarnings() bool {
	return r.Summary.Errors > 0 || r.Summary.Warnings > 0
}

// recount rebuilds Summary from Results.
func (r *Report) recount() {
	r.Summary = Summary{}
	for _, res := range r.Results {
		switch res.Severity {
		case SeverityError:
			r.Summary.Errors++
		case SeverityWarning:
			r.Summary.Warnings++
		case SeverityInfo:
			r.Summary.Info++
		case SeverityOK:
			r.Summary.OK++
		}
	}
}

// Log emits each non-OK result via slog at the appropriate level.
func (r *Report) Log() {
	for _, res := range r.Results {
		if res.Severity == SeverityOK {
			continue
		}
		attrs := []any{"check", res.Name}
		switch res.Severity {
		case SeverityError:
			slog.Error("preflight: "+res.Message, attrs...)
		case SeverityWarning:
			slog.Warn("preflight: "+res.Message, attrs...)
		default:
			slog.Info("preflight: "+res.Message, attrs...)
		}
	}
}

// add appends a result to the report and updates the summary.
func (r *Report) add(res Result) {
	r.Results = append(r.Results, res)
	switch res.Severity {
	case SeverityError:
		r.Summary.Errors++
	case SeverityWarning:
		r.Summary.Warnings++
	case SeverityInfo:
		r.Summary.Info++
	case SeverityOK:
		r.Summary.OK++
	}
}
