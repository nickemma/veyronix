package tenancy

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/util/validation"
)

type Team struct {
	Name         string   `json:"name"`
	Members      []string `json:"members"`
	Namespace    string   `json:"namespace"`
	ServiceQuota int      `json:"service_quota"`
}

type Policy struct {
	mu    sync.RWMutex
	teams map[string]Team
}

func NewPolicy() *Policy {
	return &Policy{teams: map[string]Team{"default": {Name: "default", Members: []string{"local", "*"}, Namespace: "plinth-default", ServiceQuota: 100}}}
}

func (p *Policy) Register(team Team) error {
	if team.Name == "" || team.Namespace == "" || team.ServiceQuota < 1 {
		return fmt.Errorf("team requires name, namespace, and positive service quota")
	}
	if problems := validation.IsDNS1123Subdomain(team.Name); len(problems) > 0 {
		return fmt.Errorf("team name must be DNS-safe: %s", problems[0])
	}
	if problems := validation.IsDNS1123Subdomain(team.Namespace); len(problems) > 0 {
		return fmt.Errorf("team namespace must be DNS-safe: %s", problems[0])
	}
	p.mu.Lock()
	for name, existing := range p.teams {
		if name != team.Name && existing.Namespace == team.Namespace {
			p.mu.Unlock()
			return fmt.Errorf("namespace %q is already assigned to team %q", team.Namespace, name)
		}
	}
	p.teams[team.Name] = team
	p.mu.Unlock()
	return nil
}

func (p *Policy) Authorize(actor, team, action string) error {
	p.mu.RLock()
	configured, ok := p.teams[team]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("team %q does not exist", team)
	}
	if actor == "local" || actor == "admin" || actor == "" {
		return nil
	}
	for _, member := range configured.Members {
		if member == "*" || member == actor {
			return nil
		}
	}
	return fmt.Errorf("actor %q is not authorized to %s for team %q", actor, action, team)
}

func (p *Policy) Team(name string) (Team, bool) {
	p.mu.RLock()
	team, ok := p.teams[name]
	p.mu.RUnlock()
	return team, ok
}

func (p *Policy) List() []Team {
	p.mu.RLock()
	result := make([]Team, 0, len(p.teams))
	for _, team := range p.teams {
		result = append(result, team)
	}
	p.mu.RUnlock()
	return result
}

func (p *Policy) WithinQuota(team string, services []string, candidate string) bool {
	configured, ok := p.Team(team)
	if !ok {
		return false
	}
	count := 0
	for _, service := range services {
		if service != candidate {
			count++
		}
	}
	return count < configured.ServiceQuota
}
