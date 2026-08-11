package models

import "testing"

func TestAssignTiers(t *testing.T) {
	t.Run("shadowed model drops to tier 2 when both present", func(t *testing.T) {
		tiers := AssignTiers([]string{"gpt-5.6-sol", "gpt-5.5"})
		if tiers["gpt-5.6-sol"] != Tier1 {
			t.Errorf("sol tier = %d, want %d", tiers["gpt-5.6-sol"], Tier1)
		}
		if tiers["gpt-5.5"] != Tier2 {
			t.Errorf("gpt-5.5 tier = %d, want %d", tiers["gpt-5.5"], Tier2)
		}
	})

	t.Run("opus 5 shadows opus 4.8", func(t *testing.T) {
		tiers := AssignTiers([]string{"gpt-5.6-terra", "gpt-5.4-mini"})
		if tiers["gpt-5.6-terra"] != Tier1 {
			t.Errorf("terra tier = %d, want %d", tiers["gpt-5.6-terra"], Tier1)
		}
		if tiers["gpt-5.4-mini"] != Tier2 {
			t.Errorf("mini tier = %d, want %d", tiers["gpt-5.4-mini"], Tier2)
		}
	})

	t.Run("worse model stays tier 1 when better absent", func(t *testing.T) {
		tiers := AssignTiers([]string{"gpt-5.5"})
		if tiers["gpt-5.5"] != Tier1 {
			t.Errorf("gpt-5.5 tier = %d, want %d (no shadowing model present)", tiers["gpt-5.5"], Tier1)
		}
	})

	t.Run("unknown model defaults to tier 2", func(t *testing.T) {
		tiers := AssignTiers([]string{"some-brand-new-model"})
		if tiers["some-brand-new-model"] != Tier2 {
			t.Errorf("unknown tier = %d, want %d", tiers["some-brand-new-model"], Tier2)
		}
	})

	t.Run("known unshadowed model stays tier 1", func(t *testing.T) {
		tiers := AssignTiers([]string{"gpt-5.6-sol"})
		if tiers["gpt-5.6-sol"] != Tier1 {
			t.Errorf("known tier = %d, want %d", tiers["gpt-5.6-sol"], Tier1)
		}
	})

	t.Run("multiple shadows demote several models", func(t *testing.T) {
		avail := []string{"gpt-5.6-sol", "gpt-5.5", "gpt-5.4"}
		tiers := AssignTiers(avail)
		if tiers["gpt-5.6-sol"] != Tier1 {
			t.Errorf("sol tier = %d, want %d", tiers["gpt-5.6-sol"], Tier1)
		}
		for _, worse := range []string{"gpt-5.5", "gpt-5.4"} {
			if tiers[worse] != Tier2 {
				t.Errorf("%s tier = %d, want %d", worse, tiers[worse], Tier2)
			}
		}
	})

	t.Run("every input id is assigned a tier", func(t *testing.T) {
		avail := testIDs()
		tiers := AssignTiers(avail)
		for _, id := range avail {
			if tiers[id] != Tier1 && tiers[id] != Tier2 {
				t.Errorf("%s tier = %d, want 1 or 2", id, tiers[id])
			}
		}
	})
}
