package lifecycle_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

func TestVisibilityMatchesLifecycleState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		visibility lifecycle.Visibility
		inactive   bool
		want       bool
	}{
		{name: "current includes current", visibility: lifecycle.CurrentOnly, want: true},
		{name: "current excludes inactive", visibility: lifecycle.CurrentOnly, inactive: true},
		{name: "inactive excludes current", visibility: lifecycle.InactiveOnly},
		{name: "inactive includes inactive", visibility: lifecycle.InactiveOnly, inactive: true, want: true},
		{name: "all includes current", visibility: lifecycle.All, want: true},
		{name: "all includes inactive", visibility: lifecycle.All, inactive: true, want: true},
		{name: "unknown fails closed to current", visibility: lifecycle.Visibility(99), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.visibility.Matches(tt.inactive); got != tt.want {
				t.Fatalf("Matches(%v) = %v, want %v", tt.inactive, got, tt.want)
			}
		})
	}
}

func TestVisibilitySQLCondition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		visibility lifecycle.Visibility
		want       string
	}{
		{visibility: lifecycle.CurrentOnly, want: "archived_at IS NULL"},
		{visibility: lifecycle.InactiveOnly, want: "archived_at IS NOT NULL"},
		{visibility: lifecycle.All, want: ""},
		{visibility: lifecycle.Visibility(99), want: "archived_at IS NULL"},
	}
	for _, tt := range tests {
		if got := tt.visibility.SQLCondition("archived_at"); got != tt.want {
			t.Fatalf("SQLCondition = %q, want %q", got, tt.want)
		}
	}
}
