package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
		head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
		gitFixture(t, repo, "remote", "add", "origin", repo)
		gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", head)
		gitFixture(t, repo, "config", "branch.main.remote", "origin")
		gitFixture(t, repo, "config", "branch.main.merge", "refs/heads/main")
		got, err := ResolveBase(context.Background(), repo, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != "upstream" || got.Ref != "origin/main" || got.Commit != head {
			t.Fatalf("ResolveBase() = %#v, want upstream origin/main at %s", got, head)
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
		for _, fragment := range []string{"--base <ref>", "upstream tracking branch", "default remote branch"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("ResolveBase() error = %q, want %q", err, fragment)
			}
		}
	})
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
	if !strings.Contains(assembled.Inline, "BRANCH_CHANGE") || strings.Contains(assembled.Inline, "BRANCH_SECRET_NEVER") || !strings.Contains(assembled.Inline, `"branch-secret.txt"`) {
		t.Fatalf("branch sanitized context=%q", assembled.Inline)
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
	if safe.BackendCWD == safe.CanonicalCWD || strings.Contains(safePrompt, "UNSAFE LIVE-REPOSITORY MODE") || !strings.Contains(safePrompt, "live repository is intentionally unavailable") {
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
	if live.BackendCWD != live.CanonicalCWD || !strings.Contains(livePrompt, "UNSAFE LIVE-REPOSITORY MODE") || !strings.Contains(livePrompt, "not secret-safe") || !strings.Contains(livePrompt, "self-collect") {
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

func TestIsSecretPath(t *testing.T) {
	for _, path := range []string{".ENV", "config/.env.local", "Credentials.json", "nested/secrets/value.txt", "api-token.txt", "service_api_key.json", "id_rsa", "tls/private.pem"} {
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
