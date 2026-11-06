package probe

import (
	"errors"
	"testing"
)

func TestStatusPolicyAllows(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{0, false},
		{199, false},
		{200, true},
		{204, true},
		{299, true},
		{301, false},
		{404, false},
		{500, false},
	}
	for _, tt := range tests {
		if got := Default2xx.Allows(tt.code); got != tt.want {
			t.Errorf("Allows(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestStatusPolicyHealthy(t *testing.T) {
	tests := []struct {
		name string
		r    Result
		want bool
	}{
		{"200 ok", Result{StatusCode: 200}, true},
		{"500 server error", Result{StatusCode: 500}, false},
		{"302 redirect", Result{StatusCode: 302}, false},
		{"request failed", Result{Err: errors.New("boom")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Default2xx.Healthy(tt.r); got != tt.want {
				t.Errorf("Healthy(%+v) = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}
