package transip

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/transip/gotransip/v6/domain"
	"github.com/transip/gotransip/v6/rest"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/registry/txt"
)

// fakeRepository keeps the DNS entries of its zones in memory and refuses what
// the TransIP API refuses.
type fakeRepository struct {
	zones   map[string][]domain.DNSEntry
	added   []domain.DNSEntry
	removed []domain.DNSEntry
}

func newFakeRepository(zoneName string, entries ...domain.DNSEntry) *fakeRepository {
	return &fakeRepository{zones: map[string][]domain.DNSEntry{zoneName: entries}}
}

func (r *fakeRepository) GetAll() ([]domain.Domain, error) {
	var domains []domain.Domain
	for name := range r.zones {
		domains = append(domains, domain.Domain{Name: name})
	}
	return domains, nil
}

func (r *fakeRepository) GetDNSEntries(domainName string) ([]domain.DNSEntry, error) {
	return slices.Clone(r.zones[domainName]), nil
}

func (r *fakeRepository) AddDNSEntry(domainName string, entry domain.DNSEntry) error {
	if slices.Contains(r.zones[domainName], entry) {
		return &rest.Error{Message: "this exact record already exists.", StatusCode: 406}
	}
	r.zones[domainName] = append(r.zones[domainName], entry)
	r.added = append(r.added, entry)
	return nil
}

func (r *fakeRepository) RemoveDNSEntry(domainName string, entry domain.DNSEntry) error {
	i := slices.Index(r.zones[domainName], entry)
	if i < 0 {
		return &rest.Error{Message: "Dns entry not found", StatusCode: 404}
	}
	r.zones[domainName] = slices.Delete(r.zones[domainName], i, i+1)
	r.removed = append(r.removed, entry)
	return nil
}

// contents returns the contents of the entries with the given name and type
func (r *fakeRepository) contents(zoneName, name, recordType string) []string {
	contents := []string{}
	for _, entry := range r.zones[zoneName] {
		if entry.Name == name && entry.Type == recordType {
			contents = append(contents, entry.Content)
		}
	}
	slices.Sort(contents)
	return contents
}

func entry(name, recordType, content string, ttl int) domain.DNSEntry {
	return domain.DNSEntry{Name: name, Type: recordType, Content: content, Expire: ttl}
}

func assertContents(t *testing.T, repo *fakeRepository, name, recordType string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	slices.Sort(want)
	if got := repo.contents("example.org", name, recordType); !slices.Equal(got, want) {
		t.Errorf("%s %s: expected %q, got %q", name, recordType, want, got)
	}
}

func assertCalls(t *testing.T, repo *fakeRepository, added, removed int) {
	t.Helper()
	if len(repo.added) != added || len(repo.removed) != removed {
		t.Errorf("expected %d added and %d removed entries, got %v and %v", added, removed, repo.added, repo.removed)
	}
}

func TestRecordsMergesEntriesOfOneRecordSet(t *testing.T) {
	repo := newFakeRepository("example.org",
		entry("@", "A", "192.0.2.1", 300),
		entry("www", "CNAME", "example.org.", 3600),
		entry("_acme-challenge", "TXT", "key-1", 60),
		entry("@", "A", "192.0.2.2", 120),
		entry("_acme-challenge", "TXT", "key-2", 60),
		entry("@", "CAA", "0 issue \"letsencrypt.org\"", 3600),
	)
	repo.zones["example.net"] = []domain.DNSEntry{entry("@", "A", "192.0.2.3", 300)}
	p := NewWithRepository(repo, endpoint.NewDomainFilter([]string{"example.org"}), false)

	records, err := p.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("example.org", "A", 120, "192.0.2.1", "192.0.2.2"),
		endpoint.NewEndpointWithTTL("www.example.org", "CNAME", 3600, "example.org"),
		endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 60, "key-1", "key-2"),
	}
	if len(records) != len(want) {
		t.Fatalf("expected %d endpoints, got %v", len(want), records)
	}
	for i := range want {
		got := records[i]
		if got.DNSName != want[i].DNSName || got.RecordType != want[i].RecordType ||
			got.RecordTTL != want[i].RecordTTL || !slices.Equal(got.Targets, want[i].Targets) {
			t.Errorf("expected %v, got %v", want[i], got)
		}
	}
}

func TestZonesOfASubdomainFilter(t *testing.T) {
	repo := newFakeRepository("example.org", entry("www", "A", "192.0.2.1", 300))
	repo.zones["example.net"] = nil
	p := NewWithRepository(repo, endpoint.NewDomainFilter([]string{"sub.example.org"}), false)

	zones, err := p.zones()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(zones, []string{"example.org"}) {
		t.Errorf("expected only example.org, got %v", zones)
	}
}

func TestCreateOfAnExistingEntryIsNotAnError(t *testing.T) {
	repo := newFakeRepository("example.org",
		entry("_acme-challenge", "TXT", "key-1", 60),
		entry("www", "CNAME", "example.net.", 60),
	)
	p := NewWithRepository(repo, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 60, "key-1"),
		endpoint.NewEndpoint("www.example.org", "CNAME", "example.net"),
		endpoint.NewEndpoint("example.org", "A", "192.0.2.1"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, repo, 1, 0)
	assertContents(t, repo, "_acme-challenge", "TXT", "key-1")
	assertContents(t, repo, "www", "CNAME", "example.net.")
	assertContents(t, repo, "@", "A", "192.0.2.1")
}

// staleRepository does not show the entries that were added
type staleRepository struct{ *fakeRepository }

func (r staleRepository) GetDNSEntries(string) ([]domain.DNSEntry, error) { return nil, nil }

func TestCreateToleratesAnEntryAddedMeanwhile(t *testing.T) {
	repo := newFakeRepository("example.org", entry("_acme-challenge", "TXT", "key-1", 60))
	p := NewWithRepository(staleRepository{repo}, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 60, "key-1"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertContents(t, repo, "_acme-challenge", "TXT", "key-1")
}

func TestUpdateChangesOnlyTheEntriesThatDiffer(t *testing.T) {
	repo := newFakeRepository("example.org",
		entry("@", "A", "192.0.2.1", 300),
		entry("@", "A", "192.0.2.2", 300),
		entry("@", "TXT", "v=spf1 -all", 300),
		entry("www", "A", "192.0.2.1", 300),
	)
	p := NewWithRepository(repo, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{UpdateNew: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("example.org", "A", 300, "192.0.2.2", "192.0.2.3"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, repo, 1, 1)
	assertContents(t, repo, "@", "A", "192.0.2.2", "192.0.2.3")
	assertContents(t, repo, "@", "TXT", "v=spf1 -all")
	assertContents(t, repo, "www", "A", "192.0.2.1")
	// the record set keeps answering: the new entry is there before the old one goes
	if repo.added[0].Content != "192.0.2.3" || repo.removed[0].Content != "192.0.2.1" {
		t.Errorf("unexpected changes: added %v, removed %v", repo.added, repo.removed)
	}
}

func TestUpdateOfTTLReplacesTheEntry(t *testing.T) {
	repo := newFakeRepository("example.org", entry("www", "A", "192.0.2.1", 300))
	p := NewWithRepository(repo, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{UpdateNew: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.org", "A", 3600, "192.0.2.1"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, repo, 1, 1)
	if got := repo.zones["example.org"]; !slices.Equal(got, []domain.DNSEntry{entry("www", "A", "192.0.2.1", 3600)}) {
		t.Errorf("unexpected entries %v", got)
	}
}

func TestUpdateOfCNAMERemovesTheOldTargetFirst(t *testing.T) {
	repo := newFakeRepository("example.org", entry("www", "CNAME", "old.example.net.", 300))
	p := NewWithRepository(repo, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{UpdateNew: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.org", "CNAME", 300, "new.example.net"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, repo, 1, 1)
	assertContents(t, repo, "www", "CNAME", "new.example.net.")
}

func TestDeleteRemovesOnlyTheTargetsOfTheEndpoint(t *testing.T) {
	repo := newFakeRepository("example.org",
		entry("_acme-challenge", "TXT", "key-1", 60),
		entry("_acme-challenge", "TXT", "key-2", 60),
		entry("www", "CNAME", "example.net.", 300),
	)
	p := NewWithRepository(repo, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{Delete: []*endpoint.Endpoint{
		endpoint.NewEndpoint("_acme-challenge.example.org", "TXT", "key-1"),
		endpoint.NewEndpoint("_acme-challenge.example.org", "TXT", "key-gone"),
		endpoint.NewEndpoint("www.example.org", "CNAME", "example.net"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, repo, 0, 2)
	assertContents(t, repo, "_acme-challenge", "TXT", "key-2")
	assertContents(t, repo, "www", "CNAME")
}

func TestDryRunChangesNothing(t *testing.T) {
	repo := newFakeRepository("example.org", entry("www", "A", "192.0.2.1", 300))
	p := NewWithRepository(repo, nil, true)

	err := p.ApplyChanges(context.Background(), &plan.Changes{
		Create:    []*endpoint.Endpoint{endpoint.NewEndpoint("new.example.org", "A", "192.0.2.2")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.org", "A", "192.0.2.3")},
		Delete:    []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.org", "A", "192.0.2.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, repo, 0, 0)
}

// failingRepository fails every change
type failingRepository struct{ *fakeRepository }

func (r failingRepository) AddDNSEntry(string, domain.DNSEntry) error {
	return &rest.Error{Message: "internal server error", StatusCode: 500}
}

func TestApplyChangesReturnsAPIErrors(t *testing.T) {
	p := NewWithRepository(failingRepository{newFakeRepository("example.org")}, nil, false)

	err := p.ApplyChanges(context.Background(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.org", "A", "192.0.2.1"),
	}})

	var restErr *rest.Error
	if !errors.As(err, &restErr) || restErr.StatusCode != 500 {
		t.Errorf("expected the API error, got %v", err)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	p := NewWithRepository(newFakeRepository("example.org"), nil, false)

	adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 30, "key-1").WithSetIdentifier("example.org-1234"),
		endpoint.NewEndpoint("www.example.org", "A", "192.0.2.1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if adjusted[0].SetIdentifier != "" || adjusted[0].RecordTTL != defaultTTL {
		t.Errorf("expected no set identifier and the minimal TTL, got %v", adjusted[0])
	}
	if adjusted[1].RecordTTL.IsConfigured() {
		t.Errorf("expected the TTL to stay unconfigured, got %v", adjusted[1])
	}
}

// syncOnce does what one sync of the ExternalDNS controller does: plan the
// changes from the desired endpoints to the records of the registry, and apply
// them.
func syncOnce(t *testing.T, p *Provider, policy plan.Policy, desired ...*endpoint.Endpoint) *plan.Changes {
	t.Helper()
	managed := []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME, endpoint.RecordTypeTXT}

	registry, err := txt.New(&externaldns.Config{TXTOwnerID: "default", ManagedDNSRecordTypes: managed}, p)
	if err != nil {
		t.Fatal(err)
	}

	records, err := registry.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var endpoints []*endpoint.Endpoint
	for _, ep := range desired {
		endpoints = append(endpoints, ep.DeepCopy())
	}
	endpoints, err = registry.AdjustEndpoints(endpoints)
	if err != nil {
		t.Fatal(err)
	}

	changes := (&plan.Plan{
		Policies:       []plan.Policy{policy},
		Current:        records,
		Desired:        endpoints,
		DomainFilter:   endpoint.MatchAllDomainFilters{registry.GetDomainFilter()},
		ManagedRecords: managed,
		OwnerID:        registry.OwnerID(),
	}).Calculate().Changes

	if changes.HasChanges() {
		if err := registry.ApplyChanges(context.Background(), changes); err != nil {
			t.Fatalf("could not apply %v: %v", changes, err)
		}
	}

	return changes
}

func heritage(resource string) string {
	return `"heritage=external-dns,external-dns/owner=default,external-dns/resource=` + resource + `"`
}

// An ACME DNS-01 challenge through the DNSEndpoint source, as the cert-manager
// webhook presents it: a TXT endpoint with a set identifier. The in-tree
// provider created the record again on every sync, and the API refused it.
func TestChallengeRecordIsCreatedOnceAndRemoved(t *testing.T) {
	repo := newFakeRepository("example.org",
		entry("@", "A", "192.0.2.1", 300),
		entry("a-@", "TXT", heritage("service/default/web"), 300),
		entry("@", "TXT", "v=spf1 -all", 300),
		// what earlier challenges left behind under the upsert-only policy
		entry("_acme-challenge", "TXT", "stale-1", 60),
		entry("_acme-challenge", "TXT", "stale-2", 60),
		entry("txt-_acme-challenge", "TXT", heritage("crd/default/example.org-1"), 60),
		entry("txt-_acme-challenge", "TXT", heritage("crd/default/example.org-2"), 60),
	)
	p := NewWithRepository(repo, endpoint.NewDomainFilter([]string{"example.org"}), false)

	web := endpoint.NewEndpoint("example.org", "A", "192.0.2.1").
		WithLabel(endpoint.ResourceLabelKey, "service/default/web")
	challenge := endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 60, "key-3").
		WithSetIdentifier("example.org-3").
		WithLabel(endpoint.ResourceLabelKey, "crd/default/example.org-3")

	syncOnce(t, p, &plan.SyncPolicy{}, web, challenge)
	assertContents(t, repo, "_acme-challenge", "TXT", "key-3")
	assertContents(t, repo, "txt-_acme-challenge", "TXT", heritage("crd/default/example.org-3"))

	if changes := syncOnce(t, p, &plan.SyncPolicy{}, web, challenge); changes.HasChanges() {
		t.Errorf("expected no changes on the second sync, got %v", changes)
	}

	syncOnce(t, p, &plan.SyncPolicy{}, web)
	assertContents(t, repo, "_acme-challenge", "TXT")
	assertContents(t, repo, "txt-_acme-challenge", "TXT")

	// records of other sources and records ExternalDNS does not own stay
	assertContents(t, repo, "@", "A", "192.0.2.1")
	assertContents(t, repo, "@", "TXT", "v=spf1 -all")
	assertContents(t, repo, "a-@", "TXT", heritage("service/default/web"))
}

// A domain and its wildcard are validated at the same time on one record.
func TestChallengeRecordWithTwoKeys(t *testing.T) {
	repo := newFakeRepository("example.org")
	p := NewWithRepository(repo, nil, false)

	challenge := func(keys ...string) *endpoint.Endpoint {
		return endpoint.NewEndpointWithTTL("_acme-challenge.example.org", "TXT", 60, keys...).
			WithLabel(endpoint.ResourceLabelKey, "crd/default/example.org-1")
	}

	syncOnce(t, p, &plan.SyncPolicy{}, challenge("key-1"))
	syncOnce(t, p, &plan.SyncPolicy{}, challenge("key-1", "key-2"))
	assertContents(t, repo, "_acme-challenge", "TXT", "key-1", "key-2")

	if changes := syncOnce(t, p, &plan.SyncPolicy{}, challenge("key-1", "key-2")); changes.HasChanges() {
		t.Errorf("expected no changes, got %v", changes)
	}

	syncOnce(t, p, &plan.SyncPolicy{}, challenge("key-2"))
	assertContents(t, repo, "_acme-challenge", "TXT", "key-2")

	syncOnce(t, p, &plan.SyncPolicy{})
	assertContents(t, repo, "_acme-challenge", "TXT")
	assertContents(t, repo, "txt-_acme-challenge", "TXT")
}
