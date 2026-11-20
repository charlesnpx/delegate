package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const maxCaptureBytes = 64 * 1024 * 1024

// reviewCapture owns only the temporary index and object directory. Source
// commands keep the caller's Git environment; reconstruction uses these
// private objects with the source object store as a read-only alternate.
type reviewCapture struct {
	dir       string
	objects   string
	ctx       context.Context
	sourceCtx context.Context
}

type gitEnvKey struct{}

func gitEnvironment(ctx context.Context) []string {
	if env, ok := ctx.Value(gitEnvKey{}).([]string); ok {
		return env
	}
	return os.Environ()
}

func replaceEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	prefix := key + "="
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func appendAlternate(existing, directory string) string {
	// Git's C-style parser accepts octal byte escapes, not all of the escape
	// forms emitted by Go's string quoting. Quote each path-list separator and
	// encode control and non-ASCII bytes in the form Git understands.
	var out strings.Builder
	out.WriteByte('"')
	for _, b := range []byte(directory) {
		switch {
		case b == '\\' || b == '"':
			out.WriteByte('\\')
			out.WriteByte(b)
		case b < 0x20 || b >= 0x7f:
			fmt.Fprintf(&out, "\\%03o", b)
		default:
			out.WriteByte(b)
		}
	}
	out.WriteByte('"')
	quoted := out.String()
	if existing == "" {
		return quoted
	}
	return existing + string(os.PathListSeparator) + quoted
}

func newReviewCapture(ctx context.Context, repoRoot, stateDir string) (*reviewCapture, error) {
	dir, err := os.MkdirTemp(stateDir, ".review-capture-")
	if err != nil {
		return nil, fmt.Errorf("create private review capture: %w", err)
	}
	capture := &reviewCapture{dir: dir, objects: filepath.Join(dir, "objects"), sourceCtx: ctx}
	if err := os.Mkdir(capture.objects, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	raw, err := gitOutput(ctx, repoRoot, false, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("resolve source Git object directory: %w", err)
	}
	sourceObjects := strings.TrimSuffix(string(raw), "\n")
	if !filepath.IsAbs(sourceObjects) {
		_ = os.RemoveAll(dir)
		return nil, errors.New("source Git object directory is not absolute")
	}
	env := gitEnvironment(ctx)
	privateEnv := replaceEnv(env, "GIT_INDEX_FILE", filepath.Join(dir, "index"))
	privateEnv = replaceEnv(privateEnv, "GIT_OBJECT_DIRECTORY", capture.objects)
	privateEnv = replaceEnv(privateEnv, "GIT_ALTERNATE_OBJECT_DIRECTORIES", appendAlternate(envValue(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES"), sourceObjects))
	capture.ctx = context.WithValue(ctx, gitEnvKey{}, privateEnv)
	return capture, nil
}

func (capture *reviewCapture) cleanup() error {
	return os.RemoveAll(capture.dir)
}

func (capture *reviewCapture) emptyTree(repoRoot string) (string, error) {
	raw, err := gitOutput(capture.ctx, repoRoot, false, "hash-object", "-w", "-t", "tree", "/dev/null")
	if err != nil {
		return "", fmt.Errorf("create private empty tree: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// captureWorkingTree reconstructs the effective tracked patch and untracked
// additions in a private index. It never stages, writes objects in, or modifies
// the source repository.
func (capture *reviewCapture) captureWorkingTree(repoRoot, head string, unborn bool) (string, error) {
	if raw, err := gitOutput(capture.sourceCtx, repoRoot, false, "ls-files", "--unmerged", "-z", "--"); err != nil {
		return "", fmt.Errorf("inspect source index: %w", err)
	} else if len(raw) != 0 {
		return "", errors.New("cannot capture review input with an unmerged index; resolve conflicts first")
	}
	if _, err := gitOutput(capture.ctx, repoRoot, false, "read-tree", head); err != nil {
		return "", fmt.Errorf("initialize private review index: %w", err)
	}
	sourceCtx := capture.sourceCtx
	if unborn {
		env := gitEnvironment(sourceCtx)
		env = replaceEnv(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES", appendAlternate(envValue(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES"), capture.objects))
		sourceCtx = context.WithValue(sourceCtx, gitEnvKey{}, env)
	}
	if err := rejectSpecialFiles(sourceCtx, repoRoot, capture.dir); err != nil {
		return "", err
	}
	// Inspect tracked paths without opening symlinks or other unsupported types.
	tracked, err := gitOutput(sourceCtx, repoRoot, false, "ls-files", "-z", "--")
	if err != nil {
		return "", fmt.Errorf("enumerate tracked review paths: %w", err)
	}
	for _, path := range splitNUL(tracked) {
		if !filepath.IsLocal(path) {
			return "", fmt.Errorf("invalid tracked review path %q", RedactSecretLikeDiagnostic(path))
		}
		info, statErr := os.Lstat(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("inspect tracked review path %q: %w", RedactSecretLikeDiagnostic(path), statErr)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
			return "", fmt.Errorf("unsupported special file at review path %q", RedactSecretLikeDiagnostic(path))
		}
	}
	rawStatus, err := gitOutput(sourceCtx, repoRoot, false, "diff", "--raw", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", head, "--")
	if err != nil {
		return "", fmt.Errorf("inspect tracked changes: %w", err)
	}
	for _, record := range splitNUL(rawStatus) {
		if strings.HasPrefix(record, ":") {
			fields := strings.Fields(record)
			if len(fields) >= 2 && (strings.TrimPrefix(fields[0], ":") == "160000" || fields[1] == "160000") {
				return "", errors.New("unsupported gitlink/submodule change in review capture")
			}
		}
	}
	remaining := maxCaptureBytes
	trackedPatch, err := gitLimitedOutput(sourceCtx, repoRoot, false, nil, remaining, "diff", "--binary", "--full-index", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", head, "--")
	if err != nil {
		return "", fmt.Errorf("capture tracked changes: %w", err)
	}
	remaining -= len(trackedPatch)
	if err := capture.apply(repoRoot, trackedPatch); err != nil {
		return "", err
	}
	untracked, err := gitOutput(sourceCtx, repoRoot, false, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return "", fmt.Errorf("enumerate untracked review paths: %w", err)
	}
	for _, path := range splitNUL(untracked) {
		if !filepath.IsLocal(path) {
			return "", fmt.Errorf("invalid untracked review path %q", RedactSecretLikeDiagnostic(path))
		}
		absolute := filepath.Join(repoRoot, filepath.FromSlash(path))
		if relative, relErr := filepath.Rel(capture.dir, absolute); relErr == nil && filepath.IsLocal(relative) {
			continue
		}
		info, err := os.Lstat(absolute)
		if err != nil {
			return "", fmt.Errorf("capture untracked path %q: %w", RedactSecretLikeDiagnostic(path), err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("unsupported special file or submodule at review path %q", RedactSecretLikeDiagnostic(path))
		}
		patch, err := gitLimitedOutput(sourceCtx, repoRoot, true, nil, remaining, "diff", "--no-index", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "--", "/dev/null", path)
		if err != nil {
			return "", fmt.Errorf("capture untracked path %q: %w", RedactSecretLikeDiagnostic(path), err)
		}
		remaining -= len(patch)
		if len(patch) == 0 {
			return "", fmt.Errorf("capture untracked path %q: empty addition patch", RedactSecretLikeDiagnostic(path))
		}
		if err := capture.apply(repoRoot, patch); err != nil {
			return "", fmt.Errorf("reconstruct untracked path %q: %w", RedactSecretLikeDiagnostic(path), err)
		}
	}
	raw, err := gitOutput(capture.ctx, repoRoot, false, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write private captured tree: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// Git omits FIFOs and other special files from --others. Detect such paths
// without following links, and honor the source repository's ignore rules.
func rejectSpecialFiles(ctx context.Context, repoRoot, captureDir string) error {
	return filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect review path: %w", walkErr)
		}
		if path == repoRoot {
			return nil
		}
		if path == captureDir && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.Name() == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			ignored, err := reviewPathIgnored(ctx, repoRoot, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			if ignored {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect review path: %w", err)
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ignored, err := reviewPathIgnored(ctx, repoRoot, rel)
		if err != nil {
			return err
		}
		if !ignored {
			return fmt.Errorf("unsupported special file at review path %q", RedactSecretLikeDiagnostic(rel))
		}
		return nil
	})
}

func reviewPathIgnored(ctx context.Context, repoRoot, path string) (bool, error) {
	ignored, err := gitOutput(ctx, repoRoot, true, "check-ignore", "--no-index", "--", path)
	if err != nil {
		return false, fmt.Errorf("check review path ignore rules: %w", err)
	}
	return len(ignored) > 0, nil
}

func (capture *reviewCapture) apply(repoRoot string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	if _, err := gitLimitedOutput(capture.ctx, repoRoot, false, patch, maxCaptureBytes, "apply", "--cached", "--whitespace=nowarn", "-"); err != nil {
		return fmt.Errorf("reconstruct captured review input: %w", err)
	}
	return nil
}

// gitLimitedOutput enforces an aggregate allowance as Git writes, before the
// whole patch can be materialized. The caller accounts across invocations.
func gitLimitedOutput(ctx context.Context, cwd string, acceptDiff bool, input []byte, limit int, args ...string) ([]byte, error) {
	commandArgs := gitCommandArgs(ctx, cwd, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = append(gitEnvironment(ctx), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1")
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out := &limitedGitBuffer{remaining: limit}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if out.exceeded {
		return nil, errors.New("review patch output exceeds 64 MiB; choose a smaller scope or supply an explicit review artifact")
	}
	if err == nil {
		return out.buf.Bytes(), nil
	}
	var exitErr *exec.ExitError
	if acceptDiff && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return out.buf.Bytes(), nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	return nil, errors.New(RedactSecretLikeDiagnostic(detail))
}

type limitedGitBuffer struct {
	buf       bytes.Buffer
	remaining int
	exceeded  bool
}

func (out *limitedGitBuffer) Write(p []byte) (int, error) {
	if len(p) > out.remaining {
		out.exceeded = true
		return 0, errors.New("review patch output limit exceeded")
	}
	out.remaining -= len(p)
	return out.buf.Write(p)
}

type capturedBlob struct {
	mode string
	id   string
}

func capturedTreeEntries(ctx context.Context, repoRoot, tree string) (map[string]capturedBlob, error) {
	raw, err := gitOutput(ctx, repoRoot, false, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, fmt.Errorf("inspect captured tree: %w", err)
	}
	entries := make(map[string]capturedBlob)
	for _, entry := range splitNUL(raw) {
		metadata, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || path == "" {
			return nil, errors.New("malformed captured tree entry")
		}
		entries[path] = capturedBlob{mode: fields[0], id: fields[2]}
	}
	return entries, nil
}

// selectedBlobs checks the total size of each distinct selected pre/post blob
// before reading any of their contents, including secrets and binary files.
func selectedBlobs(ctx context.Context, repoRoot string, files []changedFile, before, after map[string]capturedBlob) (map[string][]byte, error) {
	selected := make(map[string]struct{})
	for _, file := range files {
		for _, path := range file.paths() {
			for _, tree := range []map[string]capturedBlob{before, after} {
				entry, ok := tree[path]
				if !ok {
					continue
				}
				if entry.mode == "160000" {
					return nil, fmt.Errorf("unsupported gitlink/submodule at review path %q", RedactSecretLikeDiagnostic(path))
				}
				if entry.mode != "100644" && entry.mode != "100755" && entry.mode != "120000" {
					return nil, fmt.Errorf("unsupported Git file mode %s at review path %q", entry.mode, RedactSecretLikeDiagnostic(path))
				}
				selected[entry.id] = struct{}{}
			}
		}
	}
	var total int64
	for id := range selected {
		raw, err := gitOutput(ctx, repoRoot, false, "cat-file", "-s", id)
		if err != nil {
			return nil, fmt.Errorf("inspect selected review blob: %w", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || size < 0 {
			return nil, errors.New("invalid selected review blob size")
		}
		if size > int64(maxCaptureBytes)-total {
			return nil, errors.New("selected review content exceeds 64 MiB; choose a smaller scope or supply an explicit review artifact")
		}
		total += size
	}
	blobs := make(map[string][]byte, len(selected))
	for id := range selected {
		raw, err := gitOutput(ctx, repoRoot, false, "cat-file", "blob", id)
		if err != nil {
			return nil, fmt.Errorf("read selected review blob: %w", err)
		}
		blobs[id] = raw
	}
	return blobs, nil
}

// propagateCapturedTaint connects history to the captured tree, including
// uncertain delete/add transitions and ambiguous equal-blob copies.
func propagateCapturedTaint(paths map[string]bool, secretBlobs map[string]struct{}, files []changedFile, before, after map[string]capturedBlob) {
	var adds, deletes []string
	for _, file := range files {
		if file.sourcePath == "" && len(file.Status) != 0 {
			switch file.Status[0] {
			case 'A':
				adds = append(adds, file.Path)
			case 'D':
				deletes = append(deletes, file.Path)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		mark := func(path string) {
			if path != "" && !paths[path] {
				paths[path] = true
				changed = true
			}
		}
		for path, entry := range before {
			if IsSecretPath(path) || paths[path] {
				secretBlobs[entry.id] = struct{}{}
			}
		}
		for path, entry := range after {
			if IsSecretPath(path) || paths[path] {
				secretBlobs[entry.id] = struct{}{}
			}
		}
		for _, file := range files {
			if paths[file.Path] || paths[file.sourcePath] || IsSecretPath(file.Path) || IsSecretPath(file.sourcePath) {
				mark(file.Path)
				mark(file.sourcePath)
			}
			for _, path := range file.paths() {
				for _, tree := range []map[string]capturedBlob{before, after} {
					if _, ok := secretBlobs[tree[path].id]; ok {
						mark(path)
					}
				}
			}
		}
		for _, deleted := range deletes {
			for _, added := range adds {
				if paths[deleted] || paths[added] {
					mark(deleted)
					mark(added)
				}
			}
		}
	}
}
