package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// glProject — проект, как его отдаёт GitLab API v4.
type glProject struct {
	ID                    int64    `json:"id"`
	Name                  string   `json:"name"`
	Path                  string   `json:"path"`
	PathWithNamespace     string   `json:"path_with_namespace"`
	Description           *string  `json:"description"`
	DefaultBranch         string   `json:"default_branch"`
	Visibility            string   `json:"visibility"`
	Archived              bool     `json:"archived"`
	EmptyRepo             bool     `json:"empty_repo"`
	WebURL                string   `json:"web_url"`
	HTTPURLToRepo         string   `json:"http_url_to_repo"`
	SSHURLToRepo          string   `json:"ssh_url_to_repo"`
	Topics                []string `json:"topics"`
	TagList               []string `json:"tag_list"` // до GitLab 14.0
	StarCount             int      `json:"star_count"`
	CreatedAt             string   `json:"created_at"`
	LastActivityAt        string   `json:"last_activity_at"`
	MarkedForDeletionAt   *string  `json:"marked_for_deletion_at"`
	MarkedForDeletionOn   *string  `json:"marked_for_deletion_on"`
	RepositoryAccessLevel string   `json:"repository_access_level"`
	Namespace             struct {
		Kind     string `json:"kind"` // user | group
		Path     string `json:"path"`
		FullPath string `json:"full_path"`
	} `json:"namespace"`
	ForkedFrom *struct {
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
		HTTPURLToRepo     string `json:"http_url_to_repo"`
	} `json:"forked_from_project"`
}

type glUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	IsAdmin  bool   `json:"is_admin"`
}

type gitlabProvider struct{ s *Source }

// GitUser — для персональных токенов GitLab подходит любой логин, принято oauth2.
func (p gitlabProvider) GitUser() string { return "oauth2" }

func (p gitlabProvider) api() string { return p.s.URL + "/api/v4" }

func (p gitlabProvider) get(rawurl string, out interface{}) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gitscan-sync/"+version)
	if p.s.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", p.s.Token)
	}
	resp, body, err := httpDo(p.s.client, req)
	if err != nil {
		return resp, fmt.Errorf("%s: %v", p.s.Name, err)
	}
	switch {
	case resp.StatusCode == 401:
		return resp, fmt.Errorf("%s: GitLab API 401 — токен недействителен, истёк или отозван", p.s.Name)
	case resp.StatusCode == 403:
		return resp, fmt.Errorf("%s: GitLab API 403 для %s — у токена нет нужных прав (read_api): %s",
			p.s.Name, rawurl, bodySnippet(body))
	case resp.StatusCode == 429:
		return resp, fmt.Errorf("%s: GitLab API — превышен лимит запросов, сброс в %s",
			p.s.Name, resp.Header.Get("RateLimit-ResetTime"))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return resp, apiError(p.s.Name+": GitLab", resp, rawurl, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, fmt.Errorf("%s: не удалось разобрать ответ %s: %w", p.s.Name, rawurl, err)
		}
	}
	return resp, nil
}

// projects забирает все страницы списка проектов. Следует за Link: rel="next"
// (так работает и keyset-, и offset-пагинация), иначе — за X-Next-Page.
func (p gitlabProvider) projects(first string) ([]glProject, error) {
	var all []glProject
	next := first
	for i := 0; next != "" && i < 100000; i++ {
		var chunk []glProject
		resp, err := p.get(next, &chunk)
		if err != nil {
			return all, err
		}
		all = append(all, chunk...)
		if len(chunk) == 0 {
			break
		}
		next = p.nextPage(resp, next)
	}
	return all, nil
}

func (p gitlabProvider) nextPage(resp *http.Response, cur string) string {
	if n := linkNext(resp.Header.Get("Link")); n != "" {
		// GitLab строит ссылку по external_url; если он отличается от адреса
		// в конфиге (прокси, другой порт), идём на наш адрес — туда и токен
		u, err := url.Parse(n)
		if err != nil {
			return ""
		}
		base, _ := url.Parse(p.s.URL)
		if !strings.EqualFold(u.Host, base.Host) || u.Scheme != base.Scheme {
			c, _ := url.Parse(cur)
			c.RawQuery = u.RawQuery
			return c.String()
		}
		return n
	}
	if np := resp.Header.Get("X-Next-Page"); np != "" {
		u, err := url.Parse(cur)
		if err != nil {
			return ""
		}
		q := u.Query()
		q.Set("page", np)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return ""
}

// linkNext достаёт адрес rel="next" из заголовка Link.
func linkNext(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(segs[0]), "<>")
			}
		}
	}
	return ""
}

func (p gitlabProvider) me() (glUser, error) {
	var u glUser
	_, err := p.get(p.api()+"/user", &u)
	return u, err
}

// List возвращает проекты инстанса: по умолчанию все, где пользователь участник.
func (p gitlabProvider) List(cfg *Config) ([]Repo, error) {
	s := p.s
	if s.Token == "" {
		return nil, fmt.Errorf("%s: для GitLab нужен токен с правами read_api и read_repository "+
			"(`%s source add` или переменная %s)", s.Name, toolName, defaultTokenKey(s))
	}
	me, err := p.me()
	if err != nil {
		return nil, err
	}
	s.User = me.Username

	var raw []glProject
	if len(s.Groups) > 0 {
		for _, g := range s.Groups {
			q := url.Values{}
			q.Set("per_page", "100")
			q.Set("include_subgroups", "true")
			q.Set("order_by", "id")
			q.Set("sort", "asc")
			list, err := p.projects(p.api() + "/groups/" + url.PathEscape(strings.Trim(g, "/")) + "/projects?" + q.Encode())
			if err != nil {
				return nil, fmt.Errorf("группа %s: %w", g, err)
			}
			raw = append(raw, list...)
		}
	} else {
		q := url.Values{}
		q.Set("per_page", "100")
		q.Set("order_by", "id")
		q.Set("sort", "asc")
		q.Set("pagination", "keyset")
		switch s.Scope {
		case "owned":
			q.Set("owned", "true")
		case "all":
			// всё, что видно токену; у администратора — весь инстанс
		default:
			q.Set("membership", "true")
		}
		raw, err = p.projects(p.api() + "/projects?" + q.Encode())
		if err != nil {
			return nil, err
		}
	}

	seen := map[int64]bool{}
	var result []Repo
	noRepo := 0
	for _, g := range raw {
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		if g.RepositoryAccessLevel == "disabled" {
			noRepo++
			continue
		}
		result = append(result, p.toRepo(g, me.Username))
	}
	if noRepo > 0 {
		log.Printf("%s: пропущено проектов без репозитория (выключен в настройках): %d", s.Name, noRepo)
	}
	s.attach(result)
	sortRepos(result)
	return result, nil
}

func (p gitlabProvider) toRepo(g glProject, username string) Repo {
	r := Repo{
		Source:        p.s.Name,
		ID:            strconv.FormatInt(g.ID, 10),
		Name:          g.Path,
		Path:          g.PathWithNamespace,
		Owner:         g.Namespace.FullPath,
		Mine:          g.Namespace.Kind == "user" && strings.EqualFold(g.Namespace.Path, username),
		WebURL:        g.WebURL,
		CloneURL:      g.HTTPURLToRepo,
		SSHURL:        g.SSHURLToRepo,
		DefaultBranch: g.DefaultBranch,
		Topics:        g.Topics,
		Visibility:    g.Visibility,
		Private:       g.Visibility != "public",
		Fork:          g.ForkedFrom != nil,
		Archived:      g.Archived,
		Empty:         g.EmptyRepo,
		PendingDelete: g.MarkedForDeletionAt != nil || g.MarkedForDeletionOn != nil,
		Stars:         g.StarCount,
		CreatedAt:     g.CreatedAt,
		PushedAt:      g.LastActivityAt,
	}
	if r.Name == "" {
		r.Name = g.Name
	}
	if r.Owner == "" {
		if i := strings.LastIndex(r.Path, "/"); i > 0 {
			r.Owner = r.Path[:i]
		}
	}
	if len(r.Topics) == 0 {
		r.Topics = g.TagList
	}
	if g.Description != nil {
		r.Description = strings.TrimSpace(*g.Description)
	}
	if g.ForkedFrom != nil {
		r.Parent = &Parent{
			Path:     g.ForkedFrom.PathWithNamespace,
			WebURL:   g.ForkedFrom.WebURL,
			CloneURL: g.ForkedFrom.HTTPURLToRepo,
		}
	}
	return r
}

// Check проверяет токен на инстансе GitLab.
func (p gitlabProvider) Check(cfg *Config) {
	s := p.s
	printTokenSearch(cfg, s)
	if s.Token == "" {
		fmt.Printf("\nТокен не найден. Без него GitLab-источник не синхронизируется.\n")
		fmt.Printf("Добавьте: %s source add -name %s -url %s -token glpat-...\n", toolName, s.Name, s.URL)
		return
	}
	fmt.Printf("\nИспользуется: %s\n", maskToken(s.Token))
	fmt.Printf("Источник:     %s\n", s.TokenFrom)

	me, err := p.me()
	if err != nil {
		fmt.Printf("Проверка:     НЕ ПРОШЛА — %v\n", err)
		return
	}
	fmt.Printf("Проверка:     ок, %s видит вас как %s", s.URL, me.Username)
	if me.Name != "" {
		fmt.Printf(" (%s)", me.Name)
	}
	if me.IsAdmin {
		fmt.Printf(", администратор")
	}
	fmt.Println()

	var ver struct {
		Version string `json:"version"`
	}
	if _, err := p.get(p.api()+"/version", &ver); err == nil && ver.Version != "" {
		fmt.Printf("GitLab:       %s\n", ver.Version)
	}

	var self struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *string  `json:"expires_at"`
	}
	resp, err := p.get(p.api()+"/personal_access_tokens/self", &self)
	if err == nil {
		fmt.Printf("Права:        %s\n", strings.Join(self.Scopes, ", "))
		has := map[string]bool{}
		for _, sc := range self.Scopes {
			has[sc] = true
		}
		if !has["api"] && !has["read_api"] {
			fmt.Println("              ⚠ для списка проектов нужно read_api")
		}
		if !has["api"] && !has["read_repository"] && !has["write_repository"] {
			fmt.Println("              ⚠ для клонирования нужно read_repository")
		}
		if self.ExpiresAt != nil && *self.ExpiresAt != "" {
			fmt.Printf("Действует до: %s\n", *self.ExpiresAt)
		}
	} else {
		fmt.Println("Права:        не удалось узнать (старый GitLab или не персональный токен)")
	}
	if resp != nil {
		if lim := resp.Header.Get("RateLimit-Limit"); lim != "" {
			fmt.Printf("Лимит API:    %s из %s запросов осталось\n", resp.Header.Get("RateLimit-Remaining"), lim)
		}
	}
	if s.Insecure {
		fmt.Println("\n⚠ Проверка TLS-сертификата выключена (insecure) — лучше указать ca_file.")
	}
}
