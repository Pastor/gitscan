package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ghRepo — описание репозитория, как его отдаёт GitHub API.
type ghRepo struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Description   string   `json:"description"`
	Private       bool     `json:"private"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	HTMLURL       string   `json:"html_url"`
	CloneURL      string   `json:"clone_url"`
	SSHURL        string   `json:"ssh_url"`
	DefaultBranch string   `json:"default_branch"`
	Language      string   `json:"language"`
	Size          int64    `json:"size"` // КБ, по оценке GitHub
	Stars         int      `json:"stargazers_count"`
	Topics        []string `json:"topics"`
	Homepage      string   `json:"homepage"`
	CreatedAt     string   `json:"created_at"`
	PushedAt      string   `json:"pushed_at"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	License *struct {
		SPDXID string `json:"spdx_id"`
	} `json:"license"`
	Parent *struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		CloneURL string `json:"clone_url"`
	} `json:"parent"`
}

type githubProvider struct{ s *Source }

func (p githubProvider) GitUser() string { return "x-access-token" }

// api — адрес API: github.com или GitHub Enterprise (<url>/api/v3).
func (p githubProvider) api() string {
	if p.s.URL == "" {
		return "https://api.github.com"
	}
	return p.s.URL + "/api/v3"
}

func (p githubProvider) toRepo(g ghRepo) Repo {
	r := Repo{
		Source:        p.s.Name,
		ID:            strconv.FormatInt(g.ID, 10),
		Name:          g.Name,
		Path:          g.FullName,
		Owner:         g.Owner.Login,
		Mine:          strings.EqualFold(g.Owner.Login, p.s.User),
		Description:   strings.TrimSpace(g.Description),
		WebURL:        g.HTMLURL,
		CloneURL:      g.CloneURL,
		SSHURL:        g.SSHURL,
		DefaultBranch: g.DefaultBranch,
		Language:      g.Language,
		Homepage:      g.Homepage,
		Topics:        g.Topics,
		Visibility:    "public",
		Private:       g.Private,
		Fork:          g.Fork,
		Archived:      g.Archived,
		Size:          g.Size * 1024,
		Stars:         g.Stars,
		CreatedAt:     g.CreatedAt,
		PushedAt:      g.PushedAt,
	}
	if g.ID == 0 {
		r.ID = ""
	}
	if g.Private {
		r.Visibility = "private"
	}
	if g.License != nil && g.License.SPDXID != "" && g.License.SPDXID != "NOASSERTION" {
		r.License = g.License.SPDXID
	}
	if g.Parent != nil && g.Parent.FullName != "" {
		r.Parent = &Parent{Path: g.Parent.FullName, WebURL: g.Parent.HTMLURL, CloneURL: g.Parent.CloneURL}
	}
	return r
}

func (p githubProvider) get(url string, out interface{}) (*http.Response, error) {
	return p.getAs(p.s.Token, url, out)
}

func (p githubProvider) getAs(token, url string, out interface{}) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gitscan-sync/"+version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, body, err := httpDo(p.s.client, req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode == 401 {
		return resp, fmt.Errorf("GitHub API 401: токен недействителен или истёк")
	}
	if (resp.StatusCode == 403 || resp.StatusCode == 429) && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return resp, fmt.Errorf("GitHub API: исчерпан лимит запросов, сброс в %s (добавьте токен)",
			rateLimitReset(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, apiError("GitHub", resp, url, body)
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
func (p githubProvider) fetchPaged(token, base string) ([]ghRepo, error) {
	var all []ghRepo
	for page := 1; page <= 50; page++ {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		url := fmt.Sprintf("%s%sper_page=100&page=%d", base, sep, page)
		var chunk []ghRepo
		if _, err := p.getAs(token, url, &chunk); err != nil {
			return all, err
		}
		all = append(all, chunk...)
		if len(chunk) < 100 {
			break
		}
	}
	return all, nil
}

// List возвращает список репозиториев владельца.
// С токеном используется /user/repos (виден и приватный код), без токена — публичный список.
func (p githubProvider) List(cfg *Config) ([]Repo, error) {
	s := p.s
	if s.User == "" {
		return nil, fmt.Errorf("%s: не задан пользователь GitHub (user в конфиге или -user)", s.Name)
	}
	seen := map[string]bool{}
	var result []Repo

	add := func(list []ghRepo) {
		for _, g := range list {
			if !strings.EqualFold(g.Owner.Login, s.User) && !cfg.Member {
				continue
			}
			if seen[strings.ToLower(g.FullName)] {
				continue
			}
			seen[strings.ToLower(g.FullName)] = true
			result = append(result, p.toRepo(g))
		}
	}

	if s.Token != "" {
		affiliation := "owner"
		if cfg.Member {
			affiliation = "owner,collaborator,organization_member"
		}
		list, err := p.fetchPaged(s.Token, p.api()+"/user/repos?affiliation="+affiliation+"&visibility=all&sort=full_name")
		if err != nil {
			log.Printf("предупреждение: %s: не удалось получить приватный список (%v), пробую публичный", s.Name, err)
		} else {
			add(list)
		}
	}

	repoType := "owner"
	if cfg.Member {
		repoType = "all" // как на странице профиля: плюс репозитории, где пользователь участник
	}
	publicURL := p.api() + "/users/" + s.User + "/repos?type=" + repoType + "&sort=full_name"
	list, err := p.fetchPaged(s.Token, publicURL)
	if err != nil && s.Token != "" {
		// токен может быть ограничен по области видимости — пробуем анонимно
		if l2, err2 := p.fetchPaged("", publicURL); err2 == nil {
			list, err = l2, nil
			log.Printf("предупреждение: %s: токен отклонён GitHub, список получен анонимно (приватные репозитории недоступны)", s.Name)
		}
	}
	if err != nil && len(result) == 0 {
		return nil, err
	}
	add(list)

	s.attach(result)
	p.enrichForks(cfg, result)
	sortRepos(result)
	return result, nil
}

// Check показывает, откуда взят токен, и проверяет его на GitHub.
func (p githubProvider) Check(cfg *Config) {
	s := p.s
	printTokenSearch(cfg, s)
	if s.Token == "" {
		fmt.Println("\nТокен не найден — доступны только публичные репозитории,")
		fmt.Println("лимит GitHub API 60 запросов в час, форки на GitHub обновляться не будут.")
		if s.legacy {
			fmt.Printf("Проще всего: создайте файл %s со строкой\n  GITHUB_TOKEN=ghp_...\n",
				filepath.Join(cfg.Root, ".env"))
		}
		return
	}

	fmt.Printf("\nИспользуется: %s\n", maskToken(s.Token))
	fmt.Printf("Источник:     %s\n", s.TokenFrom)

	var who struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	resp, err := p.get(p.api()+"/user", &who)
	if err != nil {
		fmt.Printf("Проверка:     НЕ ПРОШЛА — %v\n", err)
		return
	}
	fmt.Printf("Проверка:     ок, GitHub видит вас как %s", who.Login)
	if who.Name != "" {
		fmt.Printf(" (%s)", who.Name)
	}
	fmt.Println()
	if sc := resp.Header.Get("X-OAuth-Scopes"); sc != "" {
		fmt.Printf("Права:        %s\n", sc)
		if !strings.Contains(sc, "repo") {
			fmt.Println("              ⚠ для приватных репозиториев и обновления форков нужен доступ repo")
		}
	} else {
		fmt.Println("Права:        fine-grained токен (проверьте: Contents — Read and write)")
	}
	if lim := resp.Header.Get("X-RateLimit-Limit"); lim != "" {
		fmt.Printf("Лимит API:    %s из %s запросов осталось\n",
			resp.Header.Get("X-RateLimit-Remaining"), lim)
	}
	if !strings.EqualFold(who.Login, s.User) {
		fmt.Printf("\n⚠ Токен принадлежит %s, а синхронизируется аккаунт %s.\n", who.Login, s.User)
		fmt.Printf("  Если это не нарочно — укажите -user %s\n", who.Login)
	}
}

// ---------- родители форков ----------

type parentInfo struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	HTMLURL  string `json:"html_url,omitempty"`
}

func parentsCachePath(cfg *Config, s *Source) string {
	return filepath.Join(s.stateDir(cfg), "parents.json")
}

func loadParents(cfg *Config, s *Source) map[string]parentInfo {
	out := map[string]parentInfo{}
	if b, err := os.ReadFile(parentsCachePath(cfg, s)); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveParents(cfg *Config, s *Source, m map[string]parentInfo) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(s.stateDir(cfg), 0o755)
	_ = os.WriteFile(parentsCachePath(cfg, s), b, 0o644)
}

// enrichForks дозапрашивает у GitHub родителя для каждого форка.
// Список репозиториев такой информации не содержит, поэтому нужен
// отдельный запрос — результат кэшируется в .sync/sources/<имя>/parents.json.
func (p githubProvider) enrichForks(cfg *Config, repos []Repo) {
	if !cfg.ForkSync {
		return
	}
	web := p.s.webBase()
	cache := loadParents(cfg, p.s)
	toParent := func(c parentInfo) *Parent {
		pp := &Parent{Path: c.FullName, WebURL: c.HTMLURL, CloneURL: c.CloneURL}
		if pp.WebURL == "" {
			pp.WebURL = web + "/" + c.FullName
		}
		if pp.CloneURL == "" {
			pp.CloneURL = web + "/" + c.FullName + ".git"
		}
		return pp
	}
	need := 0
	for i := range repos {
		r := &repos[i]
		if !r.Fork {
			continue
		}
		if c, ok := cache[r.Path]; ok && c.FullName != "" {
			r.Parent = toParent(c)
			continue
		}
		need++
	}
	if need == 0 {
		return
	}
	log.Printf("Уточняю upstream для %d форков...", need)
	fetched := 0
	for i := range repos {
		r := &repos[i]
		if !r.Fork || r.Parent != nil {
			continue
		}
		var full ghRepo
		if _, err := p.get(p.api()+"/repos/"+r.Path, &full); err != nil {
			if strings.Contains(err.Error(), "лимит запросов") {
				log.Printf("предупреждение: %v — синхронизация форков продолжится на следующем запуске", err)
				break
			}
			continue
		}
		if full.Parent != nil && full.Parent.FullName != "" {
			c := parentInfo{FullName: full.Parent.FullName, CloneURL: full.Parent.CloneURL, HTMLURL: full.Parent.HTMLURL}
			r.Parent = toParent(c)
			cache[r.Path] = c
			fetched++
		} else {
			cache[r.Path] = parentInfo{} // помним, что родителя нет
		}
	}
	if fetched > 0 {
		saveParents(cfg, p.s, cache)
		log.Printf("Найден upstream у %d форков", fetched)
	}
}
