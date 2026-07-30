package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stageFiles copies overlay, copy_files, and rig workdir into the pod
// via the init container, then signals it to exit.
func stageFiles(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, ctrlCity string, warn io.Writer) error {
	// Wait for init container to be running (up to 60s).
	if err := waitForInitContainer(ctx, ops, podName, 60*time.Second); err != nil {
		return err
	}

	// Wait for exec endpoint to be ready. The kubelet reports Running
	// before the CRI exec handler is set up, so we poll a trivial command.
	if err := waitForExecReady(ctx, ops, podName, 120*time.Second); err != nil {
		return err
	}

	// Copy rig work_dir into the pod.
	podWorkDir := projectedPodWorkDirForController(cfg, ctrlCity)
	if cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if err := copyDirToPod(ctx, ops, podName, "stage", cfg.WorkDir, podWorkDir); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging workdir %s to %s: %v\n", cfg.WorkDir, podWorkDir, err) //nolint:errcheck
		}
	}

	if err := stageProviderOverlaysToPod(ctx, ops, podName, cfg, podWorkDir, warn); err != nil {
		return err
	}

	// Copy each copy_files entry.
	for _, entry := range cfg.CopyFiles {
		dst := projectedPodCopyDestination(entry, cfg.WorkDir, ctrlCity, podWorkDir)
		if err := copyToPod(ctx, ops, podName, "stage", entry.Src, dst); err != nil {
			fmt.Fprintf(warn, "gc: warning: staging copy_file %s → %s: %v\n", entry.Src, dst, err) //nolint:errcheck
		}
	}

	// Mirror .gc/ into city volume when GC_CITY differs from work_dir.
	if ctrlCity != "" && ctrlCity != cfg.WorkDir {
		_, _ = ops.execInPod(ctx, podName, "stage",
			[]string{"sh", "-c", "cp -a /workspace/.gc /city-stage/.gc 2>/dev/null || true"}, nil)
	}

	// Signal init container to exit.
	_, err := ops.execInPod(ctx, podName, "stage",
		[]string{"touch", "/workspace/.gc-ready"}, nil)
	return err
}

func projectedPodCopyDestination(entry runtime.CopyEntry, ctrlWorkDir, ctrlCity, podWorkDir string) string {
	if ctrlWorkDir != "" && ctrlCity != "" && entry.Src != "" {
		relWorkDir, workErr := filepath.Rel(ctrlWorkDir, entry.Src)
		relCity, cityErr := filepath.Rel(ctrlCity, entry.Src)
		relWorkDir = filepath.ToSlash(relWorkDir)
		relCity = filepath.ToSlash(relCity)
		if workErr == nil && cityErr == nil &&
			relWorkDir != ".." && !strings.HasPrefix(relWorkDir, "../") &&
			path.Clean(relCity) == path.Clean(filepath.ToSlash(entry.RelDst)) {
			return path.Join(podWorkDir, relWorkDir)
		}
	}

	if entry.RelDst == "" {
		return "/workspace"
	}
	return path.Join("/workspace", filepath.ToSlash(entry.RelDst))
}

func stageProviderOverlaysToPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, podWorkDir string, warn io.Writer) error {
	if len(cfg.PackOverlayDirs) == 0 && cfg.OverlayDir == "" {
		return nil
	}
	if podWorkDir == "" {
		podWorkDir = "/workspace"
	}

	stageDir, err := os.MkdirTemp("", "gc-k8s-overlays-")
	if err != nil {
		return fmt.Errorf("preparing provider overlays: %w", err)
	}
	defer os.RemoveAll(stageDir) //nolint:errcheck

	seedExistingInstructions(cfg.WorkDir, stageDir, warn)
	providers := runtime.EffectiveOverlayProviderNames(cfg)
	for _, od := range cfg.PackOverlayDirs {
		if err := stageProviderOverlay(od, stageDir, providers, "pack overlay", warn); err != nil {
			return err
		}
	}
	if cfg.OverlayDir != "" {
		if err := stageProviderOverlay(cfg.OverlayDir, stageDir, providers, "overlay", warn); err != nil {
			return err
		}
	}
	if err := copyDirToPod(ctx, ops, podName, "stage", stageDir, podWorkDir); err != nil {
		return fmt.Errorf("staging provider overlays: %w", err)
	}
	return nil
}

func seedExistingInstructions(workDir, stageDir string, warn io.Writer) {
	if workDir == "" {
		return
	}
	src := filepath.Join(workDir, "AGENTS.md")
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return
	} else if err != nil {
		fmt.Fprintf(warn, "gc: warning: checking existing AGENTS.md: %v\n", err) //nolint:errcheck
		return
	}
	if err := runtime.StagePath(src, filepath.Join(stageDir, "AGENTS.md")); err != nil {
		fmt.Fprintf(warn, "gc: warning: preserving existing AGENTS.md: %v\n", err) //nolint:errcheck
	}
}

func stageProviderOverlay(srcDir, dstDir string, providers []string, label string, warn io.Writer) error {
	var warnings bytes.Buffer
	if err := runtime.StageProviderOverlayDir(srcDir, dstDir, providers, &warnings); err != nil {
		return fmt.Errorf("staging %s %s: %w", label, srcDir, err)
	}
	if warnings.Len() > 0 {
		fmt.Fprintf(warn, "gc: warning: staging %s %s: %s\n", label, srcDir, strings.TrimSpace(warnings.String())) //nolint:errcheck
	}
	return nil
}

// waitForInitContainer waits for the init container to be running.
func waitForInitContainer(ctx context.Context, ops k8sOps, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := ops.getPod(ctx, podName)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if len(pod.Status.InitContainerStatuses) > 0 {
			state := pod.Status.InitContainerStatuses[0].State
			if state.Running != nil {
				return nil
			}
			if state.Terminated != nil {
				// Already finished (shouldn't happen since it waits for sentinel).
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("init container not running in pod %s after %s", podName, timeout)
}

// waitForExecReady polls exec with a trivial command until it succeeds.
// The kubelet reports a container as Running before the CRI exec handler
// (SPDY) is fully set up, causing "container not found" errors if we
// exec too early. This is especially common on K3s with containerd.
func waitForExecReady(ctx context.Context, ops k8sOps, podName string, timeout time.Duration) error {
	const container = "stage"

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.execInPod(ctx, podName, container, []string{"true"}, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		if err := ctx.Err(); err != nil {
			return err
		}

		delay := 500 * time.Millisecond
		if remaining := time.Until(deadline); remaining < delay {
			delay = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("exec not ready in %s/%s after %s: %w", podName, container, timeout, lastErr)
	}
	return fmt.Errorf("exec not ready in %s/%s after %s", podName, container, timeout)
}

// copyDirToPod copies a local directory into the pod via tar-based exec.
func copyDirToPod(ctx context.Context, ops k8sOps, podName, container, srcDir, dstDir string) error {
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		return nil // skip silently if not a directory
	}

	// Create destination directory in the pod.
	_, _ = ops.execInPod(ctx, podName, container,
		[]string{"mkdir", "-p", dstDir}, nil)

	return streamArchiveToPod(ctx, ops, podName, container, srcDir, dstDir, func(w io.Writer) error {
		return tarDir(srcDir, w)
	})
}

// copyToPod copies a single file or directory to the pod.
func copyToPod(ctx context.Context, ops k8sOps, podName, container, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return nil // skip silently if source doesn't exist
	}

	if info.IsDir() {
		return copyDirToPod(ctx, ops, podName, container, src, dst)
	}

	// Single file: create parent dir, write via tar.
	parentDir := filepath.Dir(dst)
	_, _ = ops.execInPod(ctx, podName, container,
		[]string{"mkdir", "-p", parentDir}, nil)

	return streamArchiveToPod(ctx, ops, podName, container, src, parentDir, func(w io.Writer) error {
		return tarFile(src, info, filepath.Base(dst), w)
	})
}

// streamArchiveToPod creates a tar archive concurrently with extraction in the
// pod. Work directories can be many gigabytes; buffering the complete archive
// in a bytes.Buffer made supervisor memory scale with repository size and could
// OOM the shared town container. io.Pipe keeps the resident staging footprint
// bounded by the transport's in-flight data instead.
func streamArchiveToPod(
	ctx context.Context,
	ops k8sOps,
	podName, container, src, dstDir string,
	writeArchive func(io.Writer) error,
) error {
	pr, pw := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		err := writeArchive(pw)
		_ = pw.CloseWithError(err)
		archiveErr <- err
	}()

	_, execErr := ops.execInPod(ctx, podName, container,
		[]string{"tar", "xf", "-", "-C", dstDir}, pr)
	// If exec stops consuming stdin early, unblock the archive producer before
	// waiting for it. The buffered channel also prevents a completed producer
	// from depending on this goroutine receiving immediately.
	_ = pr.CloseWithError(execErr)
	tarErr := <-archiveErr

	if tarErr != nil && (execErr == nil || !errors.Is(tarErr, io.ErrClosedPipe)) {
		return fmt.Errorf("creating tar of %s: %w", src, tarErr)
	}
	return execErr
}

// tarDir creates a tar archive of a directory's contents.
func tarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	return writeTarTree(tw, dir, ".", make(map[string]bool))
}

func writeTarTree(tw *tar.Writer, path, rel string, activeDirs map[string]bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	// Dereference symlinks: use the resolved path for both stat and open to
	// avoid TOCTOU issues if the symlink target changes. Directory symlinks
	// require explicit traversal because filepath.Walk does not follow them.
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil // skip broken symlinks
		}
		info, err = os.Stat(resolved)
		if err != nil {
			return nil
		}
		path = resolved
	}

	if info.IsDir() && rel != "." && skipStagingCacheDir(rel) {
		return nil
	}

	// Skip sockets and other special file types unsupported by tar.
	if info.Mode()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice) != 0 {
		return nil
	}

	if rel != "." {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			// Limit copy to declared header size to avoid "write too long" if
			// the file grew between stat and read (e.g., events.jsonl).
			_, copyErr := io.CopyN(tw, f, header.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
	}

	resolvedDir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if activeDirs[resolvedDir] {
		return nil
	}
	activeDirs[resolvedDir] = true
	defer delete(activeDirs, resolvedDir)

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRel := entry.Name()
		if rel != "." {
			childRel = filepath.Join(rel, entry.Name())
		}
		if err := writeTarTree(
			tw,
			filepath.Join(path, entry.Name()),
			childRel,
			activeDirs,
		); err != nil {
			return err
		}
	}
	return nil
}

// skipStagingCacheDir omits local, reproducible caches that must not be copied
// into an isolated worker. Besides wasting network and startup time, these can
// dwarf the repository itself (multi-gigabyte node_modules/.next/.devenv
// trees). Claude's nested worktrees are also independent checkouts, not agent
// configuration; the rest of .claude remains staged.
func skipStagingCacheDir(rel string) bool {
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == ".claude/worktrees" {
		return true
	}
	switch filepath.Base(clean) {
	case ".devenv", ".direnv", ".next", "node_modules":
		return true
	default:
		return false
	}
}

// tarFile creates a tar archive containing a single file.
func tarFile(path string, info os.FileInfo, name string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}
