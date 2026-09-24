package ci

// The project keeps its contribution templates for both GitHub (.github/) and
// GitLab (.gitlab/). The two platforms spell them differently — GitHub issue
// forms are YAML with required fields, GitLab templates are Markdown with quick
// actions — so they cannot be one file. These tests keep the pairs saying the
// same thing: a checklist item added on one side only would silently drop an
// invariant from half the contributors' view.

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var (
	headingRe    = regexp.MustCompile(`(?m)^## (.+)$`)
	checkboxRe   = regexp.MustCompile(`(?m)^- \[ \] (.+)$`)
	quickLabelRe = regexp.MustCompile(`(?m)^/label (.+)$`)
	labelRefRe   = regexp.MustCompile(`~"([^"]+)"`)
	optionRe     = regexp.MustCompile(`(?m)^- (.+)$`)
)

// sectionOptions maps each "## " section of a GitLab template to the "- "
// option lines inside it — the Markdown stand-in for a form dropdown.
func sectionOptions(doc string) map[string][]string {
	out := map[string][]string{}
	locs := headingRe.FindAllStringSubmatchIndex(doc, -1)
	for i, loc := range locs {
		end := len(doc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[doc[loc[2]:loc[3]]] = matches(optionRe, doc[loc[1]:end])
	}
	return out
}

func matches(re *regexp.Regexp, doc string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(doc, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// quickActionLabels returns the labels applied by `/label ~"..."` quick actions.
func quickActionLabels(doc string) []string {
	var out []string
	for _, line := range matches(quickLabelRe, doc) {
		out = append(out, matches(labelRefRe, line)...)
	}
	return out
}

func definedLabels(t *testing.T) map[string]bool {
	t.Helper()
	var labels []struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, "../.github/labels.yml")), &labels); err != nil {
		t.Fatalf("parse labels.yml: %v", err)
	}
	out := map[string]bool{}
	for _, l := range labels {
		out[l.Name] = true
	}
	return out
}

func TestMergeRequestTemplateMatchesPullRequestTemplate(t *testing.T) {
	gh := readFile(t, "../.github/PULL_REQUEST_TEMPLATE.md")
	gl := readFile(t, "../.gitlab/merge_request_templates/Default.md")
	if a, b := matches(headingRe, gh), matches(headingRe, gl); !reflect.DeepEqual(a, b) {
		t.Errorf("section headings differ:\n  GitHub: %q\n  GitLab: %q", a, b)
	}
	if a, b := matches(checkboxRe, gh), matches(checkboxRe, gl); !reflect.DeepEqual(a, b) {
		t.Errorf("invariant checklists differ:\n  GitHub: %q\n  GitLab: %q", a, b)
	}
}

type issueForm struct {
	Labels []string `yaml:"labels"`
	Body   []struct {
		Type       string `yaml:"type"`
		Attributes struct {
			Label   string   `yaml:"label"`
			Options []string `yaml:"options"`
		} `yaml:"attributes"`
	} `yaml:"body"`
}

func TestGitLabIssueTemplatesMatchGitHubForms(t *testing.T) {
	pairs := map[string]string{
		"../.github/ISSUE_TEMPLATE/bug_report.yml":      "../.gitlab/issue_templates/Bug.md",
		"../.github/ISSUE_TEMPLATE/feature_request.yml": "../.gitlab/issue_templates/Feature_request.md",
	}
	for ghPath, glPath := range pairs {
		var form issueForm
		if err := yaml.Unmarshal([]byte(readFile(t, ghPath)), &form); err != nil {
			t.Fatalf("parse %s: %v", ghPath, err)
		}
		gl := readFile(t, glPath)

		options := sectionOptions(gl)
		var fields []string
		for _, el := range form.Body {
			if el.Type == "markdown" {
				continue
			}
			label := el.Attributes.Label
			fields = append(fields, label)
			// Exact, ordered comparison: an option dropped or renamed on
			// either side is drift, not only one missing from GitLab.
			want, got := el.Attributes.Options, options[label]
			if len(want) != 0 || len(got) != 0 {
				if !reflect.DeepEqual(want, got) {
					t.Errorf("%s: options of %q differ:\n  form:     %q\n  template: %q", glPath, label, want, got)
				}
			}
		}
		if headings := matches(headingRe, gl); !reflect.DeepEqual(fields, headings) {
			t.Errorf("%s: sections differ from %s's fields:\n  form:     %q\n  template: %q", glPath, ghPath, fields, headings)
		}
		if got := quickActionLabels(gl); !reflect.DeepEqual(form.Labels, got) {
			t.Errorf("%s: /label quick action applies %q, form applies %q", glPath, got, form.Labels)
		}
	}
}

func TestTemplateLabelsAreDefined(t *testing.T) {
	defined := definedLabels(t)
	for _, path := range []string{
		"../.github/ISSUE_TEMPLATE/bug_report.yml",
		"../.github/ISSUE_TEMPLATE/feature_request.yml",
	} {
		var form issueForm
		if err := yaml.Unmarshal([]byte(readFile(t, path)), &form); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, l := range form.Labels {
			if !defined[l] {
				t.Errorf("%s applies label %q, which .github/labels.yml does not define", path, l)
			}
		}
	}
	for _, path := range []string{
		"../.gitlab/issue_templates/Bug.md",
		"../.gitlab/issue_templates/Feature_request.md",
		"../.gitlab/merge_request_templates/Default.md",
	} {
		for _, l := range quickActionLabels(readFile(t, path)) {
			if !defined[l] {
				t.Errorf("%s applies label %q, which .github/labels.yml does not define", path, l)
			}
		}
	}
}
