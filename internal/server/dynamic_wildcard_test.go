package server

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/basecamp/kamal-proxy/internal/server/acme"
)

// wildcardZonedManager is a manager with legacy.example explicitly mapped to
// a DNS-01 provider (armed), vultr as the default provider, and wildcards
// preferred — the shape under which the dynamic path may plan a wildcard.
func wildcardZonedManager(t testing.TB) *SANCertManager {
	t.Helper()

	manager := testZonedManager(t, acme.ProviderVultr, map[string]acme.ProviderName{
		"legacy.example": acme.ProviderHetzner,
	})
	manager.dnsObtainers = map[acme.ProviderName]certObtainer{acme.ProviderHetzner: successfulObtainer(t)}
	manager.dnsObtainer = successfulObtainer(t)
	manager.grouper.DNSProviderAvailable = true
	manager.grouper.PreferWildcard = true
	return manager
}

func neverHeld(string) bool { return false }

// Only a zone the operator explicitly mapped proves DNS control; names under
// the default provider stay concrete, and so do the apex and deeper labels,
// which an ACME wildcard does not cover.
func TestSANCertManager_PlanDynamicIssuance_CollapsesOnlyExplicitlyMappedZones(t *testing.T) {
	manager := wildcardZonedManager(t)

	planned := manager.planDynamicIssuance([]string{
		"a.legacy.example", "b.legacy.example", "legacy.example", "deep.x.legacy.example",
		"tenant.other.net",
	}, neverHeld)

	assert.Equal(t, []string{"*.legacy.example", "legacy.example", "deep.x.legacy.example", "tenant.other.net"}, planned)
}

func TestSANCertManager_PlanDynamicIssuance_KeepsConcreteNames(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *SANCertManager)
		held  func(string) bool
	}{
		{
			name:  "prefer-wildcard off",
			setup: func(m *SANCertManager) { m.grouper.PreferWildcard = false },
			held:  neverHeld,
		},
		{
			name:  "zone solver not armed",
			setup: func(m *SANCertManager) { m.dnsObtainers = nil },
			held:  neverHeld,
		},
		{
			name:  "wildcard identifier is held",
			setup: func(m *SANCertManager) {},
			held:  func(domain string) bool { return domain == "*.legacy.example" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := wildcardZonedManager(t)
			tt.setup(manager)

			planned := manager.planDynamicIssuance([]string{"a.legacy.example", "b.legacy.example"}, tt.held)

			assert.Equal(t, []string{"a.legacy.example", "b.legacy.example"}, planned)
		})
	}
}

// A dynamic batch under a mapped zone is ordered as the zone wildcard (plus
// the apex), and every covered name — including one learned later — is served
// from it without another order.
func TestDomainIssuer_Issue_OrdersMappedZoneWildcardAndServesCoveredNames(t *testing.T) {
	manager := wildcardZonedManager(t)
	obtainer := successfulObtainer(t)
	quarantine := newDomainQuarantine()
	issuer := newDomainIssuer(manager, quarantine, domainIssuerConfig{
		Obtainer:  obtainer,
		BatchSize: func(string) int { return 10 },
	})
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example", "legacy.example"})

	issuer.Request("a.legacy.example", "service1")
	issuer.Request("b.legacy.example", "service1")
	issuer.Request("legacy.example", "service1")
	issuer.issue(issuer.nextBatch())

	require.Len(t, obtainer.Calls(), 1)
	assert.Equal(t, []string{"*.legacy.example", "legacy.example"}, obtainer.Calls()[0].Domains)

	for _, domain := range []string{"a.legacy.example", "b.legacy.example", "legacy.example"} {
		assert.True(t, manager.HasValidCertificate(domain), domain)
	}
	assert.Equal(t, 0, quarantine.Len())

	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example", "legacy.example", "c.legacy.example"})
	issuer.Request("c.legacy.example", "service1")
	assert.Empty(t, issuer.nextBatch(), "a name the wildcard covers is not issuable")
	assert.True(t, manager.HasValidCertificate("c.legacy.example"))
	assert.Len(t, obtainer.Calls(), 1)
}

// Two mapped zones at the same provider share a batch partition but never an
// order: each wildcard anchors its own.
func TestDomainIssuer_Issue_OnePartitionPerWildcardZone(t *testing.T) {
	manager := testZonedManager(t, "", map[string]acme.ProviderName{
		"legacy.example":   acme.ProviderCloudflare,
		"platform.example": acme.ProviderCloudflare,
	})
	manager.dnsObtainers = map[acme.ProviderName]certObtainer{acme.ProviderCloudflare: successfulObtainer(t)}
	manager.grouper.DNSProviderAvailable = true
	manager.grouper.PreferWildcard = true

	obtainer := successfulObtainer(t)
	issuer := newDomainIssuer(manager, newDomainQuarantine(), domainIssuerConfig{
		Obtainer:  obtainer,
		BatchSize: func(string) int { return 10 },
	})
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.platform.example"})

	issuer.Request("a.legacy.example", "service1")
	issuer.Request("b.platform.example", "service1")
	batch := issuer.nextBatch()
	require.Len(t, batch, 2, "same provider, one batch")
	issuer.issue(batch)

	calls := obtainer.Calls()
	require.Len(t, calls, 2)
	ordered := [][]string{calls[0].Domains, calls[1].Domains}
	assert.ElementsMatch(t, [][]string{{"*.legacy.example"}, {"*.platform.example"}}, ordered)
	assert.True(t, manager.HasValidCertificate("a.legacy.example"))
	assert.True(t, manager.HasValidCertificate("b.platform.example"))
	assert.Empty(t, issuer.inflight, "every hold released")
}

// A failed wildcard order must not take the zone offline: the wildcard
// identifier takes the quarantine ladder, the names it covered retry once as
// concrete identifiers (where HTTP-01 fallback can still answer).
func TestDomainIssuer_Issue_WildcardFailureRetriesCoveredNamesConcretely(t *testing.T) {
	manager := wildcardZonedManager(t)
	obtainer := &fakeObtainer{}
	obtainer.respond = func(request certificate.ObtainRequest) (*certificate.Resource, error) {
		if containsWildcard(request.Domains) {
			return nil, errors.New("acme: error presenting token: dns provider refused")
		}
		return testCertResource(t, request.Domains, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)), nil
	}
	quarantine := newDomainQuarantine()
	issuer := newDomainIssuer(manager, quarantine, domainIssuerConfig{
		Obtainer:  obtainer,
		BatchSize: func(string) int { return 10 },
	})
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example"})

	issuer.Request("a.legacy.example", "service1")
	issuer.Request("b.legacy.example", "service1")
	issuer.issue(issuer.nextBatch())

	require.Len(t, obtainer.Calls(), 1)
	assert.Equal(t, []string{"*.legacy.example"}, obtainer.Calls()[0].Domains)
	assert.True(t, quarantine.IsQuarantined("*.legacy.example"), "the wildcard identifier backs off")
	assert.False(t, quarantine.IsQuarantined("a.legacy.example"))
	assert.False(t, quarantine.IsQuarantined("b.legacy.example"))

	retry := issuer.nextBatch()
	require.Len(t, retry, 2, "covered names re-enqueued once")
	for _, request := range retry {
		assert.True(t, request.retried)
	}
	issuer.issue(retry)

	require.Len(t, obtainer.Calls(), 2)
	assert.Equal(t, []string{"a.legacy.example", "b.legacy.example"}, obtainer.Calls()[1].Domains,
		"the retry is concrete: no wildcard while it is held, and never for a retried request")
	assert.True(t, manager.HasValidCertificate("a.legacy.example"))
	assert.True(t, manager.HasValidCertificate("b.legacy.example"))
	assert.Empty(t, issuer.nextBatch())
}

// A concrete culprit in a wildcard order is still quarantined by name; only
// the wildcard's own failure is forgiven for the names it covers.
func TestDomainIssuer_Issue_ConcreteCulpritInWildcardOrderIsQuarantined(t *testing.T) {
	manager := wildcardZonedManager(t)
	obtainer := &fakeObtainer{}
	obtainer.respond = func(request certificate.ObtainRequest) (*certificate.Resource, error) {
		if slices.Contains(request.Domains, "legacy.example") {
			return nil, errors.New("error: one or more domains had a problem:\nlegacy.example: acme: error presenting token")
		}
		return testCertResource(t, request.Domains, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)), nil
	}
	quarantine := newDomainQuarantine()
	issuer := newDomainIssuer(manager, quarantine, domainIssuerConfig{
		Obtainer:  obtainer,
		BatchSize: func(string) int { return 10 },
	})
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "legacy.example"})

	issuer.Request("a.legacy.example", "service1")
	issuer.Request("legacy.example", "service1")
	issuer.issue(issuer.nextBatch())

	require.Len(t, obtainer.Calls(), 1)
	assert.Equal(t, []string{"*.legacy.example", "legacy.example"}, obtainer.Calls()[0].Domains)
	assert.True(t, quarantine.IsQuarantined("legacy.example"))
	assert.False(t, quarantine.IsQuarantined("*.legacy.example"))
	assert.False(t, quarantine.IsQuarantined("a.legacy.example"))

	retry := issuer.nextBatch()
	require.Len(t, retry, 1)
	assert.Equal(t, "a.legacy.example", retry[0].domain)
	issuer.issue(retry)
	assert.True(t, manager.HasValidCertificate("a.legacy.example"))
}

// Let's Encrypt limits duplicate certificates per identifier set: a wildcard
// already in flight is not ordered a second time by a later batch — its
// covered names are released for the next poll, which finds them covered.
func TestDomainIssuer_Issue_DoesNotOrderAnInflightWildcardTwice(t *testing.T) {
	manager := wildcardZonedManager(t)
	// Only the first order blocks; a second one — the bug — returns at once
	// so the test fails instead of deadlocking.
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	orders := 0
	obtainer := &fakeObtainer{}
	obtainer.respond = func(request certificate.ObtainRequest) (*certificate.Resource, error) {
		mu.Lock()
		orders++
		blocking := orders == 1
		mu.Unlock()
		if blocking {
			close(entered)
			<-release
		}
		return testCertResource(t, request.Domains, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour)), nil
	}
	issuer := newDomainIssuer(manager, newDomainQuarantine(), domainIssuerConfig{
		Obtainer:  obtainer,
		BatchSize: func(string) int { return 1 },
	})
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example"})

	issuer.Request("a.legacy.example", "service1")
	issuer.Request("b.legacy.example", "service1")
	first := issuer.nextBatch()
	second := issuer.nextBatch()
	require.Len(t, first, 1)
	require.Len(t, second, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		issuer.issue(first)
	}()
	<-entered

	issuer.issue(second)
	assert.Len(t, obtainer.Calls(), 1, "the second batch must not order the in-flight wildcard again")

	close(release)
	<-done

	assert.True(t, manager.HasValidCertificate("a.legacy.example"))
	assert.True(t, manager.HasValidCertificate("b.legacy.example"))
	assert.Empty(t, issuer.inflight)
}

// Adopting a wildcard takes over the concrete names it covers, so their old
// per-name certificates stop serving and the next reconcile retires them.
func TestSANCertManager_AdoptWildcard_RepointsCoveredNamesAndRetiresTheirCertificates(t *testing.T) {
	manager := wildcardZonedManager(t)
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example", "tenant.other.net"})

	oldA := adoptTestCert(t, manager, []string{"a.legacy.example"}, time.Now().Add(-time.Hour), time.Now().Add(60*24*time.Hour))
	oldB := adoptTestCert(t, manager, []string{"b.legacy.example"}, time.Now().Add(-time.Hour), time.Now().Add(60*24*time.Hour))
	other := adoptTestCert(t, manager, []string{"tenant.other.net"}, time.Now().Add(-time.Hour), time.Now().Add(60*24*time.Hour))

	wildcard := adoptTestCert(t, manager, []string{"*.legacy.example"}, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))

	assert.Equal(t, wildcard.Identifier, manager.certIDForDomain("a.legacy.example"))
	assert.Equal(t, wildcard.Identifier, manager.certIDForDomain("b.legacy.example"))
	assert.Equal(t, other.Identifier, manager.certIDForDomain("tenant.other.net"))

	renewer := newCertRenewer(manager, newDomainQuarantine(), certRenewerConfig{Obtainer: successfulObtainer(t)})
	renewer.reconcile()

	ids := []string{}
	for _, cert := range manager.ManagedCertificates() {
		ids = append(ids, cert.Identifier)
	}
	assert.ElementsMatch(t, []string{wildcard.Identifier, other.Identifier}, ids)
	assert.NotContains(t, ids, oldA.Identifier)
	assert.NotContains(t, ids, oldB.Identifier)
}

// A concrete-name certificate under a mapped zone renews as the wildcard —
// with no ARI replaces marker, since the identifier set changed — and the
// zone's other per-name certificates retire behind it.
func TestCertRenewer_RenewsMappedZoneNamesAsWildcard(t *testing.T) {
	manager := wildcardZonedManager(t)
	manager.SetDynamicDomains("service1", []string{"a.legacy.example", "b.legacy.example"})
	adoptTestCert(t, manager, []string{"a.legacy.example"}, time.Now().Add(-70*24*time.Hour), time.Now().Add(20*24*time.Hour))
	adoptTestCert(t, manager, []string{"b.legacy.example"}, time.Now().Add(-10*24*time.Hour), time.Now().Add(80*24*time.Hour))

	obtainer := successfulObtainer(t)
	renewer := newCertRenewer(manager, newDomainQuarantine(), certRenewerConfig{Obtainer: obtainer})
	renewer.reconcile()

	require.Len(t, obtainer.Calls(), 1)
	assert.Equal(t, []string{"*.legacy.example"}, obtainer.Calls()[0].Domains)
	assert.Empty(t, obtainer.Calls()[0].ReplacesCertID, "a changed identifier set carries no replaces marker")

	// The sibling's certificate was not due; it retires on the pass after the
	// wildcard took its name over. Nothing else is ordered.
	renewer.reconcile()
	require.Len(t, obtainer.Calls(), 1)

	certs := manager.ManagedCertificates()
	require.Len(t, certs, 1, "both per-name certificates retire behind the wildcard")
	assert.Equal(t, []string{"*.legacy.example"}, certs[0].Domains)
	assert.True(t, manager.HasValidCertificate("a.legacy.example"))
	assert.True(t, manager.HasValidCertificate("b.legacy.example"))
}
