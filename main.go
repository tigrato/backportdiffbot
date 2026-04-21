package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/google/go-github/v68/github"
)

const (
	defaultAPIBase = "https://api.github.com"
	maxPRCommits   = 250
)

var (
	bodyBackportRes = []*regexp.Regexp{
		regexp.MustCompile(`(?mi)\bBackport\s+#(\d+)\s+to\s+branch/[^\s]+`),
		regexp.MustCompile(`(?mi)^\s*Backport(?:\s+of)?\s*:\s*(?:https://github\.com/[^/\s]+/[^/\s]+/pull/|#)(\d+)\b`),
	}
	commitPRRE = regexp.MustCompile(`\(#(\d+)\)`)
	wordRE     = regexp.MustCompile(`\w+`)

	// licenseBoilerplatePrefixes lists comment prefixes found in standard
	// Apache 2.0 and AGPL 3.0 file headers. Lines matching these are stripped
	// from diffs because they never describe a semantic change.
	licenseBoilerplatePrefixes = []string{
		"// Copyright",
		"// Licensed under the Apache",
		"// See the License for the specific language",
		"// Unless required by applicable law",
		"// WITHOUT WARRANTIES",
		"// distributed under the License",
		"// limitations under the License",
		"// you may not use this file except",
		"// You may obtain a copy of the License",
		"//      http://www.apache.org/licenses/",
		"// (at your option) any later version",
		"// GNU Affero General Public License",
		"// GNU General Public License",
		"// This program is free software",
		"// This program is distributed in the hope",
		"// it under the terms of the GNU",
		"// the Free Software Foundation",
		"// but WITHOUT ANY WARRANTY",
		"// You should have received a copy",
		"// along with this program",
		"// MERCHANTABILITY or FITNESS",
	}

	defaultIgnoreRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(^|/)api/gen/proto/`),
		regexp.MustCompile(`(^|/)gen/proto/`),
		regexp.MustCompile(`(^|/)[^/]+\.pb\.go$`),
		regexp.MustCompile(`(^|/)[^/]+\.pb\.gw\.go$`),
		regexp.MustCompile(`(^|/)[^/]+_pb\.ts$`),
		regexp.MustCompile(`(^|/)[^/]+\.pb\.ts$`),
	}
)

type config struct {
	repo       string
	backportPR int
	apiBase    string
	timeout    time.Duration
	failOnDiff bool
}

type githubClient struct {
	gh    *github.Client
	owner string
	repo  string
}

type pullRequest struct {
	Number int
	Title  string
	Body   string
}

type pullCommit struct {
	Commit struct {
		Message string
	}
}

type sourceResolution struct {
	Numbers []int
	Method  string
}

type normalizedDiff struct {
	Files    map[string][]string
	LineNums map[string]map[string]int // file -> "ADD content"/"DEL content" -> first line number
	Ignored  []string
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	client, err := newGitHubClient(cfg.repo, cfg.apiBase, cfg.timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	backportPR, err := client.fetchPR(ctx, cfg.backportPR)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	resolution, err := resolveSourcePRs(ctx, client, backportPR)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	masterFiles := make(map[string][]string)
	masterIgnored := make(map[string]struct{})
	for _, sourcePR := range resolution.Numbers {
		diffText, err := client.fetchPRDiff(ctx, sourcePR)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fetch source PR #%d diff: %v\n", sourcePR, err)
			os.Exit(1)
		}

		normalized, err := normalizeDiff(diffText)
		if err != nil {
			fmt.Fprintf(os.Stderr, "normalize source PR #%d diff: %v\n", sourcePR, err)
			os.Exit(1)
		}

		mergeFileMaps(masterFiles, normalized.Files)
		addIgnored(masterIgnored, normalized.Ignored)
	}

	backportDiffText, err := client.fetchPRDiff(ctx, backportPR.Number)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch backport PR #%d diff: %v\n", backportPR.Number, err)
		os.Exit(1)
	}

	backportNormalized, err := normalizeDiff(backportDiffText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "normalize backport PR #%d diff: %v\n", backportPR.Number, err)
		os.Exit(1)
	}

	report := buildReport(cfg.repo, backportPR, resolution, masterFiles, backportNormalized, masterIgnored)
	fmt.Print(report)

	if cfg.failOnDiff && strings.Contains(report, "\ndiff -- ") {
		os.Exit(3)
	}
}

func parseConfig() (config, error) {
	cfg := config{
		repo:    "gravitational/teleport",
		apiBase: defaultAPIBase,
		timeout: 30 * time.Second,
	}

	flag.StringVar(&cfg.repo, "repo", cfg.repo, "GitHub repository in owner/repo form")
	flag.IntVar(&cfg.backportPR, "pr", 0, "Backport pull request number")
	flag.StringVar(&cfg.apiBase, "api-base", cfg.apiBase, "GitHub API base URL")
	flag.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "HTTP timeout")
	flag.BoolVar(&cfg.failOnDiff, "fail-on-diff", false, "Exit non-zero when differences are found")
	flag.Parse()

	// A bare URL argument (e.g. https://github.com/org/repo/pull/42) can be
	// used instead of -repo/-pr.  It also auto-sets -api-base for GHE hosts.
	if args := flag.Args(); len(args) == 1 {
		repo, n, base, err := parsePRURL(args[0])
		if err != nil {
			return cfg, err
		}
		cfg.repo = repo
		cfg.backportPR = n
		if cfg.apiBase == defaultAPIBase {
			cfg.apiBase = base
		}
	}

	if cfg.backportPR <= 0 {
		return cfg, errors.New("usage: backportdiffbot -pr N [-repo owner/repo] or backportdiffbot <PR URL>")
	}
	if _, err := url.ParseRequestURI(cfg.apiBase); err != nil {
		return cfg, fmt.Errorf("invalid -api-base %q: %w", cfg.apiBase, err)
	}
	return cfg, nil
}

func newGitHubClient(repo, apiBase string, timeout time.Duration) (*githubClient, error) {
	owner, repoName, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	token := firstNonEmpty(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN"))
	gh := github.NewClient(&http.Client{Timeout: timeout}).WithAuthToken(token)
	if apiBase != defaultAPIBase {
		base := strings.TrimRight(apiBase, "/") + "/"
		if gh, err = gh.WithEnterpriseURLs(base, base); err != nil {
			return nil, fmt.Errorf("invalid -api-base %q: %w", apiBase, err)
		}
	}
	return &githubClient{gh: gh, owner: owner, repo: repoName}, nil
}

func resolveSourcePRs(ctx context.Context, client *githubClient, pr pullRequest) (sourceResolution, error) {
	if numbers := extractBodyBackportPRNumbers(pr.Body); len(numbers) > 0 {
		return sourceResolution{
			Numbers: numbers,
			Method:  "body markers",
		}, nil
	}

	commits, err := client.fetchPRCommits(ctx, pr.Number)
	if err != nil {
		return sourceResolution{}, fmt.Errorf("no body markers found in backport PR #%d and commit fallback failed: %w", pr.Number, err)
	}

	if numbers := extractCommitPRNumbers(commits, pr.Number); len(numbers) > 0 {
		return sourceResolution{
			Numbers: numbers,
			Method:  "commit messages",
		}, nil
	}

	return sourceResolution{}, fmt.Errorf("found no source PR numbers in backport PR #%d body or commit messages", pr.Number)
}

func extractBodyBackportPRNumbers(body string) []int {
	seen := make(map[int]struct{})
	var numbers []int
	for _, re := range bodyBackportRes {
		matches := re.FindAllStringSubmatch(body, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			number, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			if _, ok := seen[number]; ok {
				continue
			}
			seen[number] = struct{}{}
			numbers = append(numbers, number)
		}
	}
	return numbers
}

func extractCommitPRNumbers(commits []pullCommit, backportPR int) []int {
	seen := make(map[int]struct{})
	var numbers []int
	for _, commit := range commits {
		matches := commitPRRE.FindAllStringSubmatch(commit.Commit.Message, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			number, err := strconv.Atoi(match[1])
			if err != nil {
				continue
			}
			if number == backportPR {
				continue
			}
			if _, ok := seen[number]; ok {
				continue
			}
			seen[number] = struct{}{}
			numbers = append(numbers, number)
		}
	}
	return numbers
}

func normalizeDiff(diffText string) (normalizedDiff, error) {
	parsed, _, err := gitdiff.Parse(strings.NewReader(diffText))
	if err != nil {
		return normalizedDiff{}, fmt.Errorf("parse diff: %w", err)
	}

	files := make(map[string][]string)
	lineNums := make(map[string]map[string]int)
	var ignoredPaths []string

	for _, file := range parsed {
		oldPath := file.OldName
		newPath := file.NewName

		if shouldIgnorePath(oldPath) || shouldIgnorePath(newPath) {
			if path := chooseDisplayPath(oldPath, newPath); path != "" {
				ignoredPaths = append(ignoredPaths, path)
			}
			continue
		}

		displayPath := chooseDisplayPath(oldPath, newPath)
		if displayPath == "" {
			continue
		}

		var lines []string
		switch {
		case file.IsNew:
			lines = append(lines, "META new file")
		case file.IsDelete:
			lines = append(lines, "META deleted file")
		}
		if file.IsRename {
			lines = append(lines, "META rename from "+oldPath, "META rename to "+newPath)
		}
		if file.IsBinary {
			lines = append(lines, "META binary file")
		}

		fileLineNums := make(map[string]int)
		for _, frag := range file.TextFragments {
			// oldLine and newLine track the 1-based position as we walk the hunk.
			oldLine := int(frag.OldPosition)
			newLine := int(frag.NewPosition)

			for _, line := range frag.Lines {
				// Strip leading whitespace so re-indented lines (e.g. code
				// wrapped in a synctest closure) compare as identical.
				content := strings.TrimLeft(strings.TrimRight(line.Line, "\n"), " \t")
				skip := content == "" || isBoilerplateLine(content)

				switch line.Op {
				case gitdiff.OpAdd:
					if !skip {
						key := "ADD " + content
						lines = append(lines, key)
						if fileLineNums[key] == 0 { // keep first occurrence
							fileLineNums[key] = newLine
						}
					}
					newLine++
				case gitdiff.OpDelete:
					if !skip {
						key := "DEL " + content
						lines = append(lines, key)
						if fileLineNums[key] == 0 {
							fileLineNums[key] = oldLine
						}
					}
					oldLine++
				case gitdiff.OpContext:
					oldLine++
					newLine++
				}
			}
		}

		if len(lines) > 0 {
			files[displayPath] = append(files[displayPath], lines...)
			if len(fileLineNums) > 0 {
				lineNums[displayPath] = fileLineNums
			}
		}
	}

	sort.Strings(ignoredPaths)
	return normalizedDiff{Files: files, LineNums: lineNums, Ignored: ignoredPaths}, nil
}

// isBoilerplateLine reports whether content is a bare comment marker or a
// line from a standard Apache 2.0 / AGPL 3.0 file header.
func isBoilerplateLine(content string) bool {
	// Exact-match single-line boilerplate markers.
	switch content {
	case "//", "// Teleport":
		return true
	}
	for _, prefix := range licenseBoilerplatePrefixes {
		if strings.HasPrefix(content, prefix) {
			return true
		}
	}
	return false
}

func buildReport(repo string, backportPR pullRequest, resolution sourceResolution, masterFiles map[string][]string, backportNormalized normalizedDiff, masterIgnored map[string]struct{}) string {
	var builder strings.Builder

	sourcePRURLs := make([]string, len(resolution.Numbers))
	for i, n := range resolution.Numbers {
		sourcePRURLs[i] = fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
	}
	fmt.Fprintf(&builder, "Backport PR:  https://github.com/%s/pull/%d -- %s\n", repo, backportPR.Number, backportPR.Title)
	fmt.Fprintf(&builder, "Source PRs:   %s (resolved via %s)\n", strings.Join(sourcePRURLs, ", "), resolution.Method)

	allIgnored := make(map[string]struct{}, len(masterIgnored)+len(backportNormalized.Ignored))
	for k := range masterIgnored {
		allIgnored[k] = struct{}{}
	}
	for _, k := range backportNormalized.Ignored {
		allIgnored[k] = struct{}{}
	}
	if len(allIgnored) > 0 {
		fmt.Fprintf(&builder, "Ignored:      %d protobuf-generated file(s)\n", len(allIgnored))
	}
	builder.WriteString("\n")

	paths := unionPaths(masterFiles, backportNormalized.Files)
	var diffs []string
	for _, path := range paths {
		diff := buildFileDiff(path, masterFiles[path], backportNormalized.Files[path], backportNormalized.LineNums[path])
		if diff != "" {
			diffs = append(diffs, diff)
		}
	}

	if len(diffs) == 0 {
		builder.WriteString("No differences after filtering protobuf-generated files.\n")
		return builder.String()
	}

	for idx, diff := range diffs {
		builder.WriteString(diff)
		if idx < len(diffs)-1 {
			builder.WriteString("\n")
		}
	}

	return builder.String()
}

// buildFileDiff compares the net changes made by the source PRs (masterLines)
// against the backport PR (backportLines) for a single file and returns a
// human-readable diff, or "" when the changes are equivalent.
// backportLineNums maps "ADD content" / "DEL content" keys to the first line
// number where that change appears in the backport PR's diff.
func buildFileDiff(path string, masterLines, backportLines []string, backportLineNums map[string]int) string {
	// Reduce each side by cancelling ADD/DEL pairs for identical content.
	masterLines = reduceLines(masterLines)
	backportLines = reduceLines(backportLines)

	masterOnly, backportOnly := lineDiff(masterLines, backportLines)
	if len(masterOnly) == 0 && len(backportOnly) == 0 {
		return ""
	}

	// Word-level semantic equivalence check (e.g. list reordering).
	if slicesEqual(metaLines(masterLines), metaLines(backportLines)) {
		mAdded, mDeleted := wordDelta(masterLines)
		bAdded, bDeleted := wordDelta(backportLines)
		if slicesEqual(mAdded, bAdded) && slicesEqual(mDeleted, bDeleted) {
			return ""
		}
	}

	// Split each side into added vs removed for cleaner section labels.
	var srcAdd, srcDel, bpAdd, bpDel []string
	for _, line := range masterOnly {
		switch {
		case strings.HasPrefix(line, "ADD "):
			srcAdd = append(srcAdd, line[4:])
		case strings.HasPrefix(line, "DEL "):
			srcDel = append(srcDel, line[4:])
		}
	}
	for _, line := range backportOnly {
		switch {
		case strings.HasPrefix(line, "ADD "):
			bpAdd = append(bpAdd, line[4:])
		case strings.HasPrefix(line, "DEL "):
			bpDel = append(bpDel, line[4:])
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "diff -- %s\n", path)

	// writeSection renders one category of differences.
	// nums is the line-number lookup map; pass nil to omit line annotations.
	writeSection := func(header string, prefix byte, lines []string, nums map[string]int, lookupPrefix string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString(header)
		for _, content := range lines {
			b.WriteByte('\t')
			b.WriteByte(prefix)
			b.WriteByte(' ')
			b.WriteString(content)
			if nums != nil {
				if ln := nums[lookupPrefix+content]; ln > 0 {
					fmt.Fprintf(&b, "\t(backport line %d)", ln)
				}
			}
			b.WriteByte('\n')
		}
	}

	// Source-only sections: lines missing from or still in the backport.
	// No backport line number is available (the line isn't in the backport diff).
	writeSection("  source added, backport missing:\n", '+', srcAdd, nil, "")
	writeSection("  source removed, backport kept:\n", '-', srcDel, nil, "")
	// Backport-only sections: annotate with the line number in the backport diff.
	writeSection("  backport added, source missing:\n", '+', bpAdd, backportLineNums, "ADD ")
	writeSection("  backport removed, source kept:\n", '-', bpDel, backportLineNums, "DEL ")

	return b.String()
}

// reduceLines cancels ADD/DEL pairs that carry the same content within a
// single side's line list.  This eliminates intermediate states introduced
// when multiple source PRs touch the same file (e.g. PR1 adds line X, PR2
// later removes it - after reduction both entries disappear).
// META lines are passed through unchanged.
func reduceLines(lines []string) []string {
	addCounts := make(map[string]int)
	delCounts := make(map[string]int)
	var meta []string

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "ADD "):
			addCounts[line[4:]]++
		case strings.HasPrefix(line, "DEL "):
			delCounts[line[4:]]++
		case strings.HasPrefix(line, "META "):
			meta = append(meta, line)
		}
	}

	out := append([]string(nil), meta...)
	for content, n := range addCounts {
		for range max(0, n-delCounts[content]) {
			out = append(out, "ADD "+content)
		}
	}
	for content, n := range delCounts {
		for range max(0, n-addCounts[content]) {
			out = append(out, "DEL "+content)
		}
	}
	return out
}

// lineDiff computes multiset set-differences between two line slices.
// Lines present in a but not b are returned as aOnly (sorted).
// Lines present in b but not a are returned as bOnly (sorted).
// This is position-independent: identical lines at different positions cancel.
func lineDiff(a, b []string) (aOnly, bOnly []string) {
	aCounts := make(map[string]int, len(a))
	bCounts := make(map[string]int, len(b))
	for _, l := range a {
		aCounts[l]++
	}
	for _, l := range b {
		bCounts[l]++
	}
	for l, n := range aCounts {
		for range max(0, n-bCounts[l]) {
			aOnly = append(aOnly, l)
		}
	}
	for l, n := range bCounts {
		for range max(0, n-aCounts[l]) {
			bOnly = append(bOnly, l)
		}
	}
	sort.Strings(aOnly)
	sort.Strings(bOnly)
	return
}

// wordDelta computes the multi-set difference between ADD words and DEL words
// across all ADD/DEL lines.  The returned slices are sorted.
// netAdded  = words present in ADD lines more often than in DEL lines.
// netDeleted = words present in DEL lines more often than in ADD lines.
func wordDelta(lines []string) (netAdded, netDeleted []string) {
	addCounts := make(map[string]int)
	delCounts := make(map[string]int)
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "ADD "):
			for _, w := range wordRE.FindAllString(line[4:], -1) {
				addCounts[w]++
			}
		case strings.HasPrefix(line, "DEL "):
			for _, w := range wordRE.FindAllString(line[4:], -1) {
				delCounts[w]++
			}
		}
	}
	for w, n := range addCounts {
		for range max(0, n-delCounts[w]) {
			netAdded = append(netAdded, w)
		}
	}
	for w, n := range delCounts {
		for range max(0, n-addCounts[w]) {
			netDeleted = append(netDeleted, w)
		}
	}
	sort.Strings(netAdded)
	sort.Strings(netDeleted)
	return
}

func metaLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, "META ") {
			out = append(out, line)
		}
	}
	return out
}

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *githubClient) fetchPR(ctx context.Context, number int) (pullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repo, number)
	if err != nil {
		return pullRequest{}, fmt.Errorf("fetch PR #%d: %w", number, err)
	}
	return pullRequest{
		Number: pr.GetNumber(),
		Title:  pr.GetTitle(),
		Body:   pr.GetBody(),
	}, nil
}

func (c *githubClient) fetchPRCommits(ctx context.Context, number int) ([]pullCommit, error) {
	opts := &github.ListOptions{PerPage: 100}
	var all []pullCommit
	for {
		batch, resp, err := c.gh.PullRequests.ListCommits(ctx, c.owner, c.repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("fetch PR #%d commits: %w", number, err)
		}
		for _, rc := range batch {
			all = append(all, pullCommit{Commit: struct{ Message string }{Message: rc.Commit.GetMessage()}})
		}
		if resp.NextPage == 0 || len(all) >= maxPRCommits {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func (c *githubClient) fetchPRDiff(ctx context.Context, number int) (string, error) {
	diff, _, err := c.gh.PullRequests.GetRaw(ctx, c.owner, c.repo, number, github.RawOptions{Type: github.Diff})
	if err != nil {
		return "", fmt.Errorf("fetch PR #%d diff: %w", number, err)
	}
	return diff, nil
}

func shouldIgnorePath(path string) bool {
	if path == "" {
		return false
	}
	for _, re := range defaultIgnoreRegexes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

func chooseDisplayPath(oldPath, newPath string) string {
	switch {
	case newPath != "" && newPath != "/dev/null":
		return newPath
	default:
		return oldPath
	}
}

func mergeFileMaps(dst, src map[string][]string) {
	for path, lines := range src {
		dst[path] = append(dst[path], lines...)
	}
}

func addIgnored(dst map[string]struct{}, ignored []string) {
	for _, path := range ignored {
		dst[path] = struct{}{}
	}
}

func unionPaths(a, b map[string][]string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for path := range a {
		seen[path] = struct{}{}
	}
	for path := range b {
		seen[path] = struct{}{}
	}
	return mapKeysSorted(seen)
}

func mapKeysSorted[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func splitRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q: want owner/repo", repo)
	}
	return parts[0], parts[1], nil
}

// parsePRURL parses a GitHub PR URL of the form
// https://github.com/{owner}/{repo}/pull/{number} and returns the
// owner/repo string, PR number, and API base URL (non-github.com hosts
// are assumed to be GitHub Enterprise with /api/v3 at the same root).
func parsePRURL(rawURL string) (repo string, number int, apiBase string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", 0, "", fmt.Errorf("not a GitHub PR URL: %q", rawURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", 0, "", fmt.Errorf("not a GitHub PR URL: %q", rawURL)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", 0, "", fmt.Errorf("invalid PR number in URL %q", rawURL)
	}
	repoStr := parts[0] + "/" + parts[1]
	base := defaultAPIBase
	if u.Host != "github.com" {
		base = u.Scheme + "://" + u.Host + "/api/v3"
	}
	return repoStr, n, base, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
