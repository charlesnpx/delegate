package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCaptureEffectiveGitChangesWithoutWritingSource(t *testing.T) {
	repo := newGitFixture(t)
	for name, content := range map[string]string{
		"tracked.txt": "before tracked\n", "recreated.txt": "before recreation\n", "delete.txt": "before delete\n",
		"rename-old.txt": "unique rename source content\n", "mode.sh": "#!/bin/sh\ntrue\n", "binary.bin": "old\x00binary",
	} {
		writeFixtureFile(t, repo, name, content)
	}
	gitFixture(t, repo, "add", ".")
	gitFixture(t, repo, "commit", "-m", "capture baseline")
	writeFixtureFile(t, repo, "tracked.txt", "staged A\n")
	gitFixture(t, repo, "add", "tracked.txt")
	writeFixtureFile(t, repo, "tracked.txt", "effective B\n")
	writeFixtureFile(t, repo, "staged addition.txt", "staged addition\n")
	gitFixture(t, repo, "add", "staged addition.txt")
	gitFixture(t, repo, "rm", "--cached", "recreated.txt")
	writeFixtureFile(t, repo, "recreated.txt", "effective recreation\n")
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "mv", "rename-old.txt", "rename-new.txt")
	if err := os.Chmod(filepath.Join(repo, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, repo, "binary.bin", "new\x00binary")
	writeFixtureFile(t, repo, "empty.txt", "")
	writeFixtureFile(t, repo, "- literal\nname.txt", "literal path\n")
	outside := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(outside, []byte("EXTERNAL_TARGET_CONTENT_MUST_NOT_APPEAR"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}

	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := objectFiles(t, repo)
	head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	state := filepath.Join(t.TempDir(), "state")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	for _, want := range []string{"effective B", "staged addition", "effective recreation", "rename-new.txt", "new mode 100755", "literal path"} {
		if !bytes.Contains(packet, []byte(want)) {
			t.Errorf("packet missing %q", want)
		}
	}
	for _, forbidden := range []string{"staged A", "new\x00binary", "GIT binary patch", "EXTERNAL_TARGET_CONTENT_MUST_NOT_APPEAR"} {
		if bytes.Contains(packet, []byte(forbidden)) {
			t.Errorf("packet contains %q", forbidden)
		}
	}
	entries := make(map[string]ManifestEntry)
	for _, entry := range assembled.Manifest {
		entries[entry.Path] = entry
	}
	for name, content := range map[string]string{"tracked.txt": "effective B\n", "staged addition.txt": "staged addition\n", "recreated.txt": "effective recreation\n", "empty.txt": "", "link.txt": outside} {
		entry, ok := entries[name]
		if !ok || entry.ByteSize != int64(len(content)) || entry.SHA256 != digestBytes([]byte(content)) {
			t.Errorf("%s manifest=%#v", name, entry)
		}
	}
	for _, name := range []string{"delete.txt", "rename-old.txt"} {
		entry := entries[name]
		if entry.Status != "omitted" || entry.Reason != "deleted" || entry.SHA256 != "" {
			t.Errorf("deleted %s manifest=%#v", name, entry)
		}
	}
	if entries["binary.bin"].Reason != "binary" {
		t.Errorf("binary manifest=%#v", entries["binary.bin"])
	}
	if assembled.Head != head || assembled.ComparisonBase != head || assembled.Base.Commit != "" {
		t.Errorf("working-tree identities=%#v", assembled)
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Error("capture modified source index")
	}
	if objectsAfter := objectFiles(t, repo); !reflect.DeepEqual(objectsBefore, objectsAfter) {
		t.Errorf("source objects changed: before=%v after=%v", objectsBefore, objectsAfter)
	}
	assertNoRawCapture(t, state)
}

func TestCaptureRemainsCoherentAfterWorktreeAndHeadMove(t *testing.T) {
	repo := newGitFixture(t)
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	writeFixtureFile(t, repo, "coherent.txt", "committed\n")
	gitFixture(t, repo, "add", "coherent.txt")
	gitFixture(t, repo, "commit", "-m", "new head")
	head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	writeFixtureFile(t, repo, "coherent.txt", "captured content\n")
	installGitWriteTreeMutation(t, repo, base)
	state := filepath.Join(t.TempDir(), "state")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	if !bytes.Contains(packet, []byte("captured content")) || bytes.Contains(packet, []byte("later live content")) {
		t.Fatalf("packet did not retain captured input: %s", packet)
	}
	if assembled.Head != head || assembled.ComparisonBase != head {
		t.Fatalf("captured head changed: %#v", assembled)
	}
	if assembled.InputDigest != "sha256:"+digestBytes(packet) {
		t.Fatal("input digest does not match packet bytes")
	}
	for _, entry := range assembled.Manifest {
		if entry.Path == "coherent.txt" && (entry.SHA256 != digestBytes([]byte("captured content\n")) || entry.ByteSize != int64(len("captured content\n"))) {
			t.Fatalf("manifest describes a later file: %#v", entry)
		}
	}
	assertNoRawCapture(t, state)
}

func TestBranchCaptureIgnoresSamePathAndUnrelatedWorktreeEdits(t *testing.T) {
	repo := newGitFixture(t)
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	writeFixtureFile(t, repo, "branch.txt", "committed branch content\n")
	gitFixture(t, repo, "add", "branch.txt")
	gitFixture(t, repo, "commit", "-m", "branch change")
	head := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	writeFixtureFile(t, repo, "branch.txt", "later dirty content\n")
	writeFixtureFile(t, repo, "unrelated.txt", "unrelated dirty content\n")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeBranch, Base: base, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	if !bytes.Contains(packet, []byte("committed branch content")) || bytes.Contains(packet, []byte("later dirty content")) || bytes.Contains(packet, []byte("unrelated dirty content")) {
		t.Fatalf("branch packet=%s", packet)
	}
	if assembled.Head != head || assembled.Base.Commit != base || assembled.ComparisonBase != base {
		t.Fatalf("branch identities=%#v", assembled)
	}
}

func TestCaptureRejectsUnmergedAndSpecialFiles(t *testing.T) {
	t.Run("special", func(t *testing.T) {
		repo := newGitFixture(t)
		if err := syscall.Mkfifo(filepath.Join(repo, "fifo"), 0o600); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(t.TempDir(), "state")
		_, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
		if err == nil || !strings.Contains(err.Error(), "unsupported special file") {
			t.Fatalf("error=%v", err)
		}
		assertNoRawCapture(t, state)
	})
	t.Run("unmerged", func(t *testing.T) {
		repo := newGitFixture(t)
		writeFixtureFile(t, repo, "conflict.txt", "base\n")
		gitFixture(t, repo, "add", "conflict.txt")
		gitFixture(t, repo, "commit", "-m", "base")
		gitFixture(t, repo, "switch", "-c", "other")
		writeFixtureFile(t, repo, "conflict.txt", "other\n")
		gitFixture(t, repo, "commit", "-am", "other")
		gitFixture(t, repo, "switch", "main")
		writeFixtureFile(t, repo, "conflict.txt", "main\n")
		gitFixture(t, repo, "commit", "-am", "main")
		if err := exec.Command("git", "-C", repo, "merge", "other").Run(); err == nil {
			t.Fatal("fixture merge unexpectedly succeeded")
		}
		state := filepath.Join(t.TempDir(), "state")
		_, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
		if err == nil || !strings.Contains(err.Error(), "unmerged") {
			t.Fatalf("error=%v", err)
		}
		assertNoRawCapture(t, state)
	})
}

func TestCaptureCancellationRemovesRawDirectory(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "change.txt", "content\n")
	bin := t.TempDir()
	fifo := filepath.Join(bin, "started")
	block := filepath.Join(bin, "block")
	if err := syscall.Mkfifo(block, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case " $* " in
  *" diff --binary "*) printf x > "$TEST_CAPTURE_FIFO"; read unused < "$TEST_CAPTURE_BLOCK" ;;
esac
exec "$TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_REAL_GIT", realGit)
	t.Setenv("TEST_CAPTURE_FIFO", fifo)
	t.Setenv("TEST_CAPTURE_BLOCK", block)
	state := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Assemble(ctx, Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
		done <- err
	}()
	started := make(chan error, 1)
	go func() {
		f, err := os.Open(fifo)
		if err == nil {
			var b [1]byte
			_, err = f.Read(b[:])
			_ = f.Close()
		}
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled capture succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not stop")
	}
	assertNoRawCapture(t, state)
}

func installGitWriteTreeMutation(t *testing.T, repo, newHead string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
"$TEST_REAL_GIT" "$@"
status=$?
case " $* " in
  *" write-tree "*)
    if [ "$status" -eq 0 ] && [ ! -e "$TEST_MUTATION_MARKER" ]; then
      : > "$TEST_MUTATION_MARKER"
      printf 'later live content\n' > "$TEST_REPO/coherent.txt"
      "$TEST_REAL_GIT" -C "$TEST_REPO" update-ref HEAD "$TEST_NEW_HEAD"
    fi ;;
esac
exit "$status"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_REAL_GIT", realGit)
	t.Setenv("TEST_REPO", repo)
	t.Setenv("TEST_NEW_HEAD", newHead)
	t.Setenv("TEST_MUTATION_MARKER", filepath.Join(bin, "mutated"))
}

func packetBytes(t *testing.T, assembled Context) []byte {
	t.Helper()
	if assembled.ArtifactPath == "" {
		return []byte(assembled.Inline)
	}
	raw, err := os.ReadFile(assembled.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertNoRawCapture(t *testing.T, state string) {
	t.Helper()
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".review-capture-") {
			t.Fatalf("raw capture retained: %s", entry.Name())
		}
	}
}

func objectFiles(t *testing.T, repo string) []string {
	t.Helper()
	root := filepath.Join(repo, ".git", "objects")
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestSelectedContentBoundCountsDistinctBlobs(t *testing.T) {
	repo := newGitFixture(t)
	// The same 33 MiB binary blob appears twice. Reconstruction patches are
	// compressed, and the selected-content limit counts the object only once.
	for _, name := range []string{"large-one.bin", "large-two.bin"} {
		file, err := os.Create(filepath.Join(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(33 << 20); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(t.TempDir(), "state")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	for _, entry := range assembled.Manifest {
		if strings.HasPrefix(entry.Path, "large-") && (entry.Reason != "binary" || entry.ByteSize != 33<<20) {
			t.Errorf("binary manifest=%#v", entry)
		}
	}
	assertNoRawCapture(t, state)
}

func TestSelectedContentBoundRejectsOversize(t *testing.T) {
	repo := newGitFixture(t)
	file, err := os.Create(filepath.Join(repo, "oversize.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	_, err = Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err == nil || !strings.Contains(err.Error(), "selected review content exceeds 64 MiB") {
		t.Fatalf("oversize error=%v", err)
	}
	assertNoRawCapture(t, state)
}

func TestAggregatePatchLimitAcrossAdditions(t *testing.T) {
	repo := newGitFixture(t)
	// Separate no-index calls must share one 64 MiB capture allowance.
	line := strings.Repeat("abcdefgh", 128) + "\n"
	for i := 0; i < 2; i++ {
		path := filepath.Join(repo, fmt.Sprintf("large-%d.txt", i))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 34*1024; j++ {
			if _, err := f.WriteString(line); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(t.TempDir(), "state")
	_, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err == nil || !strings.Contains(err.Error(), "patch output exceeds 64 MiB") {
		t.Fatalf("patch bound error=%v", err)
	}
	assertNoRawCapture(t, state)
}

func TestFinalDiffLimitAggregatesAcrossFiles(t *testing.T) {
	repo := newGitFixture(t)
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	line := strings.Repeat("abcdefgh", 128) + "\n"
	for _, name := range []string{"large-one.txt", "large-two.txt"} {
		file, err := os.Create(filepath.Join(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 34*1024; i++ {
			if _, err := file.WriteString(line); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	gitFixture(t, repo, "add", "large-one.txt", "large-two.txt")
	gitFixture(t, repo, "commit", "-m", "large branch")
	state := filepath.Join(t.TempDir(), "state")
	_, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeBranch, Base: base, StateDir: state})
	if err == nil || !strings.Contains(err.Error(), "patch output exceeds 64 MiB") {
		t.Fatalf("render bound error=%v", err)
	}
	assertNoRawCapture(t, state)
}

func TestCaptureUnbornAndSHA256ObjectFormat(t *testing.T) {
	for _, scope := range []string{ScopeWorkingTree, ScopeAuto} {
		t.Run(scope, func(t *testing.T) {
			repo := t.TempDir()
			if output, err := exec.Command("git", "-C", repo, "init", "--object-format=sha256", "-b", "main").CombinedOutput(); err != nil {
				t.Fatalf("initialize sha256 repository: %v %s", err, output)
			}
			writeFixtureFile(t, repo, "new.txt", "unborn captured\n")
			objects := objectFiles(t, repo)
			state := filepath.Join(t.TempDir(), "state")
			assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: scope, StateDir: state})
			if err != nil {
				t.Fatal(err)
			}
			defer Cleanup(assembled)
			if assembled.Head != "" || len(assembled.ComparisonBase) != 64 || assembled.Base.Source != "empty-tree" || !strings.Contains(string(packetBytes(t, assembled)), "unborn captured") {
				t.Fatalf("unborn captured state=%#v", assembled)
			}
			if got := objectFiles(t, repo); !reflect.DeepEqual(objects, got) {
				t.Fatalf("unborn capture wrote source objects: %v -> %v", objects, got)
			}
			assertNoRawCapture(t, state)
		})
	}
}

func TestCaptureUsesSourceIndexAndObjectEnvironment(t *testing.T) {
	repo := newGitFixture(t)
	custom := t.TempDir()
	objects := filepath.Join(custom, "objects")
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), objects); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(custom, "index")
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_OBJECT_DIRECTORY", objects)
	t.Setenv("GIT_INDEX_FILE", index)
	writeFixtureFile(t, repo, "external-index.txt", "custom staged content\n")
	gitFixture(t, repo, "add", "external-index.txt")
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if !bytes.Contains(packetBytes(t, assembled), []byte("custom staged content")) {
		t.Fatal("source custom index was not captured")
	}
	indexAfter, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("source custom index changed")
	}
	assertNoRawCapture(t, state)
}

func TestCaptureTreeTaintSurvivesUnmatchedSecretTransition(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, ".env", "old sensitive material\n")
	gitFixture(t, repo, "add", ".env")
	gitFixture(t, repo, "commit", "-m", "secret source")
	if err := os.Remove(filepath.Join(repo, ".env")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, repo, "public.txt", "different new material after uncertain rename\n")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	if bytes.Contains(packet, []byte("different new material")) || !bytes.Contains(packet, []byte("REDACTED\tA\t\"public.txt\"")) {
		t.Fatalf("uncertain secret transition was not withheld: %s", packet)
	}
}

func TestCaptureStateDirectoryInsideRepositoryDoesNotCaptureItself(t *testing.T) {
	repo := newGitFixture(t)
	writeFixtureFile(t, repo, "change.txt", "ordinary change\n")
	state := filepath.Join(repo, "state")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	if !bytes.Contains(packet, []byte("ordinary change")) || bytes.Contains(packet, []byte(".review-capture-")) {
		t.Fatalf("capture included its own private files: %s", packet)
	}
	assertNoRawCapture(t, state)
}

func TestDiffRedactsPrivateKeyBodyWithoutHeaderInHunk(t *testing.T) {
	for _, terminated := range []bool{true, false} {
		t.Run(fmt.Sprintf("terminated_%t", terminated), func(t *testing.T) {
			repo := newGitFixture(t)
			lines := []string{"ordinary first line", "-----BEGIN RSA PRIVATE KEY-----"}
			for i := 0; i < 30; i++ {
				lines = append(lines, fmt.Sprintf("PRIVATE_BODY_LINE_%02d", i))
			}
			if terminated {
				lines = append(lines, "-----END RSA PRIVATE KEY-----", "ordinary final line")
			}
			writeFixtureFile(t, repo, "public.txt", strings.Join(lines, "\n")+"\n")
			gitFixture(t, repo, "add", "public.txt")
			gitFixture(t, repo, "commit", "-m", "key fixture")
			lines[16] = "CHANGED_PRIVATE_BODY_MUST_NOT_ESCAPE"
			writeFixtureFile(t, repo, "public.txt", strings.Join(lines, "\n")+"\n")
			assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: filepath.Join(t.TempDir(), "state")})
			if err != nil {
				t.Fatal(err)
			}
			defer Cleanup(assembled)
			packet := packetBytes(t, assembled)
			for _, secret := range []string{"CHANGED_PRIVATE_BODY_MUST_NOT_ESCAPE", "PRIVATE_BODY_LINE_"} {
				if bytes.Contains(packet, []byte(secret)) {
					t.Fatalf("private-key body leaked in diff or source: %s", packet)
				}
			}
			if !bytes.Contains(packet, []byte(secretRedactionMarker)) || !bytes.Contains(packet, []byte("ordinary first line")) {
				t.Fatalf("private-key screening lost marker or surrounding source: %s", packet)
			}
		})
	}
}

func TestAutoTaintsPathThroughSecretRemovedAfterCapturedHead(t *testing.T) {
	repo := newGitFixture(t)
	base := gitFixtureOutput(t, repo, "rev-parse", "HEAD")
	writeFixtureFile(t, repo, ".env", "secret in committed branch\n")
	gitFixture(t, repo, "add", ".env")
	gitFixture(t, repo, "commit", "-m", "add transient secret")
	if err := os.Remove(filepath.Join(repo, ".env")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, repo, "visible.txt", "different content after uncertain transition\n")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeAuto, Base: base, StateDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	packet := packetBytes(t, assembled)
	if bytes.Contains(packet, []byte("different content after uncertain transition")) || !bytes.Contains(packet, []byte("REDACTED\tA\t\"visible.txt\"")) {
		t.Fatalf("HEAD-to-capture secret transition escaped: %s", packet)
	}
}

func TestCaptureWithSourceObjectAlternatesAndQuotedPaths(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo:\x01é \n")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, repo, "init", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "Delegate Test")
	gitFixture(t, repo, "config", "user.email", "delegate@example.invalid")
	writeFixtureFile(t, repo, "tracked.txt", "before\n")
	gitFixture(t, repo, "add", "tracked.txt")
	gitFixture(t, repo, "commit", "-m", "baseline")
	alternate := filepath.Join(root, "alternate objects")
	if err := os.Rename(filepath.Join(repo, ".git", "objects"), alternate); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(repo, ".git", "objects", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(alternate+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, repo, "tracked.txt", "captured through alternates\n")
	state := filepath.Join(root, "state:\x01é")
	assembled, err := Assemble(context.Background(), Options{CWD: repo, Scope: ScopeWorkingTree, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(assembled)
	if !bytes.Contains(packetBytes(t, assembled), []byte("captured through alternates")) {
		t.Fatal("alternate source objects were not readable")
	}
	assertNoRawCapture(t, state)
}

func TestDiffRemovesPrivateKeyFunctionContextOutsideHunk(t *testing.T) {
	key := []byte("-----BEGIN PRIVATE KEY-----\nKEY_BODY_MUST_NOT_ESCAPE\n-----END PRIVATE KEY-----\n")
	diff := []byte("diff --git a/public b/public\n@@ -100,1 +100,1 @@ KEY_BODY_MUST_NOT_ESCAPE\n-old\n+new\n")
	safe := redactPrivateKeyDiffHunks(diff, key, key)
	if bytes.Contains(safe, []byte("KEY_BODY_MUST_NOT_ESCAPE")) || !bytes.Contains(safe, []byte("+new")) {
		t.Fatalf("unsafe or over-redacted hunk: %s", safe)
	}
}
