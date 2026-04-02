package preflight

import "testing"

func TestReportHasErrors(t *testing.T) {
	t.Run("empty report", func(t *testing.T) {
		r := &Report{}
		if r.HasErrors() {
			t.Error("empty report should not have errors")
		}
	})

	t.Run("warnings only", func(t *testing.T) {
		r := &Report{}
		r.add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		r.add(Result{Name: "b", Severity: SeverityInfo, Message: "info"})
		if r.HasErrors() {
			t.Error("report with only warnings/info should not have errors")
		}
	})

	t.Run("contains error", func(t *testing.T) {
		r := &Report{}
		r.add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		r.add(Result{Name: "b", Severity: SeverityError, Message: "err"})
		if !r.HasErrors() {
			t.Error("report with an error should have errors")
		}
	})
}

func TestReportHasWarnings(t *testing.T) {
	t.Run("ok only", func(t *testing.T) {
		r := &Report{}
		r.add(Result{Name: "a", Severity: SeverityOK, Message: "ok"})
		if r.HasWarnings() {
			t.Error("report with only OK results should not have warnings")
		}
	})

	t.Run("has warning", func(t *testing.T) {
		r := &Report{}
		r.add(Result{Name: "a", Severity: SeverityWarning, Message: "warn"})
		if !r.HasWarnings() {
			t.Error("report with a warning should have warnings")
		}
	})

	t.Run("has error", func(t *testing.T) {
		r := &Report{}
		r.add(Result{Name: "a", Severity: SeverityError, Message: "err"})
		if !r.HasWarnings() {
			t.Error("report with an error should have warnings")
		}
	})
}

func TestReportAdd(t *testing.T) {
	r := &Report{}

	r.add(Result{Name: "ok", Severity: SeverityOK, Message: "msg"})
	r.add(Result{Name: "warn", Severity: SeverityWarning, Message: "msg"})
	r.add(Result{Name: "err", Severity: SeverityError, Message: "msg"})
	r.add(Result{Name: "info", Severity: SeverityInfo, Message: "msg"})

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
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}
