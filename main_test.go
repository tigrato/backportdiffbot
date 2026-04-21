package main

import (
	"strings"
	"testing"
)

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		input      string
		wantRepo   string
		wantNumber int
		wantBase   string
		wantErr    bool
	}{
		{
			input:      "https://github.com/acme/myrepo/pull/42",
			wantRepo:   "acme/myrepo",
			wantNumber: 42,
			wantBase:   "https://api.github.com",
		},
		{
			input:      "http://github.com/acme/myrepo/pull/42",
			wantRepo:   "acme/myrepo",
			wantNumber: 42,
			wantBase:   "https://api.github.com",
		},
		{
			// GitHub Enterprise host → API base derived from URL
			input:      "https://github.example.com/acme/myrepo/pull/7",
			wantRepo:   "acme/myrepo",
			wantNumber: 7,
			wantBase:   "https://github.example.com/api/v3",
		},
		{input: "https://github.com/acme/myrepo/pulls/42", wantErr: true},
		{input: "https://github.com/acme/pull/42", wantErr: true},
		{input: "not-a-url", wantErr: true},
		{input: "https://github.com/acme/myrepo/pull/0", wantErr: true},
	}
	for _, tt := range tests {
		repo, n, base, err := parsePRURL(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePRURL(%q): expected error, got repo=%q n=%d", tt.input, repo, n)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePRURL(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if repo != tt.wantRepo || n != tt.wantNumber || base != tt.wantBase {
			t.Errorf("parsePRURL(%q) = (%q, %d, %q), want (%q, %d, %q)",
				tt.input, repo, n, base, tt.wantRepo, tt.wantNumber, tt.wantBase)
		}
	}
}

func TestExtractBodyBackportPRNumbers(t *testing.T) {
	body := strings.Join([]string{
		"Some context",
		"Backport #12345 to branch/v18",
		"Backport #67890 to branch/v18.1",
		"Backport #12345 to branch/v18",
		"Backport: https://github.com/gravitational/teleport/pull/22222",
		"Backport: #33333",
	}, "\n")

	got := extractBodyBackportPRNumbers(body)
	want := []int{12345, 67890, 22222, 33333}

	if len(got) != len(want) {
		t.Fatalf("unexpected count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected numbers: got %v want %v", got, want)
		}
	}
}

func TestExtractCommitPRNumbers(t *testing.T) {
	commits := []pullCommit{
		{Commit: struct{ Message string }{Message: "[Feature] Add thing (#11111)"}},
		{Commit: struct{ Message string }{Message: "Fix test"}},
		{Commit: struct{ Message string }{Message: "[Feature] Follow-up (#22222)\n\nextra text (#11111)"}},
	}

	got := extractCommitPRNumbers(commits, 99999)
	want := []int{11111, 22222}

	if len(got) != len(want) {
		t.Fatalf("unexpected count: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected numbers: got %v want %v", got, want)
		}
	}
}

func TestNormalizeDiffIgnoresGeneratedProtobufFiles(t *testing.T) {
	diffText := strings.Join([]string{
		"diff --git a/api/client/authservice.pb.go b/api/client/authservice.pb.go",
		"index 1111111..2222222 100644",
		"--- a/api/client/authservice.pb.go",
		"+++ b/api/client/authservice.pb.go",
		"@@ -1 +1 @@",
		"-old generated line",
		"+new generated line",
		"diff --git a/lib/auth/auth.go b/lib/auth/auth.go",
		"index 3333333..4444444 100644",
		"--- a/lib/auth/auth.go",
		"+++ b/lib/auth/auth.go",
		"@@ -1 +1 @@",
		"-return false",
		"+return true",
	}, "\n")

	normalized, err := normalizeDiff(diffText)
	if err != nil {
		t.Fatalf("normalizeDiff returned error: %v", err)
	}

	if len(normalized.Ignored) != 1 || normalized.Ignored[0] != "api/client/authservice.pb.go" {
		t.Fatalf("unexpected ignored files: %v", normalized.Ignored)
	}

	got := normalized.Files["lib/auth/auth.go"]
	want := []string{"DEL return false", "ADD return true"}
	if len(got) != len(want) {
		t.Fatalf("unexpected normalized lines: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected normalized lines: got %v want %v", got, want)
		}
	}
}

func TestBuildFileDiff(t *testing.T) {
	master := []string{"DEL old", "ADD shared", "ADD master-only"}
	backport := []string{"DEL old", "ADD shared", "ADD backport-only"}

	diff := buildFileDiff("lib/auth/auth.go", master, backport, nil)

	for _, needle := range []string{
		"diff -- lib/auth/auth.go",
		"source added, backport missing:",
		"+ master-only",
		"backport added, source missing:",
		"+ backport-only",
	} {
		if !strings.Contains(diff, needle) {
			t.Fatalf("diff %q missing %q", diff, needle)
		}
	}

	if strings.Contains(diff, "shared") {
		t.Fatalf("diff %q should not include unchanged lines", diff)
	}
}

func TestBuildReport_NoDiffs(t *testing.T) {
	report := buildReport(
		"gravitational/teleport",
		pullRequest{Number: 65574, Title: "[v18] discovery: Fix deadlock in access graph aws discovery"},
		sourceResolution{Numbers: []int{65245}, Method: "body markers"},
		map[string][]string{
			"lib/srv/discovery/access_graph_aws.go": {
				"ADD \ts.Log.InfoContext(ctx, \"Access graph AWS discovery iteration started\")",
			},
		},
		normalizedDiff{
			Files: map[string][]string{
				"lib/srv/discovery/access_graph_aws.go": {
					"ADD \ts.Log.InfoContext(ctx, \"Access graph AWS discovery iteration started\")",
				},
			},
		},
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
	if !strings.Contains(report, "https://github.com/gravitational/teleport/pull/65574") {
		t.Fatalf("report missing backport PR URL: %q", report)
	}
	if !strings.Contains(report, "https://github.com/gravitational/teleport/pull/65245") {
		t.Fatalf("report missing source PR URL: %q", report)
	}
}
