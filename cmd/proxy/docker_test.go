package main

import "testing"

func TestDockerUnhealthy(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"", false},
		{"Up 2 minutes", false},
		{"Up 2 minutes (healthy)", false},
		{"Up 2 minutes (unhealthy)", true},
		{"Up 5 seconds (health: starting)", false},
	}
	for _, tc := range cases {
		if got := dockerUnhealthy(tc.status); got != tc.want {
			t.Errorf("dockerUnhealthy(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
