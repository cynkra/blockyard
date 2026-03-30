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
		r := &Report{Results: []Result{
			{Name: "a", Severity: SeverityWarning, Message: "warn"},
			{Name: "b", Severity: SeverityInfo, Message: "info"},
		}}
		if r.HasErrors() {
			t.Error("report with only warnings/info should not have errors")
		}
	})

	t.Run("contains error", func(t *testing.T) {
		r := &Report{Results: []Result{
			{Name: "a", Severity: SeverityWarning, Message: "warn"},
			{Name: "b", Severity: SeverityError, Message: "err"},
		}}
		if !r.HasErrors() {
			t.Error("report with an error should have errors")
		}
	})
}

func TestReportAdd(t *testing.T) {
	r := &Report{}

	r.add(nil) // should be ignored
	if len(r.Results) != 0 {
		t.Error("nil result should not be added")
	}

	r.add(&Result{Name: "test", Severity: SeverityInfo, Message: "msg"})
	if len(r.Results) != 1 {
		t.Error("non-nil result should be added")
	}
}
