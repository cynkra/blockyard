package preflight

import "log/slog"

// Severity classifies the urgency of a preflight check result.
type Severity int

const (
	SeverityInfo    Severity = iota // informational (operator awareness)
	SeverityWarning                 // security hazard or likely misconfiguration
	SeverityError                   // blocks startup
)

// Result describes the outcome of a single preflight check.
type Result struct {
	Name     string   // short identifier, e.g. "no_oidc"
	Severity Severity // how urgent this finding is
	Message  string   // human-readable explanation
}

// Report collects all preflight check results.
type Report struct {
	Results []Result
}

// HasErrors returns true if any result has SeverityError.
func (r *Report) HasErrors() bool {
	for _, res := range r.Results {
		if res.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Log emits each result via slog at the appropriate level.
func (r *Report) Log() {
	for _, res := range r.Results {
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

// add appends a non-nil result to the report.
func (r *Report) add(res *Result) {
	if res != nil {
		r.Results = append(r.Results, *res)
	}
}
