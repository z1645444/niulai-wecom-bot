package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// envFileMu 串行化对 .env 文件的读-改-写，避免并发写回相互覆盖
var envFileMu sync.Mutex

// UpdateEnvFile 把 key=value 写回 .env 风格的配置文件：
// 已存在的同名行（含 export 前缀）就地替换为规范形式 KEY=value，
// 原有 export 前缀与未加引号值后的 " # 注释" 予以保留，其余行原样不动；
// 键不存在时追加到文件末尾。文件不存在时不创建、直接返回 nil，
// 以便纯环境变量部署时不产生多余文件。
// 写入通过同目录临时文件 + rename 原子完成，并保留原文件权限。
func UpdateEnvFile(path, key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("value for %s must not contain newlines", key)
	}

	envFileMu.Lock()
	defer envFileMu.Unlock()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	sep := "\n"
	if strings.Contains(string(data), "\r\n") {
		sep = "\r\n"
	}
	lines := strings.Split(string(data), sep)

	replaced := false
	for i, line := range lines {
		name, ok := envLineKey(line)
		if !ok || name != key {
			continue
		}
		// 只替换第一次出现，重复行原样保留
		lines[i] = envLinePrefix(line) + key + "=" + value + envLineComment(line)
		replaced = true
		break
	}
	if !replaced {
		// 文件以换行结尾时 Split 会留下末尾空串：新行插到它之前，
		// 保持文件仍以换行结尾；否则直接追加，沿用文件原本的无换行结尾
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = slices.Insert(lines, n-1, key+"="+value)
		} else {
			lines = append(lines, key+"="+value)
		}
	}

	if err := writeFileAtomic(path, []byte(strings.Join(lines, sep))); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// envLineKey 解析一行 env 配置中的键名；空行、注释行与不含 "=" 的行返回 false
func envLineKey(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", false
	}
	// 仅当 export 后跟空白时视为前缀，避免误伤 "exporter=1" 之类的键
	if rest, ok := strings.CutPrefix(s, "export"); ok && len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		s = strings.TrimLeft(rest, " \t")
	}
	name, _, ok := strings.Cut(s, "=")
	if !ok {
		return "", false
	}
	if name = strings.TrimSpace(name); name == "" {
		return "", false
	}
	return name, true
}

// envLinePrefix 返回该行原有的 "export " 前缀（有的话），用于替换时保留写法
func envLinePrefix(line string) string {
	s := strings.TrimSpace(line)
	if rest, ok := strings.CutPrefix(s, "export"); ok && len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		return "export "
	}
	return ""
}

// envLineComment 返回行尾 " # 注释" 部分（含前导空格），替换值时原样保留。
// dotenv 约定只有未加引号的值中空白后的 # 才是注释；值被引号包裹时不做
// 解析，直接放弃保留，整行重写
func envLineComment(line string) string {
	_, rest, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || rest[0] == '"' || rest[0] == '\'' {
		return ""
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] == '#' && (rest[i-1] == ' ' || rest[i-1] == '\t') {
			return " " + strings.TrimSpace(rest[i:])
		}
	}
	return ""
}

// writeFileAtomic 通过同目录临时文件 + rename 原子写入，避免写一半留下
// 损坏的配置；文件权限沿用原文件，原文件不存在时退回 0600
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	perm := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
