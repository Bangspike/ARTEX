// Package reanalysis owns task input snapshots and evidence files.
package reanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot accepts a workspace-relative input and copies regular files only.
// The manifest digest binds names and content for directories; files use their
// byte SHA-256. No dependency directories are silently excluded.
func Snapshot(workspace, relative, destination string) (string, string, error) {
	if relative == "" || filepath.IsAbs(relative) || !filepath.IsLocal(relative) {
		return "", "", fmt.Errorf("input_path 必须是工作空间内的相对路径")
	}
	base, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", err
	}
	source := filepath.Join(base, relative)
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", err
	}
	if real != source || !strings.HasPrefix(real, base+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("输入材料不能使用符号链接或工作空间根目录")
	}
	target := filepath.Join(destination, filepath.Base(source))
	if workspaceAbs, e := filepath.Abs(workspace); e == nil {
		if rel, e := filepath.Rel(workspaceAbs, target); e == nil && filepath.IsLocal(rel) {
			target = filepath.Join(base, rel)
		}
	}
	if rel, err := filepath.Rel(source, target); err == nil && filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("输入目录不能包含分析输出目录，请选择具体材料")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	manifest := sha256.New()
	var total int64
	files := 0
	err = filepath.WalkDir(source, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := target
		if rel != "." {
			dst = filepath.Join(target, rel)
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("材料包含符号链接: %s", rel)
		}
		if e.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("材料包含非普通文件: %s", rel)
		}
		total += info.Size()
		files++
		if total > 512<<20 || files > 20000 {
			return fmt.Errorf("第一版材料上限为 512 MiB / 20000 文件")
		}
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		inside, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		in, err := root.Open(inside)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, info.Size()+1))
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if n != info.Size() {
			return fmt.Errorf("材料复制期间发生变化: %s", rel)
		}
		fmt.Fprintf(manifest, "%s\x00%x\n", filepath.ToSlash(rel), h.Sum(nil))
		return nil
	})
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		hash, err := HashFile(target)
		return target, hash, err
	}
	if files == 0 {
		return "", "", fmt.Errorf("材料目录为空")
	}
	return target, hex.EncodeToString(manifest.Sum(nil)), nil
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteExclusive never replaces prior evidence, including after partial failure.
func WriteExclusive(path string, b []byte) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

func AllowedTool(name string) bool {
	switch name {
	case "binary_session", "open_binary", "close_binary", "binary_overview", "search_procedures", "list_procedures", "search_strings", "analyze_function", "get_call_graph", "trace_feature", "analyze_javascript_application", "inspect_analysis_view", "trace_application_feature", "export_evidence_bundle":
		return true
	}
	return false
}
