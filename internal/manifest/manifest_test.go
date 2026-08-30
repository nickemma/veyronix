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
		"  memory: 512Mi\n"
	m, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "tessera-gateway" || m.Replicas != 3 || m.Env["LOG_LEVEL"] != "info" || len(m.Secrets) != 1 {
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
