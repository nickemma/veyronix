package engine

import "testing"

const root = "github.com/nickemma/veyronix"

func p(s string) string { return root + "/" + s }

func check(t *testing.T, from string, imports ...string) []Violation {
	t.Helper()
	return Check(DefaultConfig(), []Package{{Path: p(from), Imports: imports}})
}

// wantClean asserts the edge is legal.
func wantClean(t *testing.T, from string, imp string) {
	t.Helper()
	if v := check(t, from, imp); len(v) != 0 {
		t.Errorf("expected clean: %s -> %s\ngot: %s", from, imp, v[0])
	}
}

// wantViolation asserts the edge is illegal and of the expected kind.
func wantViolation(t *testing.T, kind, from, imp string) {
	t.Helper()
	v := check(t, from, imp)
	if len(v) == 0 {
		t.Errorf("expected %s violation: %s -> %s, got none", kind, from, imp)
		return
	}
	if v[0].Kind != kind {
		t.Errorf("expected kind %q, got %q for %s -> %s\n%s", kind, v[0].Kind, from, imp, v[0])
	}
}

func TestDomainIsPure(t *testing.T) {
	d := "internal/modules/deployment/domain"

	wantClean(t, d, "time")
	wantClean(t, d, "errors")
	wantClean(t, d, p("internal/modules/deployment/domain/state"))

	wantViolation(t, "vertical", d, "database/sql")
	wantViolation(t, "vertical", d, "github.com/jackc/pgx/v5")
	wantViolation(t, "vertical", d, p("sdk"))
	wantViolation(t, "vertical", d, p("internal/modules/deployment/ports"))
	wantViolation(t, "vertical", d, p("internal/platform/postgres"))
}

func TestPortsNameTheSeam(t *testing.T) {
	pt := "internal/modules/deployment/ports"

	wantClean(t, pt, "context")
	wantClean(t, pt, p("internal/modules/deployment/domain"))
	wantClean(t, pt, p("sdk")) // ports declare sdk.Provider — this is the seam
	wantClean(t, pt, p("internal/shared/pagination"))

	wantViolation(t, "vertical", pt, "net/http")
	wantViolation(t, "vertical", pt, p("internal/platform/postgres"))
	wantViolation(t, "vertical", pt, p("internal/modules/deployment/adapters/postgres"))
}

func TestApplicationDependsOnPortsOnly(t *testing.T) {
	a := "internal/modules/deployment/application"

	wantClean(t, a, "context")
	wantClean(t, a, p("internal/modules/deployment/domain"))
	wantClean(t, a, p("internal/modules/deployment/ports"))
	wantClean(t, a, p("sdk"))

	wantViolation(t, "vertical", a, "net/http")
	wantViolation(t, "vertical", a, "github.com/nats-io/nats.go")
	wantViolation(t, "vertical", a, p("internal/modules/deployment/adapters/postgres"))
	wantViolation(t, "vertical", a, p("internal/platform/postgres"))
}

func TestAdaptersMayTouchInfra(t *testing.T) {
	ad := "internal/modules/deployment/adapters/postgres"

	wantClean(t, ad, "github.com/jackc/pgx/v5")
	wantClean(t, ad, "database/sql")
	wantClean(t, ad, p("internal/modules/deployment/domain"))
	wantClean(t, ad, p("internal/modules/deployment/ports"))
	wantClean(t, ad, p("internal/platform/postgres"))
	wantClean(t, ad, p("sdk"))

	// The one thing adapters may not do: reach up into the use cases.
	wantViolation(t, "vertical", ad, p("internal/modules/deployment/application"))
}

func TestModulesAreWalledOff(t *testing.T) {
	a := "internal/modules/deployment/application"

	wantViolation(t, "horizontal", a, p("internal/modules/secrets/ports"))
	wantViolation(t, "horizontal", a, p("internal/modules/project/domain"))

	// ...until an Allow edge says otherwise.
	cfg := DefaultConfig()
	cfg.Allow = []Edge{{
		From: "internal/modules/deployment/application",
		To:   "internal/modules/secrets/ports",
	}}
	got := Check(cfg, []Package{{
		Path:    p(a),
		Imports: []string{p("internal/modules/secrets/ports")},
	}})
	if len(got) != 0 {
		t.Errorf("Allow edge ignored: %s", got[0])
	}
}

func TestProvidersSeeOnlySdkAndInfra(t *testing.T) {
	pr := "internal/providers/netlify"

	wantClean(t, pr, p("sdk"))
	wantClean(t, pr, "net/http")
	wantClean(t, pr, "encoding/json")

	// A provider that reaches into the engine has broken the whole premise.
	wantViolation(t, "horizontal", pr, p("internal/modules/deployment/domain"))
	wantViolation(t, "horizontal", pr, p("internal/modules/secrets/application"))
}

func TestSdkStaysPublishable(t *testing.T) {
	wantClean(t, "sdk", "context")
	wantClean(t, "sdk", "time")
	wantClean(t, "sdk/conformance", p("sdk"))

	wantViolation(t, "contract", "sdk", p("internal/modules/deployment/domain"))
	wantViolation(t, "contract", "sdk", p("internal/platform/crypto"))
}

func TestPlatformHasNoDomainKnowledge(t *testing.T) {
	pl := "internal/platform/postgres"

	wantClean(t, pl, "github.com/jackc/pgx/v5")
	wantClean(t, pl, p("internal/platform/config"))
	wantClean(t, pl, p("internal/shared/retry"))

	wantViolation(t, "zone", pl, p("internal/modules/deployment/domain"))
	wantViolation(t, "zone", pl, p("sdk"))
}

func TestCompositionRootIsTheTopOfTheGraph(t *testing.T) {
	// bootstrap wires everything and is constrained by nothing.
	boot := check(t, "internal/bootstrap",
		p("internal/modules/deployment/adapters/postgres"),
		p("internal/modules/secrets/application"),
		p("internal/providers/netlify"),
		p("internal/platform/postgres"),
		"github.com/nats-io/nats.go",
	)
	if len(boot) != 0 {
		t.Errorf("composition root should be exempt, got: %s", boot[0])
	}

	// ...and nothing imports it.
	wantViolation(t, "zone", "internal/modules/deployment/application", p("internal/bootstrap"))
	wantViolation(t, "zone", "internal/providers/netlify", p("cmd/veyronix-worker"))
}

func TestExternalPackagesAreNeverSources(t *testing.T) {
	got := Check(DefaultConfig(), []Package{{
		Path:    "github.com/some/library/pkg",
		Imports: []string{p("internal/modules/deployment/domain")},
	}})
	if len(got) != 0 {
		t.Errorf("external package reported as a violation source: %s", got[0])
	}
}

func TestOneViolationPerEdge(t *testing.T) {
	// domain -> another module's adapters breaks vertical AND horizontal.
	// Report it once, as the more specific vertical failure.
	got := check(t, "internal/modules/deployment/domain",
		p("internal/modules/secrets/adapters/vault"))
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 violation, got %d", len(got))
	}
	if got[0].Kind != "vertical" {
		t.Errorf("expected vertical, got %q", got[0].Kind)
	}
}
