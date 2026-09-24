package ci

// The project keeps its contribution templates for both GitHub (.github/) and
// GitLab (.gitlab/). The two platforms spell them differently — GitHub issue
// forms are YAML with required fields, GitLab templates are Markdown with quick
// actions — so they cannot be one file. These tests keep the pairs saying the
// same thing: a checklist item added on one side only would silently drop an
// invariant from half the contributors' view.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
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

// sections splits a GitLab template into the text before its first "## "
// heading and a map from each heading to the text under it.
func sections(doc string) (header string, bodies map[string]string) {
	bodies = map[string]string{}
	locs := headingRe.FindAllStringSubmatchIndex(doc, -1)
	if len(locs) == 0 {
		return doc, bodies
	}
	header = doc[:locs[0][0]]
	for i, loc := range locs {
		end := len(doc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		bodies[doc[loc[2]:loc[3]]] = doc[loc[1]:end]
	}
	return header, bodies
}

// gitlabOnlyNotes are the header paragraphs a GitLab issue template adds on
// purpose — the mirror note (GitLab cannot enforce required fields) and where
// security reports go — identified by how they start.
var gitlabOnlyNotes = []string{"Mirrors ", "Security vulnerability?"}

// firstComment returns the text inside the first HTML comment in s.
func firstComment(s string) string {
	start := strings.Index(s, "<!--")
	if start < 0 {
		return ""
	}
	rest := s[start+len("<!--"):]
	if end := strings.Index(rest, "-->"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// paragraphs splits text on blank lines and normalizes each paragraph.
func paragraphs(s string) []string {
	var out []string
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(s, -1) {
		if p = normalize(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalize collapses whitespace so prose wrapped differently on the two
// platforms (YAML strings vs Markdown comments) still compares equal.
func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

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
	// Section by section, everything else must match too: guidance comments,
	// defaults such as "none", provenance prompts. Only the platform's name
	// for the thing ("MR"), the text before the first heading, and each
	// file's closing note and quick actions are allowed to differ.
	_, ghSections := sections(trimTrailer(gh))
	_, glSections := sections(trimTrailer(gl))
	for heading, ghBody := range ghSections {
		glBody := mrWordRe.ReplaceAllString(glSections[heading], "PR")
		if normalize(ghBody) != normalize(glBody) {
			t.Errorf("section %q differs:\n  GitHub: %q\n  GitLab: %q", heading, normalize(ghBody), normalize(glBody))
		}
	}
}

var (
	mrWordRe = regexp.MustCompile(`\bMR\b`)
)

// trimTrailer drops a template's closing HTML comments and quick-action lines
// — the platform-specific "how to close an issue" note and GitLab's /draft.
func trimTrailer(doc string) string {
	for {
		doc = strings.TrimRight(doc, " \n")
		lastLine := doc[strings.LastIndex(doc, "\n")+1:]
		switch {
		case strings.HasSuffix(doc, "-->"):
			doc = doc[:strings.LastIndex(doc, "<!--")]
		case strings.HasPrefix(lastLine, "/"):
			doc = doc[:len(doc)-len(lastLine)]
		default:
			return doc
		}
	}
}

type issueForm struct {
	Labels []string `yaml:"labels"`
	Body   []struct {
		Type       string `yaml:"type"`
		Attributes struct {
			Label       string   `yaml:"label"`
			Description string   `yaml:"description"`
			Value       string   `yaml:"value"`
			Render      string   `yaml:"render"`
			Options     []string `yaml:"options"`
		} `yaml:"attributes"`
		Validations struct {
			Required bool `yaml:"required"`
		} `yaml:"validations"`
	} `yaml:"body"`
}

// issueTemplatePairs maps each GitHub issue form to its GitLab mirror. GitLab
// shows the file name in its template picker, hence the different spelling.
var issueTemplatePairs = map[string]string{
	"bug_report.yml":      "Bug.md",
	"feature_request.yml": "Feature_request.md",
}

func dirFiles(t *testing.T, dir, ext string, skip ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext || slices.Contains(skip, e.Name()) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestEveryTemplateHasAMirror catches a template added on one platform only —
// the pair tests below compare contents but cannot see a file nobody listed.
func TestEveryTemplateHasAMirror(t *testing.T) {
	var wantGH, wantGL []string
	for gh, gl := range issueTemplatePairs {
		wantGH, wantGL = append(wantGH, gh), append(wantGL, gl)
	}
	sort.Strings(wantGH)
	sort.Strings(wantGL)
	if got := dirFiles(t, "../.github/ISSUE_TEMPLATE", ".yml", "config.yml"); !reflect.DeepEqual(got, wantGH) {
		t.Errorf("GitHub issue forms %q, but issueTemplatePairs pairs %q — add the GitLab mirror and the pair", got, wantGH)
	}
	if got := dirFiles(t, "../.gitlab/issue_templates", ".md"); !reflect.DeepEqual(got, wantGL) {
		t.Errorf("GitLab issue templates %q, but issueTemplatePairs pairs %q — add the GitHub form and the pair", got, wantGL)
	}
	if got := dirFiles(t, "../.gitlab/merge_request_templates", ".md"); !reflect.DeepEqual(got, []string{"Default.md"}) {
		t.Errorf("GitLab MR templates %q; only Default.md mirrors .github/PULL_REQUEST_TEMPLATE.md", got)
	}
}

func TestGitLabIssueTemplatesMatchGitHubForms(t *testing.T) {
	for ghName, glName := range issueTemplatePairs {
		ghPath, glPath := "../.github/ISSUE_TEMPLATE/"+ghName, "../.gitlab/issue_templates/"+glName
		var form issueForm
		if err := yaml.Unmarshal([]byte(readFile(t, ghPath)), &form); err != nil {
			t.Fatalf("parse %s: %v", ghPath, err)
		}
		gl := readFile(t, glPath)

		header, bodies := sections(gl)
		var fields, intro []string
		for _, el := range form.Body {
			if el.Type == "markdown" {
				intro = append(intro, paragraphs(el.Attributes.Value)...)
				continue
			}
			label := el.Attributes.Label
			fields = append(fields, label)
			body := bodies[label]
			if d := el.Attributes.Description; d != "" && normalize(firstComment(body)) != normalize(d) {
				t.Errorf("%s: section %q guidance differs from the form's description:\n  form:     %q\n  template: %q", glPath, label, normalize(d), normalize(firstComment(body)))
			}
			if r := el.Attributes.Render; r != "" && !strings.Contains(body, "```"+r+"\n") {
				t.Errorf("%s: section %q needs a ```%s block to match the form's render", glPath, label, r)
			}
			// GitLab cannot enforce required fields; its templates say every
			// section is required unless the heading ends "(optional)". The
			// heading equals the form label, so pin that marker to the form's
			// own required flag.
			if optional := strings.HasSuffix(label, "(optional)"); optional == el.Validations.Required {
				t.Errorf("%s: field %q has required=%v, but its label says optional=%v — GitLab reporters read the label", ghPath, label, el.Validations.Required, optional)
			}
			// Exact, ordered comparison: an option dropped or renamed on
			// either side is drift, not only one missing from GitLab.
			want, got := el.Attributes.Options, matches(optionRe, body)
			if len(want) != 0 || len(got) != 0 {
				if !reflect.DeepEqual(want, got) {
					t.Errorf("%s: options of %q differ:\n  form:     %q\n  template: %q", glPath, label, want, got)
				}
			}
		}
		// The header comment must say exactly what the form's markdown says,
		// apart from the GitLab-only notes named in gitlabOnlyNotes.
		var glIntro []string
		for _, para := range paragraphs(firstComment(header)) {
			if !slices.ContainsFunc(gitlabOnlyNotes, func(p string) bool { return strings.HasPrefix(para, p) }) {
				glIntro = append(glIntro, para)
			}
		}
		if !reflect.DeepEqual(intro, glIntro) {
			t.Errorf("%s: header text differs from %s's intro:\n  form:     %q\n  template: %q", glPath, ghPath, intro, glIntro)
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
	// Every file on disk, not a list: a template the pair tests do not know
	// about yet must still apply only defined labels.
	for _, name := range dirFiles(t, "../.github/ISSUE_TEMPLATE", ".yml", "config.yml") {
		path := "../.github/ISSUE_TEMPLATE/" + name
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
	for _, dir := range []string{"../.gitlab/issue_templates", "../.gitlab/merge_request_templates"} {
		for _, name := range dirFiles(t, dir, ".md") {
			path := dir + "/" + name
			for _, l := range quickActionLabels(readFile(t, path)) {
				if !defined[l] {
					t.Errorf("%s applies label %q, which .github/labels.yml does not define", path, l)
				}
			}
		}
	}
}
