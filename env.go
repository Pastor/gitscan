package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Имена переменных, в которых может лежать токен GitHub.
var tokenKeys = []string{
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"GITHUB_PAT",
	"GH_PAT",
	"GITHUB_ACCESS_TOKEN",
	"PASTOR_SYNC_TOKEN",
}

// parseDotenv читает файл в формате .env: KEY=VALUE, по одной паре на строку.
// Понимает комментарии (#), префикс export, одинарные и двойные кавычки.
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])

		switch {
		case len(val) >= 2 && val[0] == '"' && strings.HasSuffix(val, `"`):
			val = val[1 : len(val)-1]
			val = strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(val)
		case len(val) >= 2 && val[0] == '\'' && strings.HasSuffix(val, "'"):
			val = val[1 : len(val)-1]
		default:
			// комментарий в конце незакавыченного значения
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		if key != "" {
			out[key] = val
		}
	}
	return out, sc.Err()
}

// envFileCandidates — где искать .env, в порядке приоритета.
func envFileCandidates(cfg *Config) []string {
	var list []string
	if cfg.EnvFile != "" {
		list = append(list, cfg.EnvFile)
	}
	list = append(list,
		filepath.Join(cfg.Root, ".env"),
		filepath.Join(cfg.Root, syncDirName, ".env"),
	)
	if cwd, err := os.Getwd(); err == nil {
		list = append(list, filepath.Join(cwd, ".env"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		list = append(list,
			filepath.Join(home, ".env"),
			filepath.Join(home, ".config", "pastor-sync", ".env"),
		)
	}
	return dedupePaths(list)
}

func dedupePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// TokenSource — где нашёлся токен, для понятного сообщения в логе.
type TokenSource struct {
	Value string
	Where string
}

// FindToken ищет токен GitHub по всем поддерживаемым местам:
//  1. флаг -token
//  2. переменные окружения GITHUB_TOKEN / GH_TOKEN / ...
//  3. файлы .env (-env, <папка>/.env, <папка>/.sync/.env, ./.env, ~/.env)
//  4. файлы с «голым» токеном: <папка>/.sync/token, ~/.github_token
func FindToken(cfg *Config) TokenSource {
	if v := strings.TrimSpace(cfg.Token); v != "" {
		return TokenSource{v, "флаг -token"}
	}
	for _, k := range tokenKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return TokenSource{v, "переменная окружения " + k}
		}
	}
	for _, p := range envFileCandidates(cfg) {
		vars, err := parseDotenv(p)
		if err != nil {
			continue
		}
		for _, k := range tokenKeys {
			if v := strings.TrimSpace(vars[k]); v != "" {
				return TokenSource{v, p + " (" + k + ")"}
			}
		}
	}
	candidates := []string{filepath.Join(cfg.Root, syncDirName, "token")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".github_token"))
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return TokenSource{v, p}
		}
	}
	return TokenSource{}
}

// FindUser достаёт имя владельца из .env, если оно не задано явно.
func FindUser(cfg *Config) string {
	for _, p := range envFileCandidates(cfg) {
		vars, err := parseDotenv(p)
		if err != nil {
			continue
		}
		for _, k := range []string{"GITHUB_USER", "GH_USER", "GITHUB_OWNER"} {
			if v := strings.TrimSpace(vars[k]); v != "" {
				return v
			}
		}
	}
	return ""
}

// maskToken показывает токен так, чтобы его можно было опознать, но не увести.
func maskToken(t string) string {
	r := []rune(t)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-8) + string(r[len(r)-4:])
}
