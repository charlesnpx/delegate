package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResolveBaseFallbackChain(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		repo := newGitFixture(t)
		got, err := ResolveBase(context.Background(), repo, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "explicit" || got.Ref != "HEAD" || got.Commit == "" {
			t.Fatalf("ResolveBase() = %#v", got)
		}
	})

	t.Run("upstream", func(t *testing.T) {
		repo := newGitFixture(t)
		upstreamHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
		gitFixture(t, repo, "remote", "add", "origin", repo)
		gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", upstreamHead)
		gitFixture(t, repo, "config", "branch.main.remote", "origin")
		gitFixture(t, repo, "config", "branch.main.merge", "refs/heads/main")
		writeFixtureFile(t, repo, "unpushed.txt", "local commit\n")
		gitFixture(t, repo, "add", "unpushed.txt")
		gitFixture(t, repo, "commit", "-m", "unpushed change")
		got, err := ResolveBase(context.Background(), repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "upstream-unpushed" || got.Ref != "origin/main" || got.Commit != upstreamHead {
			t.Fatalf("ResolveBase() = %#v, want unpushed upstream origin/main at %s", got, upstreamHead)
		}
	})

	t.Run("default remote branch", func(t *testing.T) {
		repo := newGitFixture(t)
		head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
		gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", head)
		gitFixture(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		got, err := ResolveBase(context.Background(), repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "default-remote" || got.Ref != "origin/main" || got.Commit != head {
			t.Fatalf("ResolveBase() = %#v, want default origin/main at %s", got, head)
		}
	})

	t.Run("default non-origin remote branch", func(t *testing.T) {
		repo := newGitFixture(t)
		head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
		gitFixture(t, repo, "remote", "add", "upstream", repo)
		gitFixture(t, repo, "update-ref", "refs/remotes/upstream/trunk", head)
		gitFixture(t, repo, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")
		got, err := ResolveBase(context.Background(), repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "default-remote" || got.Ref != "upstream/trunk" || got.Commit != head {
			t.Fatalf("ResolveBase() = %#v, want default upstream/trunk at %s", got, head)
		}
	})

	t.Run("guidance", func(t *testing.T) {
		repo := newGitFixture(t)
		_, err := ResolveBase(context.Background(), repo, "")
		if err == nil {
			t.Fatal("ResolveBase() error = nil")
		}
		for _, fragment := range []string{"--base <ref>", "upstream behind HEAD", "default remote branch"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("ResolveBase() error = %q, want %q", err, fragment)
			}
		}
	})
}

func TestResolveBasePushedFeaturePrefersDefaultRemoteBranch(t *testing.T) {
	repo := newGitFixture(t)
	mainHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", mainHead)
	gitFixture(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitFixture(t, repo, "switch", "-c", "feature")
	writeFixtureFile(t, repo, "feature.go", "package feature\n")
	gitFixture(t, repo, "add", "feature.go")
	gitFixture(t, repo, "commit", "-m", "feature")
	featureHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "update-ref", "refs/remotes/origin/feature", featureHead)
	gitFixture(t, repo, "config", "branch.feature.remote", "origin")
	gitFixture(t, repo, "config", "branch.feature.merge", "refs/heads/feature")

	got, err := ResolveBase(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "origin/main" || got.Source != "default-remote" || got.Commit != mainHead {
		t.Fatalf("ResolveBase() = %#v, want default origin/main at %s", got, mainHead)
	}
}

func TestVerifyHeadUnchanged(t *testing.T) {
	const captured = "1111111111111111111111111111111111111111"
	tests := []struct {
		name     string
		captured string
		current  string
		wantErr  string
	}{
		{name: "equal", captured: captured, current: captured},
		{
			name:     "different",
			captured: captured,
			current:  "2222222222222222222222222222222222222222",
			wantErr:  "repository HEAD moved during review assembly (1111111111111111111111111111111111111111 -> 2222222222222222222222222222222222222222); re-run the review on a quiescent repo",
		},
		{name: "initial fallback", captured: "HEAD", current: "2222222222222222222222222222222222222222"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyHeadUnchanged(test.captured, test.current)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyHeadUnchanged(%q, %q) error = %v", test.captured, test.current, err)
				}
				return
			}
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("verifyHeadUnchanged(%q, %q) error = %v, want %q", test.captured, test.current, err, test.wantErr)
			}
		})
	}
}

func TestAssembleFailsClosedWhenHEADMovesBetweenSnapshotReads(t *testing.T) {
	repo := newGitFixture(t)
	mainHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")
	writeFixtureFile(t, repo, "feature.txt", "feature\n")
	gitFixture(t, repo, "add", "feature.txt")
	gitFixture(t, repo, "commit", "-m", "feature")
	featureHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "checkout-triggered")
	wrapper := filepath.Join(binDir, "git")
	const wrapperScript = `#!/bin/sh
set -eu

case " $* " in
  *" rev-parse HEAD "*|*" rev-parse --abbrev-ref HEAD "*) snapshot_read=1 ;;
  *) snapshot_read=0 ;;
esac

"$DELEGATE_TEST_REAL_GIT" "$@"
if [ "$snapshot_read" -eq 1 ] && [ ! -e "$DELEGATE_TEST_CHECKOUT_MARKER" ]; then
  : > "$DELEGATE_TEST_CHECKOUT_MARKER"
  "$DELEGATE_TEST_REAL_GIT" -C "$DELEGATE_TEST_CHECKOUT_REPO" checkout --quiet main
fi
`
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELEGATE_TEST_REAL_GIT", realGit)
	t.Setenv("DELEGATE_TEST_CHECKOUT_MARKER", marker)
	t.Setenv("DELEGATE_TEST_CHECKOUT_REPO", repo)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = Assemble(context.Background(), Options{
		CWD:      repo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	want := "repository HEAD moved during review assembly (" + featureHead + " -> " + mainHead + "); re-run the review on a quiescent repo"
	if err == nil || err.Error() != want {
		t.Fatalf("Assemble() error = %v, want %q", err, want)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("checkout trigger marker: %v", err)
	}
}

func TestAssembleAutoCombinesCommittedBranchAndWorkingTreeOverlay(t *testing.T) {
	repo := newGitFixture(t)
	mainHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", mainHead)
	gitFixture(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitFixture(t, repo, "switch", "-c", "feature")
	writeFixtureFile(t, repo, "committed.go", "package feature\n// COMMITTED_BRANCH_CHANGE\n")
	gitFixture(t, repo, "add", "committed.go")
	gitFixture(t, repo, "commit", "-m", "committed feature change")
	writeFixtureFile(t, repo, "overlay.txt", "UNTRACKED_OVERLAY_CHANGE\n")

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeAuto, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if assembled.Scope != ScopeAuto || assembled.Base.Ref != "origin/main" {
		t.Fatalf("scope/base = %q %#v", assembled.Scope, assembled.Base)
	}
	for _, want := range []string{"COMMITTED_BRANCH_CHANGE", "UNTRACKED_OVERLAY_CHANGE"} {
		if !strings.Contains(assembled.Inline, want) {
			t.Fatalf("automatic context missing %q: %q", want, assembled.Inline)
		}
	}
}

func TestAssembleAutoUsesUntrackedRecreationOverBranchDeletion(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "recreated.txt", "base content\n")
	gitFixture(t, repo, "add", "recreated.txt")
	gitFixture(t, repo, "commit", "-m", "add file")
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")
	gitFixture(t, repo, "rm", "recreated.txt")
	gitFixture(t, repo, "commit", "-m", "delete file")
	writeFixtureFile(t, repo, "recreated.txt", "UNTRACKED_RECREATION_WINS\n")

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Base: base, Scope: ScopeAuto, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if !strings.Contains(assembled.Inline, "UNTRACKED_RECREATION_WINS") {
		t.Fatalf("automatic context dropped untracked recreation: %q", assembled.Inline)
	}
	if len(assembled.Files) != 1 || assembled.Files[0].Path != "recreated.txt" || assembled.Files[0].Status != "??" {
		t.Fatalf("automatic files = %#v, want untracked recreation", assembled.Files)
	}
}

func TestAssembleSanitizesTrackedAndUntrackedSecretPathsBeforeArtifact(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, ".env.production", "TRACKED_SECRET_OLD=never\n")
	writeFixtureFile(t, repo, "app.go", "package app\n")
	gitFixture(t, repo, "add", ".env.production", "app.go")
	gitFixture(t, repo, "commit", "-m", "add review files")

	writeFixtureFile(t, repo, ".env.production", "TRACKED_SECRET_NEW=never\n")
	writeFixtureFile(t, repo, "api-token.txt", "UNTRACKED_TOKEN_SECRET=never\n")
	writeFixtureFile(t, repo, "app.go", "package app\n// PUBLIC_REVIEW_CHANGE\n")
	writeFixtureFile(t, repo, "large.txt", strings.Repeat("public-context-", MaxInlineBytes/8))

	assembled, err := Assemble(context.Background(), Options{
		CWD:      repo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "delegate-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Cleanup(assembled) })
	if assembled.ArtifactPath == "" || assembled.Inline != "" {
		t.Fatalf("artifact=%q inline-bytes=%d, want spilled artifact", assembled.ArtifactPath, len(assembled.Inline))
	}
	raw, err := os.ReadFile(assembled.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	artifact := string(raw)
	for _, forbidden := range []string{"TRACKED_SECRET_OLD", "TRACKED_SECRET_NEW", "UNTRACKED_TOKEN_SECRET"} {
		if strings.Contains(artifact, forbidden) {
			t.Fatalf("sanitized artifact leaked %q", forbidden)
		}
	}
	for _, required := range []string{
		"REDACTED\tM\t\".env.production\"",
		"REDACTED\t??\t\"api-token.txt\"",
		"PUBLIC_REVIEW_CHANGE",
	} {
		if !strings.Contains(artifact, required) {
			t.Fatalf("sanitized artifact missing %q\n%s", required, artifact[:min(len(artifact), 2000)])
		}
	}
	info, err := os.Stat(assembled.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("artifact mode = %o, want 600", got)
	}
	prompt, err := ComposePrompt(KindReview, assembled)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, repo) || strings.Contains(prompt, "TRACKED_SECRET") {
		t.Fatalf("safe backend prompt exposed live repo or secret: %q", prompt)
	}
}

func TestAssembleRedactsRenameCopyWhenEitherEndpointIsSecretAndOmitsBinaryContent(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, ".env", "RENAMED_SECRET_MUST_NOT_LEAK\n")
	writeFixtureFile(t, repo, "credentials.json", "COPIED_SECRET_MUST_NOT_LEAK\n")
	if err := os.WriteFile(filepath.Join(repo, "client.p12"), []byte{'o', 'l', 'd', 0, 'b', 'i', 'n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "add", ".env", "credentials.json", "client.p12")
	gitFixture(t, repo, "commit", "-m", "secret fixtures")
	gitFixture(t, repo, "mv", ".env", "config.txt")
	raw, err := os.ReadFile(filepath.Join(repo, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "public-copy.txt"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "add", "public-copy.txt")
	if err := os.WriteFile(filepath.Join(repo, "client.p12"), []byte{'n', 'e', 'w', 0, 's', 'e', 'c', 'r', 'e', 't'}, 0o600); err != nil {
		t.Fatal(err)
	}

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	for _, want := range []string{
		"REDACTED\tR100\t\".env\" -> \"config.txt\"",
		"REDACTED\tC100\t\"credentials.json\" -> \"public-copy.txt\"",
		"REDACTED\tM\t\"client.p12\"",
	} {
		if !strings.Contains(assembled.Inline, want) {
			t.Fatalf("sanitized context missing %q: %q", want, assembled.Inline)
		}
	}
	for _, forbidden := range []string{"RENAMED_SECRET_MUST_NOT_LEAK", "COPIED_SECRET_MUST_NOT_LEAK", "GIT binary patch", "literal "} {
		if strings.Contains(assembled.Inline, forbidden) {
			t.Fatalf("sanitized context leaked binary/secret marker %q: %q", forbidden, assembled.Inline)
		}
	}
}

func TestAssembleRedactsContentThatTraversedSecretRenameChainAndCopies(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "public.txt", "public base\n")
	gitFixture(t, repo, "add", "public.txt")
	gitFixture(t, repo, "commit", "-m", "public base")
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")
	gitFixture(t, repo, "mv", "public.txt", ".ENV")
	writeFixtureFile(t, repo, ".ENV", "CHAIN_SECRET_MUST_NOT_LEAK\n")
	gitFixture(t, repo, "add", ".ENV")
	gitFixture(t, repo, "commit", "-m", "move through secret path")
	gitFixture(t, repo, "mv", ".ENV", "public.txt")
	raw, err := os.ReadFile(filepath.Join(repo, "public.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "public-copy.txt"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "add", "public.txt", "public-copy.txt")
	gitFixture(t, repo, "commit", "-m", "return to public paths")

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Base: base, Scope: ScopeBranch, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if strings.Contains(assembled.Inline, "CHAIN_SECRET_MUST_NOT_LEAK") {
		t.Fatalf("secret rename-chain content leaked: %q", assembled.Inline)
	}
	redacted := make(map[string]bool)
	for _, file := range assembled.Files {
		redacted[file.Path] = file.Redacted
	}
	for _, path := range []string{"public.txt", "public-copy.txt"} {
		if !redacted[path] || !strings.Contains(assembled.Inline, "REDACTED") || !strings.Contains(assembled.Inline, strconv.Quote(path)) {
			t.Fatalf("%s was not content-redacted: files=%#v context=%q", path, assembled.Files, assembled.Inline)
		}
	}
}

func TestAssembleRedactsPublicPathEditedAfterSecretRename(t *testing.T) {
	repo := newGitFixture(t)
	stable := strings.Repeat("stable context line\n", 80)
	writeFixtureFile(t, repo, "public.txt", stable+"base version\n")
	gitFixture(t, repo, "add", "public.txt")
	gitFixture(t, repo, "commit", "-m", "public base")
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")

	gitFixture(t, repo, "mv", "public.txt", ".ENV")
	writeFixtureFile(t, repo, ".ENV", stable+"SECRET_PATH_VERSION_MUST_NOT_LEAK\n")
	gitFixture(t, repo, "add", ".ENV")
	gitFixture(t, repo, "commit", "-m", "edit under secret name")

	gitFixture(t, repo, "mv", ".ENV", "public.txt")
	writeFixtureFile(t, repo, "public.txt", "FINAL_PUBLIC_EDIT_MUST_NOT_LEAK\ncompletely rewritten after rename\n")
	gitFixture(t, repo, "add", "public.txt")
	gitFixture(t, repo, "commit", "-m", "return and edit public path")

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Base: base, Scope: ScopeBranch, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if len(assembled.Files) != 1 || assembled.Files[0].Path != "public.txt" || !assembled.Files[0].Redacted {
		t.Fatalf("files = %#v, want tainted public.txt", assembled.Files)
	}
	for _, forbidden := range []string{"SECRET_PATH_VERSION_MUST_NOT_LEAK", "FINAL_PUBLIC_EDIT_MUST_NOT_LEAK"} {
		if strings.Contains(assembled.Inline, forbidden) {
			t.Fatalf("transitively tainted content %q leaked: %q", forbidden, assembled.Inline)
		}
	}
}

func TestAssemblePathTaintSurvivesMultipleRenameHops(t *testing.T) {
	repo := newGitFixture(t)
	stable := strings.Repeat("multi-hop stable line\n", 80)
	writeFixtureFile(t, repo, "origin.txt", stable+"origin\n")
	gitFixture(t, repo, "add", "origin.txt")
	gitFixture(t, repo, "commit", "-m", "origin")
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")

	hops := []struct {
		from, to, marker string
	}{
		{from: "origin.txt", to: ".ENV", marker: "secret-hop"},
		{from: ".ENV", to: "middle.txt", marker: "middle-hop"},
		{from: "middle.txt", to: "final.txt", marker: "FINAL_MULTI_HOP_MUST_NOT_LEAK"},
	}
	for _, hop := range hops {
		gitFixture(t, repo, "mv", hop.from, hop.to)
		writeFixtureFile(t, repo, hop.to, stable+hop.marker+"\n")
		gitFixture(t, repo, "add", hop.to)
		gitFixture(t, repo, "commit", "-m", hop.marker)
	}

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Base: base, Scope: ScopeBranch, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	redacted := make(map[string]bool)
	for _, file := range assembled.Files {
		redacted[file.Path] = file.Redacted
	}
	if !redacted["final.txt"] || strings.Contains(assembled.Inline, "FINAL_MULTI_HOP_MUST_NOT_LEAK") {
		t.Fatalf("multi-hop taint was lost: files=%#v context=%q", assembled.Files, assembled.Inline)
	}
}

func TestAssembleRedactsNormalizedSecretPathVariants(t *testing.T) {
	paths := []string{"SERVICE_ACCOUNT.JSON", "service_account.json", ".kube/config", "KUBE_CONFIG"}
	for _, path := range paths {
		t.Run(strings.NewReplacer("/", "_", ".", "_").Replace(path), func(t *testing.T) {
			repo := newGitFixture(t)
			writeFixtureFile(t, repo, path, "NORMALIZED_SECRET_MUST_NOT_LEAK\n")
			assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
			if err != nil {
				t.Fatal(err)
			}
			defer Cleanup(assembled)
			if !strings.Contains(assembled.Inline, "REDACTED\t??\t"+strconv.Quote(path)) {
				t.Fatalf("normalized secret path %q was not redacted: %q", path, assembled.Inline)
			}
			if strings.Contains(assembled.Inline, "NORMALIZED_SECRET_MUST_NOT_LEAK") {
				t.Fatalf("normalized secret content leaked: %q", assembled.Inline)
			}
		})
	}
}

func TestAssembleDoesNotExecuteCleanFilter(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, ".gitattributes", "filtered.txt filter=tripwire\n")
	writeFixtureFile(t, repo, "filtered.txt", "before\n")
	gitFixture(t, repo, "add", ".gitattributes", "filtered.txt")
	gitFixture(t, repo, "commit", "-m", "attribute fixture")
	marker := filepath.Join(t.TempDir(), "clean-filter-ran")
	script := filepath.Join(t.TempDir(), "clean-filter.sh")
	scriptBody := "#!/bin/sh\nprintf invoked > '" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'\ncat\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "config", "filter.tripwire.clean", script)
	writeFixtureFile(t, repo, "filtered.txt", "after\n")

	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean filter executed during assembly: %v", err)
	}
	if !strings.Contains(assembled.Inline, "after") {
		t.Fatalf("assembled context missing unfiltered worktree content: %q", assembled.Inline)
	}
}

func TestAssembleInlineSanitizedContextAndThresholdBoundaries(t *testing.T) {
	if !ShouldInline(MaxInlineFiles, MaxInlineBytes) {
		t.Fatal("inclusive file/byte boundary did not inline")
	}
	if ShouldInline(MaxInlineFiles+1, MaxInlineBytes) {
		t.Fatal("file-count boundary + 1 inlined")
	}
	if ShouldInline(MaxInlineFiles, MaxInlineBytes+1) {
		t.Fatal("byte boundary + 1 inlined")
	}

	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "visible.txt", "small public change\n")
	writeFixtureFile(t, repo, "client-secret.json", "INLINE_SECRET_MUST_NOT_LEAK\n")
	assembled, err := Assemble(context.Background(), Options{
		CWD:      repo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "delegate-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if assembled.Inline == "" || assembled.ArtifactPath != "" {
		t.Fatalf("inline-bytes=%d artifact=%q, want inline", len(assembled.Inline), assembled.ArtifactPath)
	}
	if !strings.Contains(assembled.Inline, "small public change") || strings.Contains(assembled.Inline, "INLINE_SECRET_MUST_NOT_LEAK") {
		t.Fatalf("inline sanitized context = %q", assembled.Inline)
	}
	if assembled.BackendCWD != assembled.Workspace || assembled.BackendCWD == repo {
		t.Fatalf("safe backend cwd = %q, workspace=%q repo=%q", assembled.BackendCWD, assembled.Workspace, repo)
	}
}

func TestAssembleFileCountThresholdSpillsElevenFiles(t *testing.T) {
	inlineRepo := newGitFixture(t)
	for i := 0; i < MaxInlineFiles; i++ {
		writeFixtureFile(t, inlineRepo, "inline-"+string(rune('a'+i))+".txt", "x\n")
	}
	inline, err := Assemble(context.Background(), Options{
		CWD:      inlineRepo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "inline-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(inline)
	if len(inline.Files) != MaxInlineFiles || inline.Inline == "" || inline.ArtifactPath != "" {
		t.Fatalf("files=%d inline=%d artifact=%q, want inclusive 10-file inline", len(inline.Files), len(inline.Inline), inline.ArtifactPath)
	}

	repo := newGitFixture(t)
	for i := 0; i < MaxInlineFiles+1; i++ {
		writeFixtureFile(t, repo, "file-"+string(rune('a'+i))+".txt", "x\n")
	}
	assembled, err := Assemble(context.Background(), Options{
		CWD:      repo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "delegate-state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if len(assembled.Files) != MaxInlineFiles+1 || assembled.ArtifactPath == "" {
		t.Fatalf("files=%d artifact=%q, want 11-file spill", len(assembled.Files), assembled.ArtifactPath)
	}
}

func TestAssembleBranchScopeUsesResolvedBaseAndCanonicalCWD(t *testing.T) {
	repo := newGitFixture(t)
	gitFixture(t, repo, "switch", "-c", "feature")
	writeFixtureFile(t, repo, "branch.go", "package branch\n// BRANCH_CHANGE\n")
	writeFixtureFile(t, repo, "branch-secret.txt", "BRANCH_SECRET_NEVER\n")
	gitFixture(t, repo, "add", "branch.go", "branch-secret.txt")
	gitFixture(t, repo, "commit", "-m", "feature change")
	baseCommit := gitFixtureOutput(t, repo, "rev-parse", "main")
	head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	assembled, err := Assemble(context.Background(), Options{
		CWD:      link,
		Base:     "main",
		Scope:    ScopeBranch,
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if assembled.CanonicalCWD != canonicalRepo || assembled.RepositoryRoot != canonicalRepo {
		t.Fatalf("canonical cwd=%q root=%q want=%q", assembled.CanonicalCWD, assembled.RepositoryRoot, canonicalRepo)
	}
	if assembled.Base.Source != "explicit" || assembled.Base.Ref != "main" || assembled.Scope != ScopeBranch {
		t.Fatalf("base/scope=%#v %q", assembled.Base, assembled.Scope)
	}
	if assembled.Branch != "feature" || assembled.Head != head {
		t.Fatalf("branch/head=%q/%q, want feature/%q", assembled.Branch, assembled.Head, head)
	}
	for _, want := range []string{
		"base_commit\t" + baseCommit,
		"branch\t\"feature\"",
		"head\t" + head,
	} {
		if !strings.Contains(assembled.Inline, want) {
			t.Fatalf("branch sanitized context missing %q: %q", want, assembled.Inline)
		}
	}
	if !strings.Contains(assembled.Inline, "BRANCH_CHANGE") || strings.Contains(assembled.Inline, "BRANCH_SECRET_NEVER") || !strings.Contains(assembled.Inline, `"branch-secret.txt"`) {
		t.Fatalf("branch sanitized context=%q", assembled.Inline)
	}
	prompt, err := ComposePrompt(KindReview, assembled)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Base commit: " + baseCommit + ".",
		"Branch under review: \"feature\".",
		"HEAD commit: " + head + ".",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("review prompt missing %q: %q", want, prompt)
		}
	}
}

func TestAssemblePublishesReviewIdentityForBranchWorktreeAndDetachedHEAD(t *testing.T) {
	t.Run("checked-out branch", func(t *testing.T) {
		repo := newGitFixture(t)
		gitFixture(t, repo, "switch", "-c", "feature")

		assertAssembledReviewIdentity(t, repo, "feature", gitFixtureOutput(t, repo, "rev-parse", "HEAD"))
	})

	t.Run("linked worktree", func(t *testing.T) {
		repo := newGitFixture(t)
		worktree := filepath.Join(t.TempDir(), "linked")
		gitFixture(t, repo, "worktree", "add", "-b", "linked", worktree, "main")

		assertAssembledReviewIdentity(t, worktree, "linked", gitFixtureOutput(t, worktree, "rev-parse", "HEAD"))
	})

	t.Run("detached HEAD", func(t *testing.T) {
		repo := newGitFixture(t)
		gitFixture(t, repo, "checkout", "--detach")
		if got := gitFixtureOutput(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
			t.Fatalf("detached branch label = %q, want HEAD", got)
		}

		assertAssembledReviewIdentity(t, repo, "(detached)", gitFixtureOutput(t, repo, "rev-parse", "HEAD"))
	})
}

func TestAssembleRedactsSecretLikeBranchMetadata(t *testing.T) {
	repo := newGitFixture(t)
	const secret = "AKIAIOSFODNN7EXAMPLE"
	gitFixture(t, repo, "switch", "-c", "feature-"+secret)

	assembled, err := Assemble(context.Background(), Options{
		CWD:      repo,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	prompt, err := ComposePrompt(KindReview, assembled)
	if err != nil {
		t.Fatal(err)
	}
	for _, emitted := range []string{assembled.Branch, assembled.Inline, prompt} {
		if strings.Contains(emitted, secret) {
			t.Fatalf("branch metadata leaked secret-shaped content: %q", emitted)
		}
		if !strings.Contains(emitted, secretRedactionMarker) {
			t.Fatalf("branch metadata missing redaction marker: %q", emitted)
		}
	}
}

func TestBranchChangesUseCapturedHead(t *testing.T) {
	repo := newGitFixture(t)
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "switch", "-c", "feature")
	const capturedSecret = "CAPTURED_SECRET_BLOB_MUST_NOT_LEAK\n"
	writeFixtureFile(t, repo, ".env", capturedSecret)
	gitFixture(t, repo, "add", ".env")
	gitFixture(t, repo, "commit", "-m", "secret ancestor")
	writeFixtureFile(t, repo, "captured.go", capturedSecret)
	gitFixture(t, repo, "add", "captured.go")
	gitFixture(t, repo, "commit", "-m", "captured head")
	capturedHead := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	capturedSecretBlob := gitFixtureOutput(t, repo, "rev-parse", capturedHead+":.env")
	writeFixtureFile(t, repo, "later.go", "package feature\n// LATER_HEAD_CHANGE\n")
	writeFixtureFile(t, repo, "later-secret.txt", "LATER_SECRET_BLOB_MUST_NOT_LEAK\n")
	gitFixture(t, repo, "add", "later.go", "later-secret.txt")
	gitFixture(t, repo, "commit", "-m", "later head")
	laterSecretBlob := gitFixtureOutput(t, repo, "rev-parse", "HEAD:later-secret.txt")
	gitFixture(t, repo, "rm", "later-secret.txt")
	gitFixture(t, repo, "commit", "-m", "remove later secret")

	files, diffBase, err := branchChanges(context.Background(), repo, base, capturedHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != ".env" || files[1].Path != "captured.go" {
		t.Fatalf("captured-head files = %#v, want only .env and captured.go", files)
	}
	diff, err := trackedDiff(context.Background(), repo, diffBase, capturedHead, files[1].paths()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "CAPTURED_SECRET_BLOB_MUST_NOT_LEAK") || strings.Contains(string(diff), "LATER_HEAD_CHANGE") {
		t.Fatalf("captured-head diff = %q", diff)
	}
	paths, err := collectSecretPathTaint(context.Background(), repo, diffBase, capturedHead)
	if err != nil {
		t.Fatal(err)
	}
	if !paths[".env"] || paths["later-secret.txt"] {
		t.Fatalf("captured-head secret paths = %#v", paths)
	}
	hashes, err := collectSecretBlobHashes(context.Background(), repo, diffBase, capturedHead)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hashes[capturedSecretBlob]; !ok {
		t.Fatalf("captured secret blob %q was not tainted", capturedSecretBlob)
	}
	if _, ok := hashes[laterSecretBlob]; ok {
		t.Fatalf("later secret blob %q was tainted outside captured range", laterSecretBlob)
	}
	if !diffReferencesSecretBlob(diff, hashes) {
		t.Fatal("captured secret blob did not redact the captured diff")
	}
}

func TestReviewWorkspaceAndArtifactCleanup(t *testing.T) {
	t.Run("inline workspace", func(t *testing.T) {
		repo := newGitFixture(t)
		writeFixtureFile(t, repo, "visible.txt", "change\n")
		assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
		if err != nil {
			t.Fatal(err)
		}
		workspace := assembled.Workspace
		if err := Cleanup(assembled); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("workspace still exists or stat failed: %v", err)
		}
	})

	t.Run("spilled artifact", func(t *testing.T) {
		repo := newGitFixture(t)
		writeFixtureFile(t, repo, "large.txt", strings.Repeat("x", MaxInlineBytes+1))
		assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
		if err != nil {
			t.Fatal(err)
		}
		workspace, artifact := assembled.Workspace, assembled.ArtifactPath
		if err := Cleanup(assembled); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{workspace, artifact} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s still exists or stat failed: %v", path, err)
			}
		}
	})
}

func TestAllowLiveRepoReadGatesBackendCWDAndPrompt(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "visible.txt", "change\n")
	state := filepath.Join(t.TempDir(), "state")

	safe, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(safe)
	safePrompt, err := ComposePrompt(KindReview, safe)
	if err != nil {
		t.Fatal(err)
	}
	if safe.BackendCWD == safe.CanonicalCWD || strings.Contains(safePrompt, "LIVE-REPOSITORY MODE") || !strings.Contains(safePrompt, "does not prevent a same-user backend") {
		t.Fatalf("safe gating failed: cwd=%q prompt=%q", safe.BackendCWD, safePrompt)
	}

	live, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state, AllowLiveRepoRead: true})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(live)
	livePrompt, err := ComposePrompt(KindReview, live)
	if err != nil {
		t.Fatal(err)
	}
	if live.BackendCWD != live.CanonicalCWD || !strings.Contains(livePrompt, "LIVE-REPOSITORY MODE") || !strings.Contains(livePrompt, "makes backend file reads easier") || !strings.Contains(livePrompt, "remain authoritative") {
		t.Fatalf("live gating failed: cwd=%q prompt=%q", live.BackendCWD, livePrompt)
	}
}

func TestComposeAdversarialPromptIsRefuteFirst(t *testing.T) {
	prompt, err := ComposePrompt(KindAdversarialReview, Context{Scope: ScopeWorkingTree, Inline: "context\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "refute-first") || !strings.Contains(prompt, "trying to disprove") {
		t.Fatalf("adversarial prompt = %q", prompt)
	}
}

func TestComposePromptTreatsSuppliedReviewContextAsAuthoritative(t *testing.T) {
	assembled := Context{
		Scope:        ScopeBranch,
		Base:         Base{Ref: "origin/main", Commit: "1111111111111111111111111111111111111111", Source: "default-remote"},
		Branch:       "feature",
		Head:         "2222222222222222222222222222222222222222",
		ArtifactPath: filepath.Join(t.TempDir(), artifactFilename),
	}
	for _, kind := range []string{KindReview, KindAdversarialReview} {
		t.Run(kind, func(t *testing.T) {
			prompt, err := ComposePrompt(kind, assembled)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"The supplied effective scope, resolved base, base commit, branch under review, and HEAD commit are authoritative for this review.",
				"Report them as given rather than as unavailable.",
				"For the report's Scope boundary, use the supplied branch, base commit, and HEAD commit identifiers; do not infer or claim a full commit list.",
				"Reading the assembled context is the first and only required step.",
				"Do not run git or any other repository-inspection command to recover metadata or context, and do not put a repository probe before the context read with &&.",
				"A sandbox denial of an unnecessary probe is not a reason to stop: read the assembled context and complete the review.",
				"Branch under review: \"feature\".",
				"HEAD commit: 2222222222222222222222222222222222222222.",
				"\"review.patch\"",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("review prompt missing %q:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestSecretContentPatternClasses(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		content string
	}{
		{name: "AWS access key", pattern: "aws-access-key", content: `+aws_access_key_id = "AKIAIOSFODNN7EXAMPLE"`},
		{name: "generic api key assignment", pattern: "generic-secret-assignment", content: `+api_key = "mF9xQ2vL7pR4sT8wY1zC6nK3"`},
		{name: "generic token assignment", pattern: "generic-secret-assignment", content: `+token: pR8vN2kL6xQ4sT9mW3yF7cH1`},
		{name: "generic secret assignment", pattern: "generic-secret-assignment", content: `+secret = zK7pQ2vM9xR4tW8nL3cF6hJ1`},
		{name: "PEM private key", pattern: "private-key-header", content: "+-----BEGIN RSA PRIVATE KEY-----"},
		{name: "JWT", pattern: "jwt", content: `+jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlMTIzNDU2"`},
		{name: "GitHub ghp token", pattern: "github-token", content: "+ghp_abcdefghijklmnopqrstuvwxyz1234567890"},
		{name: "GitHub gho token", pattern: "github-token", content: "+gho_abcdefghijklmnopqrstuvwxyz1234567890"},
		{name: "GitHub ghs token", pattern: "github-token", content: "+ghs_abcdefghijklmnopqrstuvwxyz1234567890"},
		{name: "Slack token", pattern: "slack-token", content: "+xoxb-123456789012-abcdefghijklmnopqrstuv"},
		{name: "URI connection string", pattern: "connection-string-uri-password", content: `+DATABASE_URL="postgres://delegate:supersensitivepassword@db.example/app"`},
		{name: "keyword connection string", pattern: "connection-string-password", content: `+CONNECTION="Server=db.example;Database=app;User Id=sa;Password=hunter2"`},
		{name: "password-first connection string", pattern: "connection-string-password-first", content: `+CONNECTION="Password=hunter2;Server=db.example;Database=app"`},
		{name: "high entropy hex assignment", pattern: "hex-assignment", content: `+digest = "9f4a7c1d8e2b6a03d5f9c7e1b4a8620df3c9157e"`},
		{name: "high entropy base64 assignment", pattern: "base64-assignment", content: `+blob = "QWxhZGRpbjpPcGVuU2VzYW1lU2VjcmV0VmFsdWUxMjM0NTY="`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, pattern := range secretContentPatterns {
				if pattern.name == test.pattern && secretPatternMatches(pattern, []byte(test.content)) {
					return
				}
			}
			t.Fatalf("secret pattern %q did not match %q", test.pattern, test.content)
		})
	}
}

func TestSecretContentScanCleanDiffSmoke(t *testing.T) {
	clean := []byte(`diff --git a/config.go b/config.go
index 0123456789abcdef0123456789abcdef01234567..89abcdef0123456789abcdef0123456789abcdef 100644
--- a/config.go
+++ b/config.go
@@ -1,3 +1,6 @@
 package config
+const tokenCount = 12
+const apiKeyName = "primary"
+const checksumAlgorithm = "sha256"
`)
	if containsSecretLikeContent(clean) {
		t.Fatalf("clean diff was classified as secret-like: %q", clean)
	}
	if got := redactSecretLikeHunks(clean); string(got) != string(clean) {
		t.Fatalf("clean diff changed by final scan:\n%s", got)
	}
}

func TestAssembleFinalSecretGateRedactsMatchingHunksInlineAndSpilled(t *testing.T) {
	t.Run("inline preserves clean hunk", func(t *testing.T) {
		repo := newGitFixture(t)
		lines := make([]string, 60)
		for i := range lines {
			lines[i] = "unchanged line " + strconv.Itoa(i)
		}
		writeFixtureFile(t, repo, "app.txt", strings.Join(lines, "\n")+"\n")
		gitFixture(t, repo, "add", "app.txt")
		gitFixture(t, repo, "commit", "-m", "add app fixture")
		lines[1] = `aws_access_key_id = "AKIAIOSFODNN7EXAMPLE"`
		lines[50] = "CLEAN_SECOND_HUNK_REMAINS"
		writeFixtureFile(t, repo, "app.txt", strings.Join(lines, "\n")+"\n")

		assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
		if err != nil {
			t.Fatal(err)
		}
		defer Cleanup(assembled)
		for _, want := range []string{"FILE\tM\t\"app.txt\"", secretRedactionMarker, "CLEAN_SECOND_HUNK_REMAINS"} {
			if !strings.Contains(assembled.Inline, want) {
				t.Fatalf("inline context missing %q:\n%s", want, assembled.Inline)
			}
		}
		if strings.Contains(assembled.Inline, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("inline context leaked secret:\n%s", assembled.Inline)
		}
	})

	t.Run("spilled artifact", func(t *testing.T) {
		repo := newGitFixture(t)
		writeFixtureFile(t, repo, "file-a.txt", "AKIAIOSFODNN7EXAMPLE\n")
		for i := 1; i < MaxInlineFiles+1; i++ {
			writeFixtureFile(t, repo, "file-"+strconv.Itoa(i)+".txt", "clean change\n")
		}
		assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
		if err != nil {
			t.Fatal(err)
		}
		defer Cleanup(assembled)
		if assembled.ArtifactPath == "" || assembled.Inline != "" {
			t.Fatalf("artifact=%q inline=%d, want spilled output", assembled.ArtifactPath, len(assembled.Inline))
		}
		raw, err := os.ReadFile(assembled.ArtifactPath)
		if err != nil {
			t.Fatal(err)
		}
		artifact := string(raw)
		for _, want := range []string{"FILE\t??\t\"file-a.txt\"", secretRedactionMarker, "clean change"} {
			if !strings.Contains(artifact, want) {
				t.Fatalf("spilled artifact missing %q:\n%s", want, artifact)
			}
		}
		if strings.Contains(artifact, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("spilled artifact leaked secret:\n%s", artifact)
		}
	})
}

func TestIsSecretPath(t *testing.T) {
	for _, path := range []string{
		".ENV", "config/.env.local", "config/PROD.ENV", ".NETRC", ".NPMRC",
		"Credentials.json", "backup-creds.yml", "db-PASSWORD.txt", "nested/secrets/value.txt", "api-token.txt",
		"KUBECONFIG-prod", "KUBE_CONFIG", "prod-SERVICE-ACCOUNT-key.JSON", "SERVICE_ACCOUNT.JSON", "service_account.json", "service_api_key.json", ".kube/config",
		"id_rsa", "id_ECDSA.pub", "id_ed25519_backup", "cert/client.P12", "cert/client.PFX",
		"stores/app.JKS", "stores/app.KEYSTORE", "tls/private.PEM", "tls/private.KEY", "auth/session.TOKEN",
		"home/.AWS/config", "home/.SSH/config", "home/.GNUPG/pubring.kbx",
	} {
		if !IsSecretPath(path) {
			t.Errorf("IsSecretPath(%q) = false", path)
		}
	}
	for _, path := range []string{"main.go", "docs/review.md", "keyboard.txt"} {
		if IsSecretPath(path) {
			t.Errorf("IsSecretPath(%q) = true", path)
		}
	}
}

func newGitFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "Delegate Test")
	gitFixture(t, repo, "config", "user.email", "delegate@example.invalid")
	writeFixtureFile(t, repo, "README.md", "fixture\n")
	gitFixture(t, repo, "add", "README.md")
	gitFixture(t, repo, "commit", "-m", "initial")
	return repo
}

func writeFixtureFile(t *testing.T, repo, name, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitFixture(t *testing.T, repo string, args ...string) {
	t.Helper()
	_ = gitFixtureOutput(t, repo, args...)
}

func gitFixtureOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func assertAssembledReviewIdentity(t *testing.T, cwd, wantBranch, wantHead string) {
	t.Helper()
	assembled, err := Assemble(context.Background(), Options{
		CWD:      cwd,
		Scope:    ScopeWorkingTree,
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Cleanup(assembled); err != nil {
			t.Errorf("Cleanup() error = %v", err)
		}
	})
	if assembled.Branch != wantBranch || assembled.Head != wantHead {
		t.Fatalf("branch/head=%q/%q, want %q/%q", assembled.Branch, assembled.Head, wantBranch, wantHead)
	}
}
