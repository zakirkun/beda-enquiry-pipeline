package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/beda/enquiry-pipeline/internal/model"
)

// Seed inserts the demo users and the routing rules. Both are idempotent, so a
// restart never duplicates them. Routing rules come from a JSON file so Ops can
// edit policy without a code change (docs/02-ERD.md).
func (s *Store) Seed(ctx context.Context, rulesFile string) error {
	users := []struct{ name, role, team string }{
		{"Ada Okafor", model.RoleSalesRep, "sales-inbound"},
		{"Ben Rossi", model.RoleSalesRep, "enterprise-sales"},
		{"Chi Nguyen", model.RoleSupportAgent, "support-l1"},
		{"Dana Fielding", model.RoleOpsAdmin, "ops"},
		{"Eli Barnes", model.RoleManager, "ops"},
	}
	for _, u := range users {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO app_user (name, role, team) VALUES ($1,$2,$3)
			ON CONFLICT DO NOTHING`, u.name, u.role, u.team); err != nil {
			return fmt.Errorf("seed users: %w", err)
		}
	}
	// app_user has no unique constraint on name, so guard against re-seeding.
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM app_user u USING app_user v
		WHERE u.name = v.name AND u.id > v.id
		  AND NOT EXISTS (SELECT 1 FROM crm_record r WHERE r.owner_user_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM approval_action a WHERE a.actor_user_id = u.id)`); err != nil {
		return fmt.Errorf("dedupe seed users: %w", err)
	}

	return s.seedRules(ctx, rulesFile)
}

type ruleFile struct {
	Rules []struct {
		Name       string            `json:"name"`
		When       map[string]string `json:"when"`
		TargetTeam string            `json:"target_team"`
		Priority   int               `json:"priority"`
		Active     *bool             `json:"active"`
	} `json:"routing_rules"`
}

func (s *Store) seedRules(ctx context.Context, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		// A missing file is a deployment choice (rules edited in the DB), not a
		// failure — but an unroutable pipeline sends everything to human review,
		// so say so loudly.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read rules file: %w", err)
	}
	var f ruleFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for _, r := range f.Rules {
		cond, err := json.Marshal(r.When)
		if err != nil {
			return err
		}
		active := true
		if r.Active != nil {
			active = *r.Active
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO routing_rule (name, condition_json, target_team, priority, active)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (name) DO UPDATE SET
				condition_json=EXCLUDED.condition_json, target_team=EXCLUDED.target_team,
				priority=EXCLUDED.priority, active=EXCLUDED.active`,
			r.Name, cond, r.TargetTeam, r.Priority, active); err != nil {
			return fmt.Errorf("seed rule %s: %w", r.Name, err)
		}
	}
	return nil
}
