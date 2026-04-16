# backportdiffbot

A command-line tool that validates backport pull requests by comparing their diffs against the original source PRs.


## Features

- **Automatic source PR detection**: Extracts source PR numbers from backport PR body or commit messages
- **Diff normalization**: Filters out:
  - License boilerplate (Apache 2.0, AGPL 3.0 headers)
  - Protobuf-generated files (`.pb.go`, `.pb.ts`, etc.)
  - Reindentation and whitespace-only changes
  - Trivial word-only changes in comments
- **Clear diff reporting**: Shows exactly what was added or removed in the backport vs. source
- **Multi-source support**: Handles backports that combine multiple source PRs

## Usage

### Basic usage

```bash
export GITHUB_TOKEN=$(gh auth token)
./backportdiffbot -pr 12345
```

### Command-line options

```bash
./backportdiffbot [options]

Options:
  -pr int
      Backport pull request number (required)
  -repo string
      GitHub repository in owner/repo format (default "gravitational/teleport")
  -api-base string
      GitHub API base URL (default "https://api.github.com")
  -timeout duration
      HTTP timeout (default 30s)
  -fail-on-diff
      Exit with code 3 when differences are found (useful for CI)
```

### Examples

**Check a backport PR for a specific repository:**
```bash
./backportdiffbot -repo myorg/myrepo -pr 456
```

**Use with GitHub Enterprise:**
```bash
./backportdiffbot -api-base https://github.enterprise.com/api/v3 -pr 789
```

**Fail CI build if backport has differences:**
```bash
./backportdiffbot -pr 456 -fail-on-diff
```

## Authentication

The tool requires a GitHub token for API access. Set one of these environment variables:

- `GITHUB_TOKEN`
- `GH_TOKEN`

The token needs read access to the repository's pull requests.

## How it works

1. **Fetch the backport PR** and parse it to find source PR references
2. **Resolve source PRs** using one of these methods:
   - Body markers: `Backport #123 to branch/...` or `Backport: #123`
   - Commit messages: `Fix something (#123)` patterns in cherry-picked commits
3. **Fetch and normalize diffs** from all source PRs and the backport PR
4. **Compare normalized diffs** and report discrepancies
5. **Generate report** showing:
   - PR URLs and metadata
   - Files with differences
   - Added/removed lines that shouldn't be there

## Output format

Example output when backport differs from source:

```
Backport PR:  https://github.com/acme/myrepo/pull/200 -- Fix authentication bug
Source PRs:   https://github.com/acme/myrepo/pull/100 (resolved via body markers)

diff -- lib/auth/server.go
  source removed, backport kept:
	- if s.legacyMode {
	- return s.startLegacy()
	- }
```

When the backport matches perfectly:

```
Backport PR:  https://github.com/acme/myrepo/pull/200 -- Fix authentication bug
Source PRs:   https://github.com/acme/myrepo/pull/100 (resolved via body markers)

No differences found.
```


## Limitations

- Maximum 250 commits per PR (GitHub API limit)
- Requires source PRs to be explicitly referenced in the backport PR body or commit messages

