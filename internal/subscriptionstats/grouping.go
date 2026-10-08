package subscriptionstats

import (
	"net/url"
	"strings"

	"github.com/p4u/claude-proxy/internal/provider"
)

type credential struct {
	id, name, provider, plan, tier string
	createdAt                      int64
}

// cohort deliberately receives no model or routing weight. Neither is evidence
// of an upstream account's subscription plan, including for deleted credentials.
func cohort(c credential, exists bool) Group {
	g := Group{Provider: c.provider, Plan: strings.ToLower(strings.TrimSpace(c.plan)), Tier: normalizeTier(c.tier), Attribution: "credential"}
	if !exists {
		g.Provider, g.ProviderName = "unknown", "Unknown provider"
		g.Plan, g.Tier, g.Attribution = "unknown", "unknown", "unknown"
		g.Label = "Unknown or deleted credential"
	} else {
		if g.Provider == "" {
			g.Provider = string(provider.Default)
		}
		if g.Plan == "" {
			g.Plan = "unknown"
		}
		g.ProviderName = g.Provider
		for _, p := range provider.All() {
			if string(p.ID) != g.Provider {
				continue
			}
			g.ProviderName = p.Name
			if p.DelegatedAuth {
				g.Attribution, g.Plan, g.Tier = "gateway", "all", "unknown"
			}
			break
		}
		if g.Attribution == "gateway" {
			g.Label = g.ProviderName + " / All plans (gateway)"
		} else {
			plan := g.Plan
			if plan == "unknown" {
				plan = "Plan unknown"
			} else {
				plan = strings.ToUpper(plan[:1]) + plan[1:]
			}
			g.Label = g.ProviderName + " / " + plan
			if g.Tier != "unknown" {
				g.Label += " (" + g.Tier + ")"
			} else if g.Provider == string(provider.Anthropic) {
				g.Label += " (tier unknown)"
			}
		}
	}
	g.Key = strings.Join([]string{url.QueryEscape(g.Provider), url.QueryEscape(g.Plan), url.QueryEscape(g.Tier), g.Attribution}, "/")
	return g
}

func normalizeTier(raw string) string {
	tier := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case tier == "":
		return "unknown"
	case tier == "5x" || strings.HasSuffix(tier, "_5x"):
		return "5x"
	case tier == "20x" || strings.HasSuffix(tier, "_20x"):
		return "20x"
	default:
		// Preserve an unrecognized *recorded* tier, rather than inventing a
		// familiar one from a plan name or the operator's selection weight.
		return tier
	}
}

func (b *builder) group(c credential, exists bool) *Group {
	g := cohort(c, exists)
	if existing := b.groups[g.Key]; existing != nil {
		return existing
	}
	b.groups[g.Key] = &g
	return &g
}

func (b *builder) account(c credential, exists bool) *Account {
	if a := b.accounts[c.id]; a != nil {
		return a
	}
	g := b.group(c, exists)
	name := c.name
	if !exists {
		name = "Unknown or deleted credential"
		if c.id == "" {
			name = "Unattributed requests"
		}
	} else if name == "" {
		name = c.id
	}
	a := &Account{ID: c.id, Name: name, Provider: g.Provider, Plan: g.Plan, Tier: g.Tier, GroupKey: g.Key, Attribution: g.Attribution}
	b.accounts[c.id] = a
	if exists {
		g.Credentials++
		b.credentials[c.id] = c
	}
	return a
}
