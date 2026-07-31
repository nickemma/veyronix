// Package engine holds the pure architecture-rule logic for archlint.
//
// Four rule families are enforced:
//
//	Horizontal — feature modules under internal/modules are isolated from each
//	             other. A module may not import another module unless an Allow
//	             edge explicitly permits it. New modules are walled off by
//	             default.
//
//	Vertical   — the hexagon layers inside a module obey inward-pointing deps:
//	             domain (pure) <- ports <- application ;  adapters implement
//	             ports and are the only layer allowed to touch infra/frameworks.
//
//	Contract   — sdk/ is the published provider contract. It may not import
//	             anything internal, and domain may not import it: domain
//	             describes deployments, sdk describes deployment targets.
//
//	Zone       — internal/platform is a technical kernel and may not know about
//	             feature modules; the composition root (cmd, internal/bootstrap)
//	             may import anything and nothing may import it.
package engine

import "strings"

// Edge is a permitted cross-module import, expressed as module-relative path
// roots (e.g. From "internal/modules/deployment/application",
// To "internal/modules/secrets/ports").
type Edge struct {
	From string
	To   string
}

// Config describes the architecture to enforce. All path fields are relative
// to ModuleRoot.
type Config struct {
	ModuleRoot string   // go module path, e.g. github.com/nickemma/veyronix
	Modules    []string // feature module roots
	Allow      []Edge   // permitted cross-module edges (consulted before deny)
	Infra      []string // import prefixes treated as framework/infra
	Contracts  []string // published contract roots, e.g. "sdk"
	Platform   []string // technical kernel roots, e.g. "internal/platform"
	Shared     []string // dependency-free helper roots, e.g. "internal/shared"
	Exempt     []string // composition roots, e.g. "cmd", "internal/bootstrap"
}

// Package is a single Go package and its import paths (full paths).
type Package struct {
	Path    string
	Imports []string
}

// Violation is one broken rule.
type Violation struct {
	From string
	To   string
	Kind string // "horizontal" | "vertical" | "contract" | "zone"
	Rule string
}

func (v Violation) String() string {
	return "✗ [" + v.Kind + "] " + v.From + "\n      imports " + v.To + "\n      → " + v.Rule
}

// Check returns every architecture violation in pkgs. Packages outside
// ModuleRoot (stdlib, third-party) are never the source of a violation.
func Check(cfg Config, pkgs []Package) []Violation {
	var out []Violation
	for _, p := range pkgs {
		fromRel := cfg.rel(p.Path)
		if fromRel == "" {
			continue // not our code
		}
		if cfg.isExempt(fromRel) {
			continue // composition root: wires everything, constrained by nothing
		}

		fromMod := cfg.moduleOf(fromRel)
		fromLayer := layerOf(fromRel)

		for _, imp := range p.Imports {
			impRel := cfg.rel(imp)
			internal := impRel != ""

			// Nobody imports the composition root. It is the top of the graph.
			if internal && cfg.isExempt(impRel) {
				out = append(out, Violation{p.Path, imp, "zone",
					"nothing may import the composition root — it wires the graph, it is not part of it"})
				continue
			}

			// sdk/ is published to the world; it may not reach back inside.
			if cfg.isContract(fromRel) {
				if internal && !cfg.isContract(impRel) {
					out = append(out, Violation{p.Path, imp, "contract",
						"sdk is a published contract — it may not import anything under internal/"})
				}
				continue
			}

			// internal/platform is a technical kernel: no domain knowledge.
			if cfg.isPlatform(fromRel) {
				if internal && !cfg.isPlatform(impRel) && !cfg.isShared(impRel) {
					out = append(out, Violation{p.Path, imp, "zone",
						"platform is a technical kernel — it must not know about modules, sdk, or providers"})
				}
				continue
			}

			// Vertical rules apply only when the source sits in a hexagon layer.
			if fromLayer != "" {
				if rule, bad := cfg.violatesVertical(fromMod, fromLayer, imp, impRel); bad {
					out = append(out, Violation{p.Path, imp, "vertical", rule})
					continue // one violation per edge is enough
				}
			}

			// Horizontal rules: cross-module imports between feature modules.
			if internal {
				impMod := cfg.moduleOf(impRel)
				if fromMod != "" && impMod != "" && impMod != fromMod && !cfg.allowed(fromRel, impRel) {
					out = append(out, Violation{p.Path, imp, "horizontal",
						fromMod + " may not import another module (" + impMod + "); add an Allow edge if this is intended"})
				}
			}
		}
	}
	return out
}

// violatesVertical evaluates the inward-pointing layer rules.
func (c Config) violatesVertical(fromMod, fromLayer, imp, impRel string) (string, bool) {
	infra := c.matchesInfra(imp)
	internal := impRel != ""
	impMod := c.moduleOf(impRel)
	impLayer := layerOf(impRel)
	sameMod := internal && impMod != "" && impMod == fromMod
	contract := internal && c.isContract(impRel)

	switch fromLayer {
	case "domain":
		if infra {
			return "domain must be pure — no framework/infra imports", true
		}
		if contract {
			return "domain must not import sdk — domain describes deployments, sdk describes targets", true
		}
		if internal && !(sameMod && impLayer == "domain") {
			return "domain may import only its own module's domain", true
		}

	case "ports":
		if infra {
			return "ports must speak in domain terms — no framework/infra imports", true
		}
		if internal {
			switch {
			case sameMod && (impLayer == "domain" || impLayer == "ports"):
			case contract: // ports name sdk.Provider — that is the seam
			case c.isShared(impRel):
			default:
				return "ports may import only its own module's domain, sdk, and shared", true
			}
		}

	case "application":
		if infra {
			return "application must not import frameworks/infra — depend on a port", true
		}
		if internal {
			switch {
			case sameMod && (impLayer == "domain" || impLayer == "ports"):
			case contract:
			case c.isShared(impRel):
			case sameMod && impLayer == "adapters":
				return "application must not import its own adapters — inject the port", true
			case c.isPlatform(impRel):
				return "application must not import platform directly — go through a port", true
			case impMod != "" && impMod != fromMod:
				// cross-module: left to the horizontal/Allow check, not vertical
			default:
				return "application may import only its own domain/ports, sdk, and shared", true
			}
		}

	case "adapters":
		// adapters MAY import infra/frameworks — that's their job. Only the
		// internal shape is constrained.
		if internal {
			switch {
			case sameMod && (impLayer == "domain" || impLayer == "ports"):
			case contract:
			case c.isShared(impRel):
			case c.isPlatform(impRel):
			case sameMod && impLayer == "application":
				return "adapters must not import application — adapters implement ports", true
			case impMod != "" && impMod != fromMod:
				// cross-module: left to the horizontal/Allow check
			default:
				return "adapters may import only its own domain/ports, sdk, platform, and shared", true
			}
		}
	}
	return "", false
}

// ── helpers ──────────────────────────────────────────────────────────────

// rel returns path relative to the module root, or "" if path is external.
func (c Config) rel(path string) string {
	if path == c.ModuleRoot {
		return "."
	}
	if trimmed := strings.TrimPrefix(path, c.ModuleRoot+"/"); trimmed != path {
		return trimmed
	}
	return ""
}

func (c Config) moduleOf(rel string) string {
	for _, m := range c.Modules {
		if under(rel, m) {
			return m
		}
	}
	return ""
}

func layerOf(rel string) string {
	for _, l := range [...]string{"domain", "ports", "application", "adapters"} {
		if strings.Contains(rel, "/"+l+"/") || strings.HasSuffix(rel, "/"+l) {
			return l
		}
	}
	return ""
}

func (c Config) matchesInfra(imp string) bool {
	for _, f := range c.Infra {
		if imp == f || strings.HasPrefix(imp, f+"/") {
			return true
		}
	}
	return false
}

func (c Config) allowed(fromRel, impRel string) bool {
	for _, e := range c.Allow {
		if under(fromRel, e.From) && under(impRel, e.To) {
			return true
		}
	}
	return false
}

func under(rel, root string) bool {
	return rel == root || strings.HasPrefix(rel, root+"/")
}

func anyUnder(rel string, roots []string) bool {
	for _, r := range roots {
		if under(rel, r) {
			return true
		}
	}
	return false
}

func (c Config) isContract(rel string) bool { return anyUnder(rel, c.Contracts) }
func (c Config) isPlatform(rel string) bool { return anyUnder(rel, c.Platform) }
func (c Config) isShared(rel string) bool   { return anyUnder(rel, c.Shared) }
func (c Config) isExempt(rel string) bool   { return anyUnder(rel, c.Exempt) }

// DefaultConfig is the architecture of record for Veyronix. Modules grow this
// list as they are born — never before — and Allow stays empty while cross-
// module wiring lives in the composition root (internal/bootstrap).
func DefaultConfig() Config {
	return Config{
		ModuleRoot: "github.com/nickemma/veyronix",
		Modules: []string{
			"internal/modules/identity",
			"internal/modules/authz",
			"internal/modules/project",
			"internal/modules/secrets",
			"internal/modules/deployment",
			"internal/modules/audit",
			"internal/modules/notification",

			// Provider adapters are a module too, so the horizontal rule keeps
			// them out of the engine's internals. They see sdk and infra only.
			// Split into one entry per provider if you want them isolated from
			// each other as well.
			"internal/providers",
		},
		Allow: []Edge{
			// Cross-module imports are denied by default. When a module
			// legitimately needs another's PORT, add the edge here, e.g.:
			//   {From: "internal/modules/deployment/application",
			//    To:   "internal/modules/secrets/ports"},
		},
		Contracts: []string{"sdk"},
		Platform:  []string{"internal/platform"},
		Shared:    []string{"internal/shared"},
		Exempt:    []string{"cmd", "internal/bootstrap"},
		Infra: []string{
			"connectrpc.com/connect",
			"github.com/jackc/pgx",
			"github.com/nats-io/nats.go",
			"github.com/prometheus/client_golang",
			"go.opentelemetry.io/otel",
			"github.com/docker/docker",
			"golang.org/x/crypto",
			"google.golang.org/protobuf",
			"net/http",
			"database/sql",
			"os/exec",
		},
	}
}
