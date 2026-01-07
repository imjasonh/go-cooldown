package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for _, tt := range []struct {
		desc    string
		input   string
		want    time.Duration
		wantErr bool
	}{{
		desc:  "standard hours",
		input: "24h",
		want:  24 * time.Hour,
	}, {
		desc:  "days",
		input: "7d",
		want:  7 * 24 * time.Hour,
	}, {
		desc:  "months",
		input: "2M",
		want:  2 * 30 * 24 * time.Hour,
	}, {
		desc:  "years",
		input: "1y",
		want:  365 * 24 * time.Hour,
	}, {
		desc:  "fractional days",
		input: "0.5d",
		want:  12 * time.Hour,
	}, {
		desc:  "mixed standard units",
		input: "1h30m",
		want:  90 * time.Minute,
	}, {
		desc:  "combination with hours after custom unit",
		input: "1d12h",
		want:  36 * time.Hour,
	}, {
		desc:    "invalid format",
		input:   "abc",
		wantErr: true,
	}, {
		desc:    "trailing number",
		input:   "7d5",
		wantErr: true,
	}} {
		t.Run(tt.desc, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
