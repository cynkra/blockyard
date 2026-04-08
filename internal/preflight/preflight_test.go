package preflight

import (
	"encoding/json"
	"testing"
)

func TestReportHasErrors(t *testing.T) {
	t.Run("empty report", func(t *testing.T) {
		r := &Report{}
		if r.HasErrors() {
			t.Error("empty report should not have errors")
		}
	})

	t.Run("warnings only", func(t *testing.T) {
		r := &Report{}
		r.Add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		r.Add(Result{Name: "b", Severity: SeverityInfo, Message: "info"})
		if r.HasErrors() {
			t.Error("report with only warnings/info should not have errors")
		}
	})

	t.Run("contains error", func(t *testing.T) {
		r := &Report{}
		r.Add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		r.Add(Result{Name: "b", Severity: SeverityError, Message: "err"})
		if !r.HasErrors() {
			t.Error("report with an error should have errors")
		}
	})
}

func TestReportHasWarnings(t *testing.T) {
	t.Run("ok only", func(t *testing.T) {
		r := &Report{}
		r.Add(Result{Name: "a", Severity: SeverityOK, Message: "ok"})
		if r.HasWarnings() {
			t.Error("report with only OK results should not have warnings")
		}
	})

	t.Run("has warning", func(t *testing.T) {
		r := &Report{}
		r.Add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		if !r.HasWarnings() {
			t.Error("report with a warning should have warnings")
		}
	})

	t.Run("has error", func(t *testing.T) {
		r := &Report{}
		r.Add(Result{Name: "a", Severity: SeverityError, Message: "err"})
		if !r.HasWarnings() {
			t.Error("report with an error should have warnings")
		}
	})
}

func TestReportAdd(t *testing.T) {
	r := &Report{}

	r.Add(Result{Name: "ok", Severity: SeverityOK, Message: "msg"})
	r.Add(Result{Name: "warn", Severity: SeverityWarning, Message: "msg"})
	r.Add(Result{Name: "err", Severity: SeverityError, Message: "msg"})
	r.Add(Result{Name: "info", Severity: SeverityInfo, Message: "msg"})

	if len(r.Results) != 4 {
		t.Errorf("expected 4 results, got %d", len(r.Results))
	}
	if r.Summary.OK != 1 {
		t.Errorf("expected 1 OK, got %d", r.Summary.OK)
	}
	if r.Summary.Warnings != 1 {
		t.Errorf("expected 1 Warning, got %d", r.Summary.Warnings)
	}
	if r.Summary.Errors != 1 {
		t.Errorf("expected 1 Error, got %d", r.Summary.Errors)
	}
	if r.Summary.Info != 1 {
		t.Errorf("expected 1 Info, got %d", r.Summary.Info)
	}
}

func TestSeverityString(t *testing.T) {
	tests := []struct {
		s    Severity
		want string
	}{
		{SeverityOK, "ok"},
		{SeverityInfo, "info"},
		{SeverityWarning, "warning"},
		{SeverityError, "error"},
		{Severity(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestSeverityMarshalJSON(t *testing.T) {
	tests := []struct {
		s    Severity
		want string
	}{
		{SeverityOK, `"ok"`},
		{SeverityInfo, `"info"`},
		{SeverityWarning, `"warning"`},
		{SeverityError, `"error"`},
	}
	for _, tt := range tests {
		data, err := json.Marshal(tt.s)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", tt.s, err)
		}
		if string(data) != tt.want {
			t.Errorf("Marshal(%v) = %s, want %s", tt.s, data, tt.want)
		}
	}
}

func TestSeverityUnmarshalJSON(t *testing.T) {
	tests := []struct {
		input string
		want  Severity
	}{
		{`"ok"`, SeverityOK},
		{`"info"`, SeverityInfo},
		{`"warning"`, SeverityWarning},
		{`"error"`, SeverityError},
	}
	for _, tt := range tests {
		var s Severity
		if err := json.Unmarshal([]byte(tt.input), &s); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tt.input, err)
		}
		if s != tt.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tt.input, s, tt.want)
		}
	}
}

func TestSeverityUnmarshalJSON_Errors(t *testing.T) {
	var s Severity

	// Unknown severity string.
	if err := json.Unmarshal([]byte(`"critical"`), &s); err == nil {
		t.Error("expected error for unknown severity")
	}

	// Invalid JSON.
	if err := json.Unmarshal([]byte(`123`), &s); err == nil {
		t.Error("expected error for non-string JSON")
	}
}

func TestReportRecount(t *testing.T) {
	r := &Report{
		Results: []Result{
			{Severity: SeverityOK},
			{Severity: SeverityWarning},
			{Severity: SeverityWarning},
			{Severity: SeverityError},
			{Severity: SeverityInfo},
		},
	}

	r.recount()

	if r.Summary.OK != 1 {
		t.Errorf("OK = %d, want 1", r.Summary.OK)
	}
	if r.Summary.Warnings != 2 {
		t.Errorf("Warnings = %d, want 2", r.Summary.Warnings)
	}
	if r.Summary.Errors != 1 {
		t.Errorf("Errors = %d, want 1", r.Summary.Errors)
	}
	if r.Summary.Info != 1 {
		t.Errorf("Info = %d, want 1", r.Summary.Info)
	}
}

func TestReportLog(t *testing.T) {
	r := &Report{}
	r.Add(Result{Name: "a", Severity: SeverityOK, Message: "ok"})
	r.Add(Result{Name: "b", Severity: SeverityWarning, Message: "warn"})
	r.Add(Result{Name: "c", Severity: SeverityError, Message: "err"})
	r.Add(Result{Name: "d", Severity: SeverityInfo, Message: "info"})

	// Log should not panic. We're just covering the method.
	r.Log()
}

func TestReportJSONRoundTrip(t *testing.T) {
	r := &Report{}
	r.Add(Result{Name: "a", Severity: SeverityWarning, Message: "test", Category: "config"})

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}

	var r2 Report
	if err := json.Unmarshal(data, &r2); err != nil {
		t.Fatal(err)
	}
	if len(r2.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(r2.Results))
	}
	if r2.Results[0].Severity != SeverityWarning {
		t.Errorf("Severity = %v, want Warning", r2.Results[0].Severity)
	}
}
