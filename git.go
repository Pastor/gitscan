package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// git выполняет команду git в каталоге dir и возвращает stdout.
func git(cfg *Config, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = gitEnv(cfg)

	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() == context.DeadlineExceeded {
			msg = "превышен таймаут " + cfg.Timeout.String() + ": " + msg
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitLines — как git, но результат разбит на непустые строки.
func gitLines(cfg *Config, dir string, args ...string) ([]string, error) {
	out, err := git(cfg, dir, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

var askPassPath string

// gitEnv готовит окружение: отключает интерактивные запросы пароля и,
// если задан токен, подставляет его через GIT_ASKPASS (токен не попадает
// ни в командную строку, ни в .git/config).
func gitEnv(cfg *Config) []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=0",
		"LC_ALL=C",
	)
	if cfg.Token != "" && askPassPath != "" {
		env = append(env,
			"GIT_ASKPASS="+askPassPath,
			"PASTOR_SYNC_TOKEN="+cfg.Token,
		)
	}
	return env
}

// setupAskPass создаёт временный helper для передачи токена git-у.
func setupAskPass(cfg *Config) func() {
	if cfg.Token == "" || runtime.GOOS == "windows" {
		return func() {}
	}
	f, err := os.CreateTemp("", "pastor-askpass-*.sh")
	if err != nil {
		return func() {}
	}
	script := "#!/bin/sh\ncase \"$1\" in\n  Username*) echo \"x-access-token\" ;;\n  *) echo \"$PASTOR_SYNC_TOKEN\" ;;\nesac\n"
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return func() {}
	}
	_ = f.Close()
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		_ = os.Remove(f.Name())
		return func() {}
	}
	askPassPath = f.Name()
	return func() {
		_ = os.Remove(askPassPath)
		askPassPath = ""
	}
}

func isGitRepo(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	// .git может быть каталогом (обычный клон) или файлом (worktree/submodule)
	return st.IsDir() || st.Mode().IsRegular()
}

func hasSubmodules(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".gitmodules"))
	return err == nil
}

// isClean сообщает, что в рабочей копии нет незакоммиченных изменений.
func isClean(cfg *Config, dir string) bool {
	// --no-optional-locks: не трогаем .git при проверке (важно для сетевых и ro-томов)
	out, err := git(cfg, dir, "--no-optional-locks", "status", "--porcelain", "--untracked-files=no")
	return err == nil && out == ""
}

// currentBranch возвращает имя текущей ветки или "" при detached HEAD.
func currentBranch(cfg *Config, dir string) string {
	out, err := git(cfg, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// defaultBranch определяет ветку по умолчанию: сначала по origin/HEAD,
// затем по подсказке из API, затем по типичным именам.
func defaultBranch(cfg *Config, dir, hint string) string {
	if out, err := git(cfg, dir, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, "origin/")
	}
	if hint != "" {
		if _, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+hint); err == nil {
			return hint
		}
	}
	for _, b := range []string{"main", "master", "develop", "trunk"} {
		if _, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+b); err == nil {
			return b
		}
	}
	return currentBranch(cfg, dir)
}

// remoteBranches — список веток origin (без origin/HEAD).
func remoteBranches(cfg *Config, dir string) []string {
	lines, err := gitLines(cfg, dir, "for-each-ref", "--format=%(refname:strip=3)", "refs/remotes/origin")
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range lines {
		if b == "HEAD" || b == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// dirSize считает размер каталога на диске.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d Б", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), []string{"КБ", "МБ", "ГБ", "ТБ", "ПБ"}[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dм %02dс", int(d.Minutes()), int(d.Seconds())%60)
}
