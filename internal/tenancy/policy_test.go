package tenancy

import "testing"

func TestPolicyAuthorizationAndQuota(t *testing.T) {
	policy := NewPolicy()
	if err := policy.Register(Team{Name: "payments", Members: []string{"alice"}, Namespace: "plinth-payments", ServiceQuota: 2}); err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize("alice", "payments", "apply"); err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize("mallory", "payments", "apply"); err == nil {
		t.Fatal("expected unauthorized actor to be rejected")
	}
	if !policy.WithinQuota("payments", []string{"api"}, "api") {
		t.Fatal("existing service should not consume an additional quota slot")
	}
	if policy.WithinQuota("payments", []string{"api", "worker"}, "new") {
		t.Fatal("expected service quota to reject a third service")
	}
}

func TestPolicyRejectsInvalidNamespace(t *testing.T) {
	policy := NewPolicy()
	if err := policy.Register(Team{Name: "payments", Members: []string{"alice"}, Namespace: "Not A Namespace", ServiceQuota: 1}); err == nil {
		t.Fatal("expected invalid namespace to be rejected")
	}
}

func TestPolicyRejectsNamespaceReuseAcrossTeams(t *testing.T) {
	policy := NewPolicy()
	if err := policy.Register(Team{Name: "payments", Members: []string{"alice"}, Namespace: "plinth-shared", ServiceQuota: 1}); err != nil {
		t.Fatal(err)
	}
	if err := policy.Register(Team{Name: "analytics", Members: []string{"bob"}, Namespace: "plinth-shared", ServiceQuota: 1}); err == nil {
		t.Fatal("expected namespace reuse to be rejected")
	}
}
