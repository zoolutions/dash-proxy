package server

import (
	"log/slog"
	"slices"
	"strings"
)

// Wildcard planning for the dynamic path.
//
// planIssuanceDomains collapses deploy-registered hosts through the domain
// grouper, which infers zones from public-suffix structure and trusts any DNS
// provider to answer for them. Names learned from a tls-domains-source are
// tenant-owned and spread across roots the proxy has no DNS control over, so
// that heuristic can order what it can never validate (#108). The dynamic
// path therefore collapses under exactly one signal: a zone the operator
// EXPLICITLY mapped to a DNS-01 provider (--acme-dns-provider zone=provider).
// The mapping is the operator asserting control over that zone's DNS; the
// default provider and "auto" assert nothing and never collapse a name.

// planDynamicIssuance rewrites a batch of names into the identifier set to
// order: a single-label name directly under a mapped zone whose solver is
// armed becomes the zone's wildcard; the apex and deeper labels — which an
// ACME wildcard does not cover — stay concrete and ride the same order. held
// reports an identifier currently on the quarantine ladder: a held wildcard
// keeps its names concrete, so a zone whose DNS-01 is failing degrades to
// per-name orders (and HTTP-01 fallback) instead of going offline. Output
// keeps first-appearance order.
func (m *SANCertManager) planDynamicIssuance(domains []string, held func(string) bool) []string {
	identifierOf := m.planDynamicIdentifiers(domains, held)

	planned := make([]string, 0, len(domains))
	for _, domain := range domains {
		identifier := identifierOf[domain]
		if !slices.Contains(planned, identifier) {
			planned = append(planned, identifier)
		}
	}
	return planned
}

// planDynamicIdentifiers maps each name to the identifier that will be
// ordered for it: the name itself, or its mapped zone's wildcard.
func (m *SANCertManager) planDynamicIdentifiers(domains []string, held func(string) bool) map[string]string {
	identifierOf := make(map[string]string, len(domains))

	m.mu.RLock()
	prefer := m.grouper != nil && m.grouper.PreferWildcard
	m.mu.RUnlock()

	for _, domain := range domains {
		identifierOf[domain] = domain
		if !prefer {
			continue
		}
		if wildcard, ok := m.mappedZoneWildcard(domain); ok && (held == nil || !held(wildcard)) {
			identifierOf[domain] = wildcard
		}
	}
	return identifierOf
}

// mappedZoneWildcard returns the wildcard covering a name when the name sits
// exactly one label below an explicitly mapped zone and that zone's DNS-01
// solver is armed. The apex, deeper names, names under the default provider
// and wildcards themselves report false.
func (m *SANCertManager) mappedZoneWildcard(domain string) (string, bool) {
	if strings.HasPrefix(domain, "*.") {
		return "", false
	}

	_, zone := m.selection.ProviderFor(domain)
	if zone == "" {
		return "", false
	}

	label, ok := strings.CutSuffix(normalizeDomainName(domain), "."+zone)
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}

	wildcard := "*." + zone
	if !m.hasDNSProviderFor(wildcard) {
		return "", false
	}
	return wildcard, true
}

// adoptWildcardCoverageLocked hands every concrete name a freshly adopted
// wildcard covers over to it, when the name's owner accepts the wildcard's
// directory. The names' previous certificates then serve nothing and renew
// nothing, so the renewer retires them: a zone that was certified one name at
// a time converges on its wildcard instead of renewing the sprawl forever.
// Callers must hold m.mu.
func (m *SANCertManager) adoptWildcardCoverageLocked(managed *ManagedCert) {
	taken := []string{}
	for _, wildcard := range managed.Domains {
		if !strings.HasPrefix(wildcard, "*.") {
			continue
		}
		for domain, certID := range m.domainToCert {
			if certID == managed.Identifier || strings.HasPrefix(domain, "*.") || !matchesWildcard(wildcard, domain) {
				continue
			}
			owner, ok := m.registeredDomains[domain]
			if !ok {
				owner, ok = m.dynamicDomains[domain]
			}
			if !ok || owner == "" || !m.certMatchesServiceDirectoryLocked(managed, owner) {
				continue
			}
			m.domainToCert[domain] = managed.Identifier
			taken = append(taken, domain)
		}
	}

	if len(taken) > 0 {
		slices.Sort(taken)
		slog.Info("Wildcard certificate takes over covered names",
			"identifier", managed.Identifier, "domains", taken)
	}
}
