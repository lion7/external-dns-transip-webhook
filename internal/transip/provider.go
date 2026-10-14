/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package transip is the TransIP provider that ExternalDNS shipped in-tree up
// to v0.21.0, with these changes:
//
//   - all entries with the same name and type are one endpoint, as they are one
//     record set to the planner (it used to see only the last entry)
//   - creating an entry that already exists is not an error
//   - an update adds and removes only the entries that differ, and a delete
//     removes only the endpoint's own targets
//   - set identifiers, which TransIP cannot store, are dropped from the desired
//     endpoints so they match the records read back
//   - every API call has a timeout, so an unreachable API is an error and not a
//     call that hangs until ExternalDNS gives up and exits
package transip

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/transip/gotransip/v6"
	"github.com/transip/gotransip/v6/domain"
	"github.com/transip/gotransip/v6/rest"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	// 60 seconds is the current minimal TTL for TransIP and will replace unconfigured
	// and lower TTL's for Endpoints
	defaultTTL = 60

	// apexName is the name TransIP uses for the entries of the domain itself
	apexName = "@"
)

// DNSRepository is the part of the TransIP domain repository the provider uses.
type DNSRepository interface {
	GetAll() ([]domain.Domain, error)
	GetDNSEntries(domainName string) ([]domain.DNSEntry, error)
	AddDNSEntry(domainName string, dnsEntry domain.DNSEntry) error
	RemoveDNSEntry(domainName string, dnsEntry domain.DNSEntry) error
}

// Provider is an implementation of Provider for TransIP.
type Provider struct {
	provider.BaseProvider
	repo         DNSRepository
	domainFilter *endpoint.DomainFilter
	dryRun       bool

	// ExternalDNS calls the provider from one goroutine, but nothing in the
	// webhook protocol guarantees that
	mu sync.Mutex
}

// New creates a TransIP provider that authenticates with the private key of
// the given account. Every TransIP API call, the token request included, gives
// up after timeout.
func New(accountName, privateKeyFile string, timeout time.Duration, domainFilter *endpoint.DomainFilter, dryRun bool) (*Provider, error) {
	if accountName == "" {
		return nil, errors.New("TransIP account name not set")
	}

	if privateKeyFile == "" {
		return nil, errors.New("TransIP private key file not set")
	}

	if timeout <= 0 {
		return nil, errors.New("TransIP timeout must be positive")
	}

	apiMode := gotransip.APIModeReadWrite
	if dryRun {
		apiMode = gotransip.APIModeReadOnly
	}

	return newWithConfiguration(gotransip.ClientConfiguration{
		AccountName:    accountName,
		PrivateKeyPath: privateKeyFile,
		Mode:           apiMode,
		// gotransip falls back on http.DefaultClient, which never gives up. A
		// call that hangs then outlasts the HTTP client timeout of ExternalDNS,
		// which exits on it; an error here is a 500, which it retries.
		HTTPClient: &http.Client{Timeout: timeout},
	}, domainFilter, dryRun)
}

func newWithConfiguration(configuration gotransip.ClientConfiguration, domainFilter *endpoint.DomainFilter, dryRun bool) (*Provider, error) {
	client, err := gotransip.NewClient(configuration)
	if err != nil {
		return nil, fmt.Errorf("could not setup TransIP API client: %w", err)
	}

	return NewWithRepository(&domain.Repository{Client: client}, domainFilter, dryRun), nil
}

// NewWithRepository creates a TransIP provider on top of the given repository.
func NewWithRepository(repo DNSRepository, domainFilter *endpoint.DomainFilter, dryRun bool) *Provider {
	if domainFilter == nil {
		domainFilter = endpoint.NewDomainFilter(nil)
	}

	return &Provider{
		repo:         repo,
		domainFilter: domainFilter,
		dryRun:       dryRun,
	}
}

// GetDomainFilter returns the domains this provider manages.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// AdjustEndpoints makes the desired endpoints comparable with the endpoints
// that Records returns.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	for _, ep := range endpoints {
		// TransIP cannot store a set identifier, so an endpoint that keeps one
		// never matches its own record and is created again on every sync
		ep.SetIdentifier = ""

		// TransIP stores no TTL below the minimum, a lower one would be an
		// update on every sync
		if ep.RecordTTL.IsConfigured() && ep.RecordTTL < defaultTTL {
			ep.RecordTTL = defaultTTL
		}
	}

	return endpoints, nil
}

// Records returns the list of records in all zones
func (p *Provider) Records(_ context.Context) ([]*endpoint.Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	zones, err := p.zones()
	if err != nil {
		return nil, err
	}

	var endpoints []*endpoint.Endpoint
	// go over all zones and their DNS entries and create endpoints for them
	for _, zoneName := range zones {
		entries, err := p.repo.GetDNSEntries(zoneName)
		if err != nil {
			return nil, fmt.Errorf("could not get DNS entries of %s: %w", zoneName, err)
		}

		// all entries with the same name and type are one endpoint
		type recordSet struct{ name, recordType string }
		var order []recordSet
		sets := map[recordSet][]domain.DNSEntry{}
		for _, r := range entries {
			if !provider.SupportedRecordType(r.Type) {
				continue
			}

			key := recordSet{r.Name, r.Type}
			if _, ok := sets[key]; !ok {
				order = append(order, key)
			}
			sets[key] = append(sets[key], r)
		}

		for _, key := range order {
			ttl := sets[key][0].Expire
			targets := make([]string, 0, len(sets[key]))
			for _, r := range sets[key] {
				ttl = min(ttl, r.Expire)
				targets = append(targets, r.Content)
			}

			name := endpointNameForRecord(key.name, zoneName)
			if ep := endpoint.NewEndpointWithTTL(name, key.recordType, endpoint.TTL(ttl), targets...); ep != nil {
				endpoints = append(endpoints, ep)
			}
		}
	}

	return endpoints, nil
}

// ApplyChanges applies a given set of changes. Every step leaves the zone in a
// state from which the same changes can be applied again.
func (p *Provider) ApplyChanges(_ context.Context, changes *plan.Changes) error {
	if changes == nil || !changes.HasChanges() {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	zones, err := p.zones()
	if err != nil {
		return err
	}

	// the DNS entries of every zone that gets updated, fetched once
	c := &changeSet{provider: p, zones: zones, entries: map[string][]domain.DNSEntry{}}

	// first remove obsolete DNS records
	for _, ep := range changes.Delete {
		if err := c.apply(ep, c.remove); err != nil {
			return err
		}
	}

	// then create new DNS records
	for _, ep := range changes.Create {
		if err := c.apply(ep, c.create); err != nil {
			return err
		}
	}

	// then update existing DNS records
	for _, ep := range changes.UpdateNew {
		if err := c.apply(ep, c.update); err != nil {
			return err
		}
	}

	return nil
}

// zones returns the names of the zones this provider manages
func (p *Provider) zones() ([]string, error) {
	domains, err := p.repo.GetAll()
	if err != nil {
		return nil, fmt.Errorf("could not get domains: %w", err)
	}

	var zones []string
	for _, d := range domains {
		// TransIP API doesn't expose a unique identifier for zones, other than
		// the domain name itself
		if p.domainFilter.Match(d.Name) || p.domainFilter.MatchParent(d.Name) {
			zones = append(zones, d.Name)
		}
	}

	return zones, nil
}

// changeSet applies the changes of one ApplyChanges call
type changeSet struct {
	provider *Provider
	zones    []string
	entries  map[string][]domain.DNSEntry
}

// apply runs the change on the endpoint's zone with the DNS entries the
// endpoint stands for
func (c *changeSet) apply(ep *endpoint.Endpoint, change func(epLog *log.Entry, zoneName string, entries []domain.DNSEntry) error) error {
	epLog := log.WithFields(log.Fields{
		"record": ep.DNSName,
		"type":   ep.RecordType,
	})

	zoneName := zoneNameForDNSName(ep.DNSName, c.zones)
	if zoneName == "" {
		epLog.Warn("could not find zone for endpoint")
		return nil
	}

	if _, ok := c.entries[zoneName]; !ok {
		entries, err := c.provider.repo.GetDNSEntries(zoneName)
		if err != nil {
			return fmt.Errorf("could not get DNS entries of %s: %w", zoneName, err)
		}
		c.entries[zoneName] = entries
	}

	return change(epLog.WithField("zone", zoneName), zoneName, dnsEntriesForEndpoint(ep, zoneName))
}

// remove removes the given entries, whatever their TTL
func (c *changeSet) remove(epLog *log.Entry, zoneName string, entries []domain.DNSEntry) error {
	epLog.Info("endpoint has to go")

	for _, entry := range entries {
		for _, existing := range c.matching(zoneName, entry) {
			if err := c.removeEntry(zoneName, existing); err != nil {
				epLog.WithError(err).Error("could not remove DNS entry")
				return err
			}
		}
	}

	return nil
}

// create adds the given entries next to the ones that exist
func (c *changeSet) create(epLog *log.Entry, zoneName string, entries []domain.DNSEntry) error {
	epLog.Info("endpoint should be created")

	for _, entry := range entries {
		if err := c.ensureEntry(zoneName, entry); err != nil {
			epLog.WithError(err).Error("could not add DNS entry")
			return err
		}
	}

	return nil
}

// update makes the given entries the only ones of their name and type
func (c *changeSet) update(epLog *log.Entry, zoneName string, entries []domain.DNSEntry) error {
	if len(entries) == 0 {
		return nil
	}
	epLog.Info("endpoint needs updating")

	var obsolete []domain.DNSEntry
	for _, existing := range c.entries[zoneName] {
		if existing.Name != entries[0].Name || existing.Type != entries[0].Type {
			continue
		}
		if !slices.ContainsFunc(entries, func(entry domain.DNSEntry) bool { return sameContent(entry, existing) }) {
			obsolete = append(obsolete, existing)
		}
	}

	removeObsolete := func() error {
		for _, existing := range obsolete {
			if err := c.removeEntry(zoneName, existing); err != nil {
				epLog.WithError(err).Error("could not remove DNS entry")
				return err
			}
		}
		return nil
	}

	// a CNAME cannot exist next to another one, any other record set keeps
	// answering while it changes when the new entries are added first
	if entries[0].Type == endpoint.RecordTypeCNAME {
		if err := removeObsolete(); err != nil {
			return err
		}
		obsolete = nil
	}

	for _, entry := range entries {
		if err := c.ensureEntry(zoneName, entry); err != nil {
			epLog.WithError(err).Error("could not add DNS entry")
			return err
		}
	}

	return removeObsolete()
}

// matching returns the existing entries with the name, type and content of the
// given entry
func (c *changeSet) matching(zoneName string, entry domain.DNSEntry) []domain.DNSEntry {
	var matches []domain.DNSEntry
	for _, existing := range c.entries[zoneName] {
		if sameContent(existing, entry) {
			matches = append(matches, existing)
		}
	}
	return matches
}

// ensureEntry adds the entry unless it exists, replacing the ones that differ
// in TTL only
func (c *changeSet) ensureEntry(zoneName string, entry domain.DNSEntry) error {
	exists := false
	for _, existing := range c.matching(zoneName, entry) {
		if existing.Expire == entry.Expire {
			exists = true
			continue
		}

		// TransIP API client does have an UpdateDNSEntry call but that does only
		// allow you to update the content of a DNSEntry, not the TTL
		// to work around this, remove the old entry first and add the new entry
		if err := c.removeEntry(zoneName, existing); err != nil {
			return err
		}
	}

	if exists {
		entryLog(zoneName, entry).Debug("DNS entry already exists")
		return nil
	}

	return c.addEntry(zoneName, entry)
}

func (c *changeSet) addEntry(zoneName string, entry domain.DNSEntry) error {
	if c.provider.dryRun {
		entryLog(zoneName, entry).Info("not adding DNS entry in dry-run mode")
		return nil
	}
	entryLog(zoneName, entry).Info("adding DNS entry")

	// the entry may have been added since the entries were fetched
	if err := c.provider.repo.AddDNSEntry(zoneName, entry); err != nil && !isAlreadyExists(err) {
		return err
	}

	c.entries[zoneName] = append(c.entries[zoneName], entry)
	return nil
}

func (c *changeSet) removeEntry(zoneName string, entry domain.DNSEntry) error {
	if c.provider.dryRun {
		entryLog(zoneName, entry).Info("not removing DNS entry in dry-run mode")
		return nil
	}
	entryLog(zoneName, entry).Info("removing DNS entry")

	// the entry may have been removed since the entries were fetched
	if err := c.provider.repo.RemoveDNSEntry(zoneName, entry); err != nil && !isNotFound(err) {
		return err
	}

	c.entries[zoneName] = slices.DeleteFunc(c.entries[zoneName], func(existing domain.DNSEntry) bool {
		return existing == entry
	})
	return nil
}

func entryLog(zoneName string, entry domain.DNSEntry) *log.Entry {
	return log.WithFields(log.Fields{
		"domain":  zoneName,
		"name":    entry.Name,
		"type":    entry.Type,
		"content": entry.Content,
		"ttl":     entry.Expire,
	})
}

// isAlreadyExists reports whether the API refused an entry because the exact
// same entry is there
func isAlreadyExists(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// isNotFound reports whether the API could not find the entry
func isNotFound(err error) bool {
	var restErr *rest.Error
	return errors.As(err, &restErr) && restErr.StatusCode == http.StatusNotFound
}

// sameContent reports whether two entries have the same name, type and
// content. TransIP keeps the text of a TXT entry as it is, and writes a host
// name with or without a trailing dot.
func sameContent(a, b domain.DNSEntry) bool {
	if a.Name != b.Name || a.Type != b.Type {
		return false
	}
	if a.Type == endpoint.RecordTypeTXT {
		return a.Content == b.Content
	}
	return strings.EqualFold(strings.TrimSuffix(a.Content, "."), strings.TrimSuffix(b.Content, "."))
}

// endpointNameForRecord returns "www.example.org" for the DNSEntry name "www"
// and the Domain with Name "example.org"
func endpointNameForRecord(name, zoneName string) string {
	// root name is identified by "@" and should be translated to domain name for
	// the endpoint entry.
	if name == apexName {
		return zoneName
	}

	return fmt.Sprintf("%s.%s", name, zoneName)
}

// recordNameForEndpoint returns "www" for Endpoint with DNSName "www.example.org"
// and Domain with Name "example.org"
func recordNameForEndpoint(ep *endpoint.Endpoint, zoneName string) string {
	// root name is identified by "@" and should be translated to domain name for
	// the endpoint entry.
	name := strings.TrimSuffix(ep.DNSName, ".")
	if strings.EqualFold(name, zoneName) {
		return apexName
	}

	return name[:len(name)-len(zoneName)-1]
}

// getMinimalValidTTL returns max between given Endpoint's RecordTTL and
// defaultTTL
func getMinimalValidTTL(ep *endpoint.Endpoint) int {
	// TTL cannot be lower than defaultTTL
	if ep.RecordTTL < defaultTTL {
		return defaultTTL
	}

	return int(ep.RecordTTL)
}

// dnsEntriesForEndpoint creates DNS entries for given endpoint and returns
// resulting DNS entry set
func dnsEntriesForEndpoint(ep *endpoint.Endpoint, zoneName string) []domain.DNSEntry {
	ttl := getMinimalValidTTL(ep)

	entries := []domain.DNSEntry{}
	for _, target := range ep.Targets {
		// external hostnames require a trailing dot in TransIP API
		if ep.RecordType == endpoint.RecordTypeCNAME {
			target = provider.EnsureTrailingDot(target)
		}

		entries = append(entries, domain.DNSEntry{
			Name:    recordNameForEndpoint(ep, zoneName),
			Expire:  ttl,
			Type:    ep.RecordType,
			Content: target,
		})
	}

	return entries
}

// zoneNameForDNSName returns the most specific zone the given name is in, or
// an empty string if it is in none
func zoneNameForDNSName(name string, zones []string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))

	match := ""
	for _, zoneName := range zones {
		zone := strings.ToLower(zoneName)
		if (name == zone || strings.HasSuffix(name, "."+zone)) && len(zoneName) > len(match) {
			match = zoneName
		}
	}

	return match
}
