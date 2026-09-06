package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaEmitsOneCompactLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := executeArgs([]string{"schema", "--agent"}, &stdout, &stderr); err != nil {
		t.Fatalf("schema --agent: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("agent JSON must be one compact line, got %q", stdout.String())
	}
	var got struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Data          struct {
			Commands []struct {
				Name string `json:"name"`
			} `json:"commands"`
			ErrorCodes []struct {
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
				Meaning   string `json:"meaning"`
			} `json:"error_codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if got.SchemaVersion != agentSchemaVersion || !got.OK {
		t.Fatalf("unexpected envelope: %#v", got)
	}
	if len(got.Data.Commands) != 9 {
		t.Fatalf("got %d commands, want 9", len(got.Data.Commands))
	}
	// The Minor fix: schema --agent must publish every error code the tool
	// can emit, with a meaning for each, so an agent that reads this once can
	// discover the rest without guessing from a hardcoded "for example" list.
	if len(got.Data.ErrorCodes) == 0 {
		t.Fatal("schema must publish error_codes")
	}
	for _, code := range got.Data.ErrorCodes {
		if code.Code == "" || code.Meaning == "" {
			t.Fatalf("error_codes entry missing code or meaning: %#v", code)
		}
	}
}

// A harness that captures only stdout must still receive a structured failure.
func TestAgentErrorsGoToStdoutWithANonzeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := executeArgs([]string{"does-not-exist", "--agent"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !ErrorAlreadyReported(err) {
		t.Fatalf("agent error should be marked reported: %v", err)
	}
	if ExitCode(err) == 0 {
		t.Fatal("an agent error must still exit nonzero")
	}
	if stderr.Len() != 0 {
		t.Fatalf("agent errors belong on stdout, stderr had: %s", stderr.String())
	}
	var got struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Suggestion string `json:"suggestion"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if got.OK || got.Error.Code != "unknown_command" || got.Error.Suggestion == "" {
		t.Fatalf("unexpected error payload: %#v", got)
	}
}

func TestAgentWindowPaginates(t *testing.T) {
	start, end, meta, err := agentWindow(37, 0, 0, 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 || end != 5 || !meta.HasMore || meta.NextOffset == nil || *meta.NextOffset != 5 {
		t.Fatalf("first window = %#v (%d..%d)", meta, start, end)
	}
	_, end, meta, err = agentWindow(37, 35, 5, 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if end != 37 || meta.HasMore || meta.NextOffset != nil {
		t.Fatalf("last window = %#v", meta)
	}
	if _, _, _, err := agentWindow(37, 0, 500, 5, 100); err == nil {
		t.Fatal("a limit above the maximum should be refused")
	}
	if _, _, _, err := agentWindow(37, -1, 5, 5, 100); err == nil {
		t.Fatal("a negative offset should be refused")
	}
}
