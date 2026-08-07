package main

import (
	"reflect"
	"testing"
)

func memberOn(ip, state string, canary bool) dockerContainer {
	c := dockerContainer{State: state, Labels: map[string]string{}}
	if canary {
		c.Labels[labelCanary] = "true"
	}
	c.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"edge": {IPAddress: ip}}
	return c
}

// Several services share one host, so the UI attributes access-log entries by
// upstream URL. These must match cmd/proxy's format exactly — a mismatch shows
// up as a card claiming no traffic at all.
func TestServiceBackendsMatchProxyFormat(t *testing.T) {
	s := &Service{Port: 3000, Members: []dockerContainer{memberOn("172.26.0.15", "running", false)}}
	if got, want := serviceBackends(s), []string{"http://172.26.0.15:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}
}

// Stopped replicas hold no IP and serve nothing, so they must contribute
// nothing — otherwise a stopped service would claim its neighbours' requests.
func TestServiceBackendsSkipsStopped(t *testing.T) {
	s := &Service{Port: 3000, Members: []dockerContainer{
		memberOn("172.26.0.15", "exited", false),
		memberOn("172.26.0.16", "running", false),
	}}
	if got, want := serviceBackends(s), []string{"http://172.26.0.16:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}

	allStopped := &Service{Port: 3000, Members: []dockerContainer{memberOn("172.26.0.15", "exited", false)}}
	if got := serviceBackends(allStopped); len(got) != 0 {
		t.Fatalf("backends = %v, want none for a fully stopped service", got)
	}
}

// A staged canary serves live traffic alongside the primary, so its requests
// belong to this service too.
func TestServiceBackendsIncludesCanary(t *testing.T) {
	s := &Service{Port: 3000, Members: []dockerContainer{
		memberOn("172.26.0.15", "running", false),
		memberOn("172.26.0.99", "running", true),
	}}
	got := serviceBackends(s)
	want := []string{"http://172.26.0.15:3000", "http://172.26.0.99:3000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v (sorted, canary included)", got, want)
	}
}

// A service with no port label cannot be routed, so it has no upstreams to
// claim — returning something would attribute traffic on a guess.
func TestServiceBackendsNoPort(t *testing.T) {
	s := &Service{Members: []dockerContainer{memberOn("172.26.0.15", "running", false)}}
	if got := serviceBackends(s); len(got) != 0 {
		t.Fatalf("backends = %v, want none without a port", got)
	}
}

// Duplicates would make the UI's membership test do redundant work and read as
// more replicas than exist.
func TestServiceBackendsDeduped(t *testing.T) {
	s := &Service{Port: 3000, Members: []dockerContainer{
		memberOn("172.26.0.15", "running", false),
		memberOn("172.26.0.15", "running", false),
	}}
	if got, want := serviceBackends(s), []string{"http://172.26.0.15:3000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}
}
