package transip

import (
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// TransIP takes a host name without a trailing dot as relative to the zone.
// The in-tree provider added the dot to CNAME targets only, so an NS
// delegation to external name servers was refused by the API, and that error
// failed the whole apply.
func TestContentForTarget(t *testing.T) {
	tests := []struct{ recordType, target, want string }{
		{"A", "192.0.2.1", "192.0.2.1"},
		{"AAAA", "2001:db8::1", "2001:db8::1"},
		{"TXT", `"v=spf1 -all"`, `"v=spf1 -all"`},
		{"CNAME", "foo.example.com", "foo.example.com."},
		{"CNAME", "foo.example.com.", "foo.example.com."},
		{"NS", "ns0.example.com", "ns0.example.com."},
		{"NS", "ns1.example.com.", "ns1.example.com."},
		{"MX", "10 mail.example.com", "10 mail.example.com."},
		{"MX", "10 mail.example.com.", "10 mail.example.com."},
		{"MX", "0 .", "0 ."},
		{"SRV", "10 20 5060 sip.example.com", "10 20 5060 sip.example.com."},
		{"SRV", "10 20 5060 sip.example.com.", "10 20 5060 sip.example.com."},
	}
	for _, tt := range tests {
		if got := contentForTarget(tt.recordType, tt.target); got != tt.want {
			t.Errorf("%s %q: expected %q, got %q", tt.recordType, tt.target, tt.want, got)
		}
	}
}

// A delegation to external name servers is written fully qualified and read
// back as ExternalDNS sent it, so the second sync changes nothing.
func TestNSDelegationIsFullyQualified(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	sub := endpoint.NewEndpointWithTTL("sub.example.org", "NS", 3600, "ns0.example.com", "ns1.example.com.").
		WithLabel(endpoint.ResourceLabelKey, "crd/default/sub")

	syncOnce(t, p, &plan.SyncPolicy{}, sub)
	assertContents(t, repo, "sub", "NS", "ns0.example.com.", "ns1.example.com.")

	if changes := syncOnce(t, p, &plan.SyncPolicy{}, sub); changes.HasChanges() {
		t.Errorf("expected no changes on the second sync, got %v", changes)
	}
	// two NS entries and the ownership record
	assertCalls(t, repo, 3, 0)
}

// A mail exchanger is read back, so the second sync changes nothing. With the
// default type list of ExternalDNS, which has no MX, it was created again on
// every sync.
func TestMXRecordIsReadBack(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	mail := endpoint.NewEndpointWithTTL("lists.example.org", "MX", 3600, "10 mail.example.com", "20 backup.example.com").
		WithLabel(endpoint.ResourceLabelKey, "crd/default/mail")

	syncOnce(t, p, &plan.SyncPolicy{}, mail)
	assertContents(t, repo, "lists", "MX", "10 mail.example.com.", "20 backup.example.com.")

	if changes := syncOnce(t, p, &plan.SyncPolicy{}, mail); changes.HasChanges() {
		t.Errorf("expected no changes on the second sync, got %v", changes)
	}
	// two MX entries and the ownership record
	assertCalls(t, repo, 3, 0)
}

// An SRV record is written with a fully qualified host and read back.
func TestSRVRecordIsReadBack(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	sip := endpoint.NewEndpointWithTTL("_sip._tcp.example.org", "SRV", 3600, "10 20 5060 sip.example.com").
		WithLabel(endpoint.ResourceLabelKey, "crd/default/sip")

	syncOnce(t, p, &plan.SyncPolicy{}, sip)
	assertContents(t, repo, "_sip._tcp", "SRV", "10 20 5060 sip.example.com.")

	if changes := syncOnce(t, p, &plan.SyncPolicy{}, sip); changes.HasChanges() {
		t.Errorf("expected no changes on the second sync, got %v", changes)
	}
	assertCalls(t, repo, 2, 0)
}
