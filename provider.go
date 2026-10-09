package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repo — репозиторий в нейтральном виде, не зависящем от провайдера.
type Repo struct {
	Source        string   `json:"source"`
	ID            string   `json:"id,omitempty"` // стабильный идентификатор у провайдера
	Name          string   `json:"name"`
	Path          string   `json:"path"`  // полный путь у провайдера: owner/name, group/sub/name
	Owner         string   `json:"owner"` // владелец или namespace
	Mine          bool     `json:"mine"`  // лежит в личном пространстве пользователя источника
	Description   string   `json:"description,omitempty"`
	WebURL        string   `json:"web_url,omitempty"`
	CloneURL      string   `json:"clone_url,omitempty"`
	SSHURL        string   `json:"ssh_url,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Language      string   `json:"language,omitempty"`
	License       string   `json:"license,omitempty"`
	Homepage      string   `json:"homepage,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	Visibility    string   `json:"visibility,omitempty"` // public | internal | private
	Private       bool     `json:"private"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	Empty         bool     `json:"empty,omitempty"`
	PendingDelete bool     `json:"pending_delete,omitempty"`
	Size          int64    `json:"size,omitempty"` // байт, по оценке сервера
	Stars         int      `json:"stars,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	PushedAt      string   `json:"pushed_at,omitempty"`
	Parent        *Parent  `json:"parent,omitempty"`

	src *Source
}

// Parent — исходный репозиторий форка.
type Parent struct {
	Path     string `json:"path"`
	WebURL   string `json:"web_url,omitempty"`
	CloneURL string `json:"clone_url,omitempty"`
}

// Key — уникальное имя репозитория в папке: <источник>/<путь>.
func (r Repo) Key() string {
	if r.Source == "" {
		return r.Path
	}
	return r.Source + "/" + r.Path
}

// URLFor возвращает адрес для клонирования с учётом выбранного протокола.
func (r Repo) URLFor(cfg *Config) string {
	if r.src.useSSH(cfg) && r.SSHURL != "" {
		return r.SSHURL
	}
	return r.CloneURL
}

// RepoDir — каталог репозитория: <repositories>/<источник>/<путь>.
func (c *Config) RepoDir(r Repo) string {
	return filepath.Join(c.Repos(), r.Source, filepath.FromSlash(r.Path))
}

// provider — то, что нужно знать синхронизации о конкретном хостинге.
type provider interface {
	List(cfg *Config) ([]Repo, error) // все доступные репозитории источника
	Check(cfg *Config)                // проверка токена для команды `token`
	GitUser() string                  // логин для HTTPS-аутентификации git по токену
}

func (s *Source) provider() provider {
	if s.Type == "gitlab" {
		return gitlabProvider{s}
	}
	return githubProvider{s}
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
		if cfg.Only != "" && !strings.Contains(strings.ToLower(r.Key()), strings.ToLower(cfg.Only)) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func sortRepos(repos []Repo) {
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i].Key()) < strings.ToLower(repos[j].Key())
	})
}

// interleave перемешивает очереди источников по кругу, чтобы ограничение
// потоков одного источника не задерживало остальные.
func interleave(lists [][]Repo) []Repo {
	var out []Repo
	for i := 0; ; i++ {
		added := false
		for _, l := range lists {
			if i < len(l) {
				out = append(out, l[i])
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

// ---------- кэш списка репозиториев ----------

func repoCachePath(cfg *Config, s *Source) string {
	return filepath.Join(s.stateDir(cfg), "repos.json")
}

// saveRepoCache сохраняет список источника, чтобы реестр и scan работали офлайн.
func saveRepoCache(cfg *Config, s *Source, repos []Repo) {
	if err := os.MkdirAll(s.stateDir(cfg), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(repoCachePath(cfg, s), b, 0o644)
}

func loadRepoCache(cfg *Config, s *Source) []Repo {
	b, err := os.ReadFile(repoCachePath(cfg, s))
	if err != nil {
		return nil
	}
	var repos []Repo
	if err := json.Unmarshal(b, &repos); err != nil {
		return nil
	}
	s.attach(repos)
	return repos
}

// ---------- HTTP ----------

// httpDo выполняет запрос; GET повторяет при 429 и временных ошибках сервера,
// соблюдая Retry-After.
func httpDo(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return resp, nil, err
		}
		retry := resp.StatusCode == 429 || resp.StatusCode == 502 ||
			resp.StatusCode == 503 || resp.StatusCode == 504
		if !retry || req.Method != "GET" || attempt >= 3 {
			return resp, body, nil
		}
		wait := time.Duration(1<<attempt) * 2 * time.Second
		if v, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && v >= 0 {
			wait = time.Duration(v) * time.Second
		}
		if wait > time.Minute {
			wait = time.Minute
		}
		time.Sleep(wait)
	}
}

func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func apiError(name string, resp *http.Response, url string, body []byte) error {
	return fmt.Errorf("%s API %d для %s: %s", name, resp.StatusCode, url, bodySnippet(body))
}
