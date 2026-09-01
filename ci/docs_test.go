package ci

// Executable docs/contract drift guards. AGENTS.md's definition of done warns
// that "the driver table and lifecycle table go stale silently" — these tests
// make that staleness a failing build instead of a review-time hope. Each test
// reconciles a piece of prose (README tables, AGENTS.md invariant paths, the
// gate's own file list) against the code that is its source of truth.
//
// Same conventions as chart_test.go: plain testing, paths relative to ci/.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// section returns README content from the given heading up to the next heading
// of the same or higher level.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	start := strings.Index(doc, "\n"+heading+"\n")
	if start < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	rest := doc[start+1+len(heading):]
	level := strings.Count(strings.Split(heading, " ")[0], "#")
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^#{1,%d} `, level))
	if loc := re.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDriverTableMatchesCodeAndSchema reconciles three lists that must be the
// same set: the README driver table, the CRD driver enum, and the driver names
// actually registered by the store packages. A driver added to any one of them
// without the other two fails here.
func TestDriverTableMatchesCodeAndSchema(t *testing.T) {
	readme := readFile(t, "../README.md")
	table := section(t, readme, "## Store drivers")
	rowRe := regexp.MustCompile("(?m)^\\| `([a-z0-9]+)` \\|")
	documented := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(table, -1) {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatal("no driver rows parsed from the README Store drivers table")
	}

	// The driver enum is the marker containing "artifactory" (the auth enum
	// contains "sigv4" instead).
	types := readFile(t, "../api/v1alpha1/artifactclass_types.go")
	enumRe := regexp.MustCompile(`validation:Enum=([a-z0-9;]*artifactory[a-z0-9;]*)`)
	em := enumRe.FindStringSubmatch(types)
	if em == nil {
		t.Fatal("driver enum marker not found in artifactclass_types.go")
	}
	inSchema := map[string]bool{}
	for _, d := range strings.Split(em[1], ";") {
		inSchema[d] = true
	}

	registered := map[string]bool{}
	regRe := regexp.MustCompile(`\.Register\("([a-z0-9]+)"`)
	err := filepath.WalkDir("../internal/store", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		for _, m := range regRe.FindAllStringSubmatch(readFile(t, path), -1) {
			registered[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/store: %v", err)
	}

	doc, sch, reg := sortedSet(documented), sortedSet(inSchema), sortedSet(registered)
	if fmt.Sprint(doc) != fmt.Sprint(sch) || fmt.Sprint(sch) != fmt.Sprint(reg) {
		t.Fatalf("driver lists diverged:\n  README table:   %v\n  CRD enum:       %v\n  Register calls: %v",
			doc, sch, reg)
	}
}

// TestHTTPAuthSchemesDocumented reconciles the http driver's auth enum against
// the README's http-driver section.
func TestHTTPAuthSchemesDocumented(t *testing.T) {
	types := readFile(t, "../api/v1alpha1/artifactclass_types.go")
	enumRe := regexp.MustCompile(`validation:Enum=([a-zA-Z;]*sigv4[a-zA-Z;]*)`)
	em := enumRe.FindStringSubmatch(types)
	if em == nil {
		t.Fatal("http auth enum marker not found")
	}
	httpDoc := section(t, readFile(t, "../README.md"), "### The `http` driver")
	for _, scheme := range strings.Split(em[1], ";") {
		if !strings.Contains(httpDoc, "`"+scheme+"`") {
			t.Errorf("auth scheme %q is in the CRD enum but not documented in the README http driver section", scheme)
		}
	}
}

// TestLifecycleTableCoversSpecFields asserts every user-facing lifecycle field
// on ArtifactSpec has a row in the README lifecycle table. One-way on purpose:
// prose-only rows (generator runs, class-level knobs) are legal.
func TestLifecycleTableCoversSpecFields(t *testing.T) {
	spec := readFile(t, "../api/v1alpha1/artifact_types.go")
	structRe := regexp.MustCompile(`(?s)type ArtifactSpec struct \{(.*?)\n\}`)
	sm := structRe.FindStringSubmatch(spec)
	if sm == nil {
		t.Fatal("ArtifactSpec struct not found")
	}
	tagRe := regexp.MustCompile(`json:"([a-zA-Z]+)`)
	// Identity-defining fields are documented in their own sections, not the
	// lifecycle table.
	notLifecycle := map[string]bool{"classRef": true, "identity": true, "params": true}

	table := section(t, readFile(t, "../README.md"), "## Lifecycle semantics")
	for _, m := range tagRe.FindAllStringSubmatch(sm[1], -1) {
		field := m[1]
		if notLifecycle[field] {
			continue
		}
		if !strings.Contains(table, "`"+field+"`") && !strings.Contains(table, field) {
			t.Errorf("ArtifactSpec field %q has no row in the README lifecycle table", field)
		}
	}
}

// TestStateVocabularyDocumented asserts the README's state enumeration names
// every State* constant.
func TestStateVocabularyDocumented(t *testing.T) {
	conds := readFile(t, "../api/v1alpha1/conditions.go")
	stateRe := regexp.MustCompile(`State\w+\s*=\s*"([A-Za-z]+)"`)
	lifecycle := section(t, readFile(t, "../README.md"), "## Lifecycle semantics")
	for _, m := range stateRe.FindAllStringSubmatch(conds, -1) {
		if !strings.Contains(lifecycle, m[1]) {
			t.Errorf("state %q (conditions.go) missing from the README state enumeration", m[1])
		}
	}
}

// TestNamedPathsExist asserts every repo path named by AGENTS.md invariant 3
// and every entry of ci/test.sh's generated-files freshness array actually
// exists — a renamed directory otherwise leaves the freshness check vacuously
// green (this happened: crds/ became crd-base/ and the gate guarded nothing).
func TestNamedPathsExist(t *testing.T) {
	check := func(origin, p string) {
		t.Helper()
		p = strings.TrimSuffix(p, "/*")
		if _, err := os.Stat(filepath.Join("..", p)); err != nil {
			t.Errorf("%s names %q, which does not exist", origin, p)
		}
	}

	gate := readFile(t, "test.sh")
	arrRe := regexp.MustCompile(`(?s)generated=\((.*?)\)`)
	am := arrRe.FindStringSubmatch(gate)
	if am == nil {
		t.Fatal("generated=() array not found in ci/test.sh")
	}
	for _, entry := range strings.Fields(am[1]) {
		check("ci/test.sh generated array", entry)
	}

	agents := readFile(t, "../AGENTS.md")
	inv3Re := regexp.MustCompile(`(?s)3\. \*\*Generated files.*?4\. \*\*`)
	im := inv3Re.FindString(agents)
	if im == "" {
		t.Fatal("invariant 3 block not found in AGENTS.md")
	}
	pathRe := regexp.MustCompile("`([a-zA-Z0-9_./-]+/[a-zA-Z0-9_./*-]+)`")
	for _, m := range pathRe.FindAllStringSubmatch(im, -1) {
		check("AGENTS.md invariant 3", m[1])
	}
}

// TestRBACPosture makes the README's "no wildcard cluster permissions" and
// "secrets stay namespaced" claims (invariants 6/7) executable, over both the
// kustomize RBAC and the rendered chart.
func TestRBACPosture(t *testing.T) {
	auditDoc := func(origin string, doc map[string]interface{}) {
		kind, _ := doc["kind"].(string)
		rules, _ := doc["rules"].([]interface{})
		for _, r := range rules {
			rule, _ := r.(map[string]interface{})
			for _, key := range []string{"apiGroups", "resources", "verbs"} {
				items, _ := rule[key].([]interface{})
				for _, it := range items {
					s, _ := it.(string)
					if s == "*" {
						t.Errorf("%s: %s grants wildcard %s — README promises no wildcard permissions", origin, kind, key)
					}
					if s == "secrets" && kind == "ClusterRole" {
						t.Errorf("%s: secrets granted in a ClusterRole — invariant 7 requires a namespaced Role", origin)
					}
				}
			}
		}
	}
	auditStream := func(origin string, data []byte) {
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var doc map[string]interface{}
			if err := dec.Decode(&doc); err != nil {
				return
			}
			if doc != nil {
				auditDoc(origin, doc)
			}
		}
	}

	entries, err := os.ReadDir("../config/rbac")
	if err != nil {
		t.Fatalf("read config/rbac: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p := filepath.Join("../config/rbac", e.Name())
		auditStream(p, []byte(readFile(t, p)))
	}

	// The chart's manager ClusterRole must be config/rbac/role.yaml verbatim
	// (plus its two-line generated header) — it is controller-gen output the
	// chart mirrors, never edits.
	chartRole := readFile(t, "../charts/artifact-controller/templates/manager-clusterrole.yaml")
	lines := strings.SplitN(chartRole, "\n", 3)
	if len(lines) < 3 || lines[2] != readFile(t, "../config/rbac/role.yaml") {
		t.Error("charts/.../templates/manager-clusterrole.yaml is not config/rbac/role.yaml plus its 2-line header — run make manifests")
	}

	if _, err := exec.LookPath("helm"); err != nil {
		t.Log("helm not installed; skipping rendered-chart RBAC audit")
		return
	}
	out, err := exec.Command("helm", "template", "rbacaudit", "../charts/artifact-controller",
		"-n", "artifact-system", "-f", "../charts/artifact-controller/ci-values.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	auditStream("rendered chart", out)
}

// TestFakeDriverStaysGated asserts invariant 9 at the wiring level: the fake
// store registers only behind --enable-fake-store.
func TestFakeDriverStaysGated(t *testing.T) {
	main := readFile(t, "../cmd/main.go")
	if strings.Count(main, "fake.Register") != 1 {
		t.Fatal("expected exactly one fake.Register call in cmd/main.go")
	}
	gated := regexp.MustCompile(`(?s)if enableFakeStore \{[^}]*fake\.Register`)
	if !gated.MatchString(main) {
		t.Error("fake.Register is not inside the enableFakeStore conditional — invariant 9")
	}
}
