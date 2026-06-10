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

func TestNormalizeDiffCapturesRenamesSeparately(t *testing.T) {
	diffText := strings.Join([]string{
		"diff --git a/lib/old.go b/lib/new.go",
		"similarity index 100%",
		"rename from lib/old.go",
		"rename to lib/new.go",
	}, "\n")

	normalized, err := normalizeDiff(diffText)
	if err != nil {
		t.Fatalf("normalizeDiff returned error: %v", err)
	}

	if len(normalized.Files) != 0 {
		t.Fatalf("rename-only diff should not produce file lines: %v", normalized.Files)
	}
	meta := normalized.Meta["lib/new.go"]
	if meta.RenameFrom != "lib/old.go" {
		t.Fatalf("unexpected rename metadata: %+v", meta)
	}
}

func TestNormalizeDiffCapturesCopyAndModeMetadata(t *testing.T) {
	diffText := strings.Join([]string{
		"diff --git a/lib/original.go b/lib/copied.go",
		"similarity index 100%",
		"copy from lib/original.go",
		"copy to lib/copied.go",
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"old mode 100644",
		"new mode 100755",
	}, "\n")

	normalized, err := normalizeDiff(diffText)
	if err != nil {
		t.Fatalf("normalizeDiff returned error: %v", err)
	}

	copyMeta := normalized.Meta["lib/copied.go"]
	if copyMeta.CopyFrom != "lib/original.go" {
		t.Fatalf("unexpected copy metadata: %+v", copyMeta)
	}

	modeMeta := normalized.Meta["bin/tool.sh"]
	if modeMeta.OldMode != "100644" || modeMeta.NewMode != "100755" {
		t.Fatalf("unexpected mode metadata: %+v", modeMeta)
	}
}

func TestApplyNormalizedDiffMovesRenamedSourceState(t *testing.T) {
	addedDiff := strings.Join([]string{
		"diff --git a/lib/old.go b/lib/old.go",
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/lib/old.go",
		"@@ -0,0 +1,2 @@",
		"+package lib",
		"+const answer = 42",
	}, "\n")
	renameDiff := strings.Join([]string{
		"diff --git a/lib/old.go b/lib/new.go",
		"similarity index 100%",
		"rename from lib/old.go",
		"rename to lib/new.go",
	}, "\n")
	backportDiff := strings.Join([]string{
		"diff --git a/lib/new.go b/lib/new.go",
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/lib/new.go",
		"@@ -0,0 +1,2 @@",
		"+package lib",
		"+const answer = 42",
	}, "\n")

	masterFiles := make(map[string][]string)
	masterMeta := make(map[string]fileMetadata)
	added, err := normalizeDiff(addedDiff)
	if err != nil {
		t.Fatalf("normalizeDiff add returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, added)

	renamed, err := normalizeDiff(renameDiff)
	if err != nil {
		t.Fatalf("normalizeDiff rename returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, renamed)

	if _, ok := masterFiles["lib/old.go"]; ok {
		t.Fatalf("old path still present after rename: %v", masterFiles)
	}
	if _, ok := masterMeta["lib/old.go"]; ok {
		t.Fatalf("old metadata path still present after rename: %v", masterMeta)
	}

	backportNormalized, err := normalizeDiff(backportDiff)
	if err != nil {
		t.Fatalf("normalizeDiff backport returned error: %v", err)
	}

	report := buildReport(
		"acme/myrepo",
		pullRequest{Number: 200, Title: "Rename chain"},
		sourceResolution{Numbers: []int{100, 101}, Method: "body markers"},
		masterFiles,
		masterMeta,
		backportNormalized,
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
}

func TestBuildReport_NoDiffsForPureRename(t *testing.T) {
	diffText := strings.Join([]string{
		"diff --git a/lib/old.go b/lib/new.go",
		"similarity index 100%",
		"rename from lib/old.go",
		"rename to lib/new.go",
	}, "\n")

	normalized, err := normalizeDiff(diffText)
	if err != nil {
		t.Fatalf("normalizeDiff returned error: %v", err)
	}

	masterFiles := make(map[string][]string)
	masterMeta := make(map[string]fileMetadata)
	applyNormalizedDiff(masterFiles, masterMeta, normalized)

	report := buildReport(
		"acme/myrepo",
		pullRequest{Number: 200, Title: "Pure rename"},
		sourceResolution{Numbers: []int{100}, Method: "body markers"},
		masterFiles,
		masterMeta,
		normalized,
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
}

func TestBuildReport_NoDiffsForPureCopy(t *testing.T) {
	diffText := strings.Join([]string{
		"diff --git a/lib/original.go b/lib/copied.go",
		"similarity index 100%",
		"copy from lib/original.go",
		"copy to lib/copied.go",
	}, "\n")

	normalized, err := normalizeDiff(diffText)
	if err != nil {
		t.Fatalf("normalizeDiff returned error: %v", err)
	}

	masterFiles := make(map[string][]string)
	masterMeta := make(map[string]fileMetadata)
	applyNormalizedDiff(masterFiles, masterMeta, normalized)

	report := buildReport(
		"acme/myrepo",
		pullRequest{Number: 200, Title: "Pure copy"},
		sourceResolution{Numbers: []int{100}, Method: "body markers"},
		masterFiles,
		masterMeta,
		normalized,
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
}

func TestApplyNormalizedDiffComposesModeChanges(t *testing.T) {
	modeDiffOne := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"old mode 100644",
		"new mode 100755",
	}, "\n")
	modeDiffTwo := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"old mode 100755",
		"new mode 100700",
	}, "\n")
	backportDiff := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"old mode 100644",
		"new mode 100700",
	}, "\n")

	masterFiles := make(map[string][]string)
	masterMeta := make(map[string]fileMetadata)

	first, err := normalizeDiff(modeDiffOne)
	if err != nil {
		t.Fatalf("normalizeDiff modeDiffOne returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, first)

	second, err := normalizeDiff(modeDiffTwo)
	if err != nil {
		t.Fatalf("normalizeDiff modeDiffTwo returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, second)

	backportNormalized, err := normalizeDiff(backportDiff)
	if err != nil {
		t.Fatalf("normalizeDiff backportDiff returned error: %v", err)
	}

	report := buildReport(
		"acme/myrepo",
		pullRequest{Number: 200, Title: "Mode change"},
		sourceResolution{Numbers: []int{100, 101}, Method: "body markers"},
		masterFiles,
		masterMeta,
		backportNormalized,
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
}

func TestApplyNormalizedDiffIgnoresModeAfterNewFile(t *testing.T) {
	addedDiff := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/bin/tool.sh",
		"@@ -0,0 +1 @@",
		"+echo hello",
	}, "\n")
	modeDiff := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"old mode 100644",
		"new mode 100755",
	}, "\n")
	backportDiff := strings.Join([]string{
		"diff --git a/bin/tool.sh b/bin/tool.sh",
		"new file mode 100755",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/bin/tool.sh",
		"@@ -0,0 +1 @@",
		"+echo hello",
	}, "\n")

	masterFiles := make(map[string][]string)
	masterMeta := make(map[string]fileMetadata)

	added, err := normalizeDiff(addedDiff)
	if err != nil {
		t.Fatalf("normalizeDiff addedDiff returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, added)

	modeOnly, err := normalizeDiff(modeDiff)
	if err != nil {
		t.Fatalf("normalizeDiff modeDiff returned error: %v", err)
	}
	applyNormalizedDiff(masterFiles, masterMeta, modeOnly)

	backportNormalized, err := normalizeDiff(backportDiff)
	if err != nil {
		t.Fatalf("normalizeDiff backportDiff returned error: %v", err)
	}

	report := buildReport(
		"acme/myrepo",
		pullRequest{Number: 200, Title: "New file with chmod"},
		sourceResolution{Numbers: []int{100, 101}, Method: "body markers"},
		masterFiles,
		masterMeta,
		backportNormalized,
		nil,
	)

	if !strings.Contains(report, "No differences after filtering protobuf-generated files.") {
		t.Fatalf("unexpected report: %q", report)
	}
}

func TestBuildFileDiff(t *testing.T) {
	master := []string{"DEL old", "ADD shared", "ADD master-only"}
	backport := []string{"DEL old", "ADD shared", "ADD backport-only"}

	diff := buildFileDiff("lib/auth/auth.go", master, fileMetadata{}, backport, fileMetadata{}, nil)

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
		nil,
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
