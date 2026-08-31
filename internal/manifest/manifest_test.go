package manifest

import "testing"

func TestParseYAML(t *testing.T) {
	input := "name: tessera-gateway\n" +
		"image: ghcr.io/nickemma/tessera:v0.4.1\n" +
		"port: 8080\n" +
		"replicas: 3\n" +
		"env:\n" +
		"  LOG_LEVEL: info\n" +
		"secrets:\n" +
		"  - DATABASE_URL\n" +
		"resources:\n" +
		"  cpu: 500m\n" +
		"  memory: 512Mi\n" +
		"rollout:\n" +
		"  enabled: true\n" +
		"  steps: [10, 100]\n" +
		"  max_error_rate: 0.05\n"
	m, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "tessera-gateway" || m.Replicas != 3 || m.Env["LOG_LEVEL"] != "info" || len(m.Secrets) != 1 || len(m.Rollout.Steps) != 2 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

func TestRejectsInvalidManifest(t *testing.T) {
	input := "name: Bad Name\nimage: test\nport: 8080\nreplicas: 1\n" +
		"resources:\n  cpu: 1\n  memory: 1\n"
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRejectsInvalidResourceQuantity(t *testing.T) {
	m := Manifest{Name: "tessera", Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: Resources{CPU: "not-a-quantity", Memory: "128Mi"}}
	if err := m.Validate(); err == nil {
		t.Fatal("expected invalid CPU quantity to be rejected")
	}
}

func TestRejectsInvalidOrDuplicateEnvironmentNames(t *testing.T) {
	base := Manifest{Name: "tessera", Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: Resources{CPU: "100m", Memory: "128Mi"}}
	base.Env = map[string]string{"bad name": "value"}
	if err := base.Validate(); err == nil {
		t.Fatal("expected invalid environment name to be rejected")
	}
	base.Env = map[string]string{"DATABASE_URL": "inline"}
	base.Secrets = []string{"DATABASE_URL"}
	if err := base.Validate(); err == nil {
		t.Fatal("expected duplicate environment and secret name to be rejected")
	}
}
