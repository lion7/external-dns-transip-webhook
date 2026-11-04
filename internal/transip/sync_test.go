package transip

import (
	"testing"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// The account has a budget of 1000 TransIP API calls an hour, so a sync lists
// the domains once: ApplyChanges uses the zones of the Records call before it.
func TestASyncListsTheDomainsOnce(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	for i, target := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		web := endpoint.NewEndpoint("www.example.org", "A", target).
			WithLabel(endpoint.ResourceLabelKey, "service/default/web")
		syncOnce(t, p, &plan.SyncPolicy{}, web)
		if repo.listed != i+1 {
			t.Fatalf("sync %d: expected the domains to be listed %d times, got %d", i+1, i+1, repo.listed)
		}
	}
}

// Every sync lists the domains anew, so a domain added to the account is seen
// on the next sync.
func TestADomainAddedToTheAccountIsSeenOnTheNextSync(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)
	syncOnce(t, p, &plan.SyncPolicy{})

	repo.zones["example.net"] = nil
	web := endpoint.NewEndpoint("www.example.net", "A", "192.0.2.1").
		WithLabel(endpoint.ResourceLabelKey, "service/default/web")
	syncOnce(t, p, &plan.SyncPolicy{}, web)

	if len(repo.zones["example.net"]) == 0 {
		t.Fatal("expected the record to be created in the new domain")
	}
}

// An ApplyChanges without a Records call before it lists the domains itself.
func TestApplyChangesWithoutRecordsListsTheDomains(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	web := endpoint.NewEndpoint("www.example.org", "A", "192.0.2.1")
	if err := p.ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{web}}); err != nil {
		t.Fatal(err)
	}
	if repo.listed != 1 {
		t.Errorf("expected the domains to be listed once, got %d", repo.listed)
	}
}
