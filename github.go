package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Repo — описание репозитория, как его отдаёт GitHub API.
type Repo struct {
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Description   string   `json:"description"`
	Private       bool     `json:"private"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	Disabled      bool     `json:"disabled"`
	HTMLURL       string   `json:"html_url"`
	CloneURL      string   `json:"clone_url"`
	SSHURL        string   `json:"ssh_url"`
	DefaultBranch string   `json:"default_branch"`
	Language      string   `json:"language"`
	Size          int64    `json:"size"` // КБ, по оценке GitHub
	Stars         int      `json:"stargazers_count"`
	ForksCount    int      `json:"forks_count"`
	OpenIssues    int      `json:"open_issues_count"`
	Topics        []string `json:"topics"`
	Homepage      string   `json:"homepage"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	PushedAt      string   `json:"pushed_at"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	License *struct {
		SPDXID string `json:"spdx_id"`
		Name   string `json:"name"`
	} `json:"license"`
	Parent *struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"parent"`
}

// URLFor возвращает адрес для клонирования с учётом выбранного протокола.
func (r Repo) URLFor(cfg *Config) string {
	if cfg.SSH {
		return r.SSHURL
	}
	return r.CloneURL
}

const apiBase = "https://api.github.com"

func apiGet(cfg *Config, url string, out interface{}) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gitscan-sync/"+version)
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode == 401 {
		return resp, fmt.Errorf("GitHub API 401: токен недействителен или истёк")
	}
	if resp.StatusCode == 403 && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return resp, fmt.Errorf("GitHub API: исчерпан лимит запросов, сброс в %s (добавьте токен)",
			rateLimitReset(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return resp, fmt.Errorf("GitHub API %d для %s: %s", resp.StatusCode, url, snippet)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, fmt.Errorf("не удалось разобрать ответ %s: %w", url, err)
		}
	}
	return resp, nil
}

func rateLimitReset(resp *http.Response) string {
	v := resp.Header.Get("X-RateLimit-Reset")
	if v == "" {
		return "неизвестно"
	}
	var ts int64
	if _, err := fmt.Sscanf(v, "%d", &ts); err != nil {
		return v
	}
	return time.Unix(ts, 0).Format("15:04:05")
}

// fetchPaged забирает все страницы одного эндпоинта.
func fetchPaged(cfg *Config, base string) ([]Repo, error) {
	var all []Repo
	for page := 1; page <= 50; page++ {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		url := fmt.Sprintf("%s%sper_page=100&page=%d", base, sep, page)
		var chunk []Repo
		if _, err := apiGet(cfg, url, &chunk); err != nil {
			return all, err
		}
		all = append(all, chunk...)
		if len(chunk) < 100 {
			break
		}
	}
	return all, nil
}

// FetchRepos возвращает список репозиториев владельца.
// С токеном используется /user/repos (виден и приватный код), без токена — публичный список.
func FetchRepos(cfg *Config) ([]Repo, error) {
	seen := map[string]bool{}
	var result []Repo

	add := func(list []Repo) {
		for _, r := range list {
			if !strings.EqualFold(r.Owner.Login, cfg.User) && !cfg.Member {
				continue
			}
			if seen[strings.ToLower(r.FullName)] {
				continue
			}
			seen[strings.ToLower(r.FullName)] = true
			result = append(result, r)
		}
	}

	if cfg.Token != "" {
		affiliation := "owner"
		if cfg.Member {
			affiliation = "owner,collaborator,organization_member"
		}
		list, err := fetchPaged(cfg, apiBase+"/user/repos?affiliation="+affiliation+"&visibility=all&sort=full_name")
		if err != nil {
			log.Printf("предупреждение: не удалось получить приватный список (%v), пробую публичный", err)
		} else {
			add(list)
		}
	}

	repoType := "owner"
	if cfg.Member {
		repoType = "all" // как на странице профиля: плюс репозитории, где пользователь участник
	}
	publicURL := apiBase + "/users/" + cfg.User + "/repos?type=" + repoType + "&sort=full_name"
	list, err := fetchPaged(cfg, publicURL)
	if err != nil && cfg.Token != "" {
		// токен может быть ограничен по области видимости — пробуем анонимно
		anon := *cfg
		anon.Token = ""
		if l2, err2 := fetchPaged(&anon, publicURL); err2 == nil {
			list, err = l2, nil
			log.Printf("предупреждение: токен отклонён GitHub, список получен анонимно (приватные репозитории недоступны)")
		}
	}
	if err != nil && len(result) == 0 {
		return nil, err
	}
	add(list)

	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

// DedupeByDir убирает конфликт имён каталогов: если два репозитория разных
// владельцев называются одинаково, оставляем принадлежащий пользователю.
func DedupeByDir(cfg *Config, repos []Repo) []Repo {
	byName := map[string]int{}
	var out []Repo
	for _, r := range repos {
		key := strings.ToLower(r.Name)
		if i, ok := byName[key]; ok {
			keepNew := strings.EqualFold(r.Owner.Login, cfg.User) &&
				!strings.EqualFold(out[i].Owner.Login, cfg.User)
			dropped := r.FullName
			if keepNew {
				dropped = out[i].FullName
				out[i] = r
			}
			log.Printf("предупреждение: имя каталога %q занято, %s пропущен", r.Name, dropped)
			continue
		}
		byName[key] = len(out)
		out = append(out, r)
	}
	return out
}

// Filter отбрасывает форки/архивы/несовпадающие по -only, если так задано.
func Filter(cfg *Config, repos []Repo) []Repo {
	var out []Repo
	for _, r := range repos {
		if r.Fork && !cfg.Forks {
			continue
		}
		if r.Archived && !cfg.Archived {
			continue
		}
		if cfg.Only != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(cfg.Only)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// saveRepoCache сохраняет ответ API, чтобы генератор реестра мог работать офлайн.
func saveRepoCache(cfg *Config, repos []Repo) {
	dir := filepath.Join(cfg.Root, syncDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "repos.json"), b, 0o644)
}

func loadRepoCache(cfg *Config) []Repo {
	b, err := os.ReadFile(filepath.Join(cfg.Root, syncDirName, "repos.json"))
	if err != nil {
		return nil
	}
	var repos []Repo
	if err := json.Unmarshal(b, &repos); err != nil {
		return nil
	}
	return repos
}
