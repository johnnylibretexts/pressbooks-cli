package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The four published artifacts each carry a hand-copied version of the error
// code table that `schema --agent` emits. Nothing generates them, so they can
// silently disagree with the binary — and once did: blocked_by_host was
// corrected to retryable:true in the schema while all three of README.md,
// spec.yaml and tools-manifest.json still published retryable:false. An agent
// reading the docs would have given up on a host that was merely throttling.
//
// This test is the enforcement that was missing. It reads each artifact from
// disk and checks that every code the schema declares appears there with the
// same retryable value.
func TestPublishedDocsMatchTheSchemaErrorCodes(t *testing.T) {
	schema := currentAgentSchema()
	if len(schema.ErrorCodes) == 0 {
		t.Fatal("the schema declares no error codes; this test would pass vacuously")
	}

	root := repoRoot(t)
	for _, f := range []struct {
		file string
		// find reports the retryable value this artifact publishes for code,
		// and whether it mentions the code at all.
		find func(body, code string) (retryable, found bool)
	}{
		{"tools-manifest.json", findJSONish},
		{"spec.yaml", findYAMLish},
		{"README.md", findMarkdownTable},
	} {
		body, err := os.ReadFile(filepath.Join(root, f.file))
		if err != nil {
			t.Fatalf("reading %s: %v", f.file, err)
		}
		for _, c := range schema.ErrorCodes {
			retryable, found := f.find(string(body), c.Code)
			if !found {
				t.Errorf("%s does not publish error code %q, which the schema emits", f.file, c.Code)
				continue
			}
			if retryable != c.Retryable {
				t.Errorf("%s publishes %q as retryable=%v; the schema says %v", f.file, c.Code, retryable, c.Retryable)
			}
		}
	}
}

// findJSONish matches {"code": "x", "retryable": true, ...}.
func findJSONish(body, code string) (bool, bool) {
	re := regexp.MustCompile(`\{"code": "` + regexp.QuoteMeta(code) + `", "retryable": (true|false)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return false, false
	}
	return m[1] == "true", true
}

// findYAMLish matches {code: x, retryable: true, ...}.
func findYAMLish(body, code string) (bool, bool) {
	re := regexp.MustCompile(`\{code: ` + regexp.QuoteMeta(code) + `, retryable: (true|false)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return false, false
	}
	return m[1] == "true", true
}

// findMarkdownTable matches | `x` | yes | ... |.
func findMarkdownTable(body, code string) (bool, bool) {
	re := regexp.MustCompile("\\|\\s*`" + regexp.QuoteMeta(code) + "`\\s*\\|\\s*(yes|no)\\s*\\|")
	m := re.FindStringSubmatch(body)
	if m == nil {
		return false, false
	}
	return m[1] == "yes", true
}

// repoRoot walks up from the test's working directory to the module root, so
// the test does not depend on how deep the package sits.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root above the test's working directory")
	return ""
}

// SKILL.md lists the codes as prose rather than a table, so it is checked for
// mention only — a code the schema emits must at least appear there.
func TestSkillDocMentionsEveryErrorCode(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "SKILL.md"))
	if err != nil {
		t.Fatalf("reading SKILL.md: %v", err)
	}
	for _, c := range currentAgentSchema().ErrorCodes {
		if !strings.Contains(string(body), fmt.Sprintf("`%s`", c.Code)) {
			t.Errorf("SKILL.md does not mention error code %q", c.Code)
		}
	}
}
