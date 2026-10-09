package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitLab — минимальный GitLab API: /user, /projects (keyset через Link),
// /groups/:id/projects (offset через X-Next-Page).
type fakeGitLab struct {
	mu       sync.Mutex
	token    string
	projects []map[string]interface{}
	requests []string
}

func (f *fakeGitLab) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.RequestURI())
		projects := f.projects
		f.mu.Unlock()
		if r.Header.Get("PRIVATE-TOKEN") != f.token {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message":"401 Unauthorized"}`)
			return
		}
		switch {
		case r.URL.Path == "/api/v4/user":
			fmt.Fprint(w, `{"username":"pastor","name":"Pastor"}`)
		case r.URL.Path == "/api/v4/projects":
			if r.URL.Query().Get("membership") != "true" {
				t.Errorf("ожидался membership=true: %s", r.URL.RawQuery)
			}
			// по одному проекту на страницу, следующая — через Link
			after := r.URL.Query().Get("id_after")
			i := 0
			if after != "" {
				fmt.Sscanf(after, "%d", &i)
			}
			if i < len(projects) {
				if i+1 < len(projects) {
					w.Header().Set("Link", fmt.Sprintf(`<http://external.example/api/v4/projects?id_after=%d&membership=true>; rel="next"`, i+1))
				}
				_ = json.NewEncoder(w).Encode(projects[i : i+1])
				return
			}
			fmt.Fprint(w, `[]`)
		case strings.HasPrefix(r.URL.RawPath, "/api/v4/groups/infra%2Ftools/projects"):
			page := r.URL.Query().Get("page")
			if page == "" || page == "1" {
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode(projects[:1])
				return
			}
			_ = json.NewEncoder(w).Encode(projects[1:])
		default:
			t.Errorf("неожиданный запрос %s", r.URL.RequestURI())
			w.WriteHeader(404)
		}
	})
}

func project(id int, path, cloneURL string, extra map[string]interface{}) map[string]interface{} {
	ns := path[:strings.LastIndex(path, "/")]
	kind := "group"
	if ns == "pastor" {
		kind = "user"
	}
	p := map[string]interface{}{
		"id":                  id,
		"name":                filepath.Base(path),
		"path":                filepath.Base(path),
		"path_with_namespace": path,
		"default_branch":      "main",
		"visibility":          "internal",
		"web_url":             "https://git.example/" + path,
		"http_url_to_repo":    cloneURL,
		"ssh_url_to_repo":     "git@ssh.git.example:" + path + ".git",
		"namespace":           map[string]string{"kind": kind, "path": filepath.Base(ns), "full_path": ns},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func testConfig(t *testing.T, root string) *Config {
	t.Helper()
	return &Config{
		Root: root, ReposDir: defaultRepos, User: "Pastor", Jobs: 2,
		Forks: true, Member: true, Archived: true, Submodules: true,
		Timeout: time.Minute,
	}
}

func writeSources(t *testing.T, root string, sources ...*Source) {
	t.Helper()
	if err := writeConfig(filepath.Join(root, configName), &fileConfig{Sources: sources}); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabListPaginationAndMapping(t *testing.T) {
	fake := &fakeGitLab{token: "glpat-test"}
	fake.projects = []map[string]interface{}{
		project(1, "pastor/notes", "https://git.example/pastor/notes.git", nil),
		project(2, "infra/tools/deploy", "https://git.example/infra/tools/deploy.git", map[string]interface{}{
			"forked_from_project": map[string]string{"path_with_namespace": "upstream/deploy", "web_url": "https://git.example/upstream/deploy"},
			"archived":            true,
		}),
		project(3, "infra/wiki-only", "https://git.example/infra/wiki-only.git", map[string]interface{}{
			"repository_access_level": "disabled",
		}),
	}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	root := t.TempDir()
	t.Setenv("GITLAB_TOKEN_WORK", "glpat-test")
	writeSources(t, root, &Source{Name: "work", Type: "gitlab", URL: srv.URL + "/"})
	cfg := testConfig(t, root)
	if err := loadSources(cfg, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	s := cfg.source("work")
	if s.URL != srv.URL {
		t.Fatalf("завершающий слэш в url не убран: %q", s.URL)
	}
	if s.TokenFrom != "переменная окружения GITLAB_TOKEN_WORK" {
		t.Fatalf("токен взят из %q", s.TokenFrom)
	}

	repos, err := s.provider().List(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("ожидалось 2 проекта (третий без репозитория), получено %d: %+v", len(repos), repos)
	}
	byPath := map[string]Repo{}
	for _, r := range repos {
		byPath[r.Path] = r
	}
	notes, deploy := byPath["pastor/notes"], byPath["infra/tools/deploy"]
	if !notes.Mine || notes.Fork || notes.Key() != "work/pastor/notes" || notes.Visibility != "internal" || !notes.Private {
		t.Errorf("pastor/notes разобран неверно: %+v", notes)
	}
	if deploy.Mine || !deploy.Fork || !deploy.Archived || deploy.Parent == nil || deploy.Parent.Path != "upstream/deploy" {
		t.Errorf("infra/tools/deploy разобран неверно: %+v", deploy)
	}
	if deploy.Owner != "infra/tools" || deploy.ID != "2" || deploy.src != s {
		t.Errorf("owner/id/src: %+v", deploy)
	}
	// ссылка next указывала на чужой хост — запрос должен был уйти на наш
	for _, rq := range fake.requests {
		if strings.Contains(rq, "external.example") {
			t.Errorf("запрос ушёл по чужой ссылке: %s", rq)
		}
	}
	if len(fake.requests) != 4 { // /user + 3 страницы
		t.Errorf("запросов %d: %v", len(fake.requests), fake.requests)
	}
	if !containsFold(s.hosts, "ssh.git.example") {
		t.Errorf("ssh-хост не запомнен: %v", s.hosts)
	}
}

func TestGitLabGroupsAndBadToken(t *testing.T) {
	fake := &fakeGitLab{token: "right"}
	fake.projects = []map[string]interface{}{
		project(10, "infra/tools/a", "https://git.example/infra/tools/a.git", nil),
		project(11, "infra/tools/sub/b", "https://git.example/infra/tools/sub/b.git", nil),
	}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	root := t.TempDir()
	cfg := testConfig(t, root)
	s := &Source{Name: "corp", Type: "gitlab", URL: srv.URL, Groups: []string{"infra/tools"}, Token: "right"}
	s.client = newHTTPClient(s)
	repos, err := s.provider().List(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos[1].Path != "infra/tools/sub/b" {
		t.Fatalf("группа с подгруппами: %+v", repos)
	}

	s.Token = "wrong"
	if _, err := s.provider().List(cfg); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("ожидалась ошибка 401, получено %v", err)
	}
	s.Token = ""
	if _, err := s.provider().List(cfg); err == nil || !strings.Contains(err.Error(), "GITLAB_TOKEN_CORP") {
		t.Fatalf("без токена должна быть подсказка про переменную: %v", err)
	}
}

func TestParseAndMatchRemote(t *testing.T) {
	cases := []struct{ in, host, path string }{
		{"https://github.com/Pastor/gitscan.git", "github.com", "Pastor/gitscan"},
		{"https://x-access-token:secret@github.com/Pastor/gitscan", "github.com", "Pastor/gitscan"},
		{"git@github.com:Pastor/gitscan.git", "github.com", "Pastor/gitscan"},
		{"ssh://git@git.corp:2222/infra/tools/app.git", "git.corp", "infra/tools/app"},
		{"/local/path/repo.git", "", ""},
	}
	for _, c := range cases {
		h, p := parseRemote(c.in)
		if h != c.host || p != c.path {
			t.Errorf("parseRemote(%q) = %q, %q; ожидалось %q, %q", c.in, h, p, c.host, c.path)
		}
	}

	gh := &Source{Name: "github", Type: "github"}
	gl := &Source{Name: "work", Type: "gitlab", URL: "https://git.corp/gitlab"}
	for _, s := range []*Source{gh, gl} {
		s.addHost(s.webBase())
	}
	cfg := &Config{Sources: []*Source{gh, gl}}
	if s, p := cfg.matchRemote("git@github.com:Pastor/x.git"); s != gh || p != "Pastor/x" {
		t.Errorf("github: %v %q", s, p)
	}
	if s, p := cfg.matchRemote("https://git.corp/gitlab/infra/app.git"); s != gl || p != "infra/app" {
		t.Errorf("gitlab с префиксом пути: %v %q", s, p)
	}
	if s, _ := cfg.matchRemote("https://bitbucket.org/a/b.git"); s != nil {
		t.Errorf("чужой хост сопоставлен с %s", s.Name)
	}
}

func TestAskPassOnlyForOwnHosts(t *testing.T) {
	script := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(script, []byte(askPassScript), 0o700); err != nil {
		t.Fatal(err)
	}
	ask := func(prompt string) string {
		cmd := exec.Command("sh", script, prompt)
		cmd.Env = append(os.Environ(), "GITSCAN_GIT_USER=oauth2", "GITSCAN_GIT_TOKEN=glpat-secret",
			"GITSCAN_GIT_HOSTS=git.corp ssh.git.corp")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	cases := map[string]string{
		"Username for 'https://git.corp': ":             "oauth2",
		"Password for 'https://oauth2@git.corp': ":      "glpat-secret",
		"Password for 'https://oauth2@git.corp:8443': ": "glpat-secret",
		"Username for 'https://github.com': ":           "",
		"Password for 'https://oauth2@git.corp.evil': ": "",
		"Password for 'https://evilgit.corp': ":         "",
	}
	for prompt, want := range cases {
		if got := ask(prompt); got != want {
			t.Errorf("%q → %q, ожидалось %q", prompt, got, want)
		}
	}
}

// ---------- сквозной сценарий на локальных репозиториях ----------

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo создаёт bare-репозиторий с веткой main, веткой dev и тегом.
func bareRepo(t *testing.T, base, name string) string {
	t.Helper()
	work := filepath.Join(base, "work-"+name)
	bare := filepath.Join(base, name+".git")
	run(t, base, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# "+name+"\n\nТестовый репозиторий "+name+" для проверки синхронизации.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "init")
	run(t, work, "tag", "v1.0")
	run(t, work, "branch", "dev")
	run(t, base, "clone", "-q", "--bare", work, bare)
	return bare
}

func TestSyncGitLabEndToEnd(t *testing.T) {
	base := t.TempDir()
	app := bareRepo(t, base, "app")
	lib := bareRepo(t, base, "lib")
	flat := bareRepo(t, base, "flat")

	fake := &fakeGitLab{token: "glpat-e2e"}
	fake.projects = []map[string]interface{}{
		project(1, "platform/backend/app", "file://"+app, nil),
		project(2, "pastor/lib", "file://"+lib, map[string]interface{}{"empty_repo": false}),
	}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	root := filepath.Join(base, "mirror")
	t.Setenv("GITLAB_TOKEN_WORK", "glpat-e2e")
	gh := &Source{Name: "github", Type: "github", Disabled: true}
	writeSources(t, root, gh, &Source{Name: "work", Type: "gitlab", URL: srv.URL, Jobs: 1})

	// репозиторий старой плоской раскладки с origin на GitHub
	old := filepath.Join(root, defaultRepos, "flat")
	run(t, base, "clone", "-q", "file://"+flat, old)
	run(t, old, "remote", "set-url", "origin", "https://github.com/Pastor/flat.git")

	cfg := testConfig(t, root)
	if err := loadSources(cfg, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	cmdSync(cfg)

	repos := filepath.Join(root, defaultRepos)
	appDir := filepath.Join(repos, "work", "platform", "backend", "app")
	for _, d := range []string{appDir, filepath.Join(repos, "work", "pastor", "lib"), filepath.Join(repos, "github", "Pastor", "flat")} {
		if !isGitRepo(d) {
			t.Fatalf("нет репозитория %s", d)
		}
	}
	if isGitRepo(old) {
		t.Fatal("плоская копия не перенесена")
	}
	if b := run(t, appDir, "branch", "--list", "dev"); !strings.Contains(b, "dev") {
		t.Errorf("локальная ветка dev не создана")
	}
	if tags := run(t, appDir, "tag"); tags != "v1.0" {
		t.Errorf("теги: %q", tags)
	}

	// реестр
	var reg Registry
	b, err := os.ReadFile(filepath.Join(root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Schema != registrySchema || reg.Total != 3 {
		t.Fatalf("реестр: schema %d, total %d", reg.Schema, reg.Total)
	}
	kinds := map[string]string{}
	for _, e := range reg.Repos {
		kinds[e.Key] = e.Kind + "/" + e.Provider
		if e.Key == "work/platform/backend/app" && (e.URL != "https://git.example/platform/backend/app" || e.TagCount != 1 || e.BranchCount != 2) {
			t.Errorf("запись app: %+v", e)
		}
	}
	want := map[string]string{
		"work/platform/backend/app": "member/gitlab",
		"work/pastor/lib":           "own/gitlab",
		"github/Pastor/flat":        "own/github",
	}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s: %q, ожидалось %q (все: %v)", k, kinds[k], v, kinds)
		}
	}
	md, _ := os.ReadFile(filepath.Join(root, "REGISTRY.md"))
	if !strings.Contains(string(md), "## work") || !strings.Contains(string(md), "Проекты групп") {
		t.Errorf("в REGISTRY.md нет раздела источника work")
	}

	// проект перенесли в другую группу — копия переезжает, а не клонируется заново
	marker := filepath.Join(appDir, "local-note.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.projects[0] = project(1, "platform/core/app", "file://"+app, nil)
	fake.mu.Unlock()
	cfg2 := testConfig(t, root)
	cfg2.NoRegistry = true
	if err := loadSources(cfg2, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	cmdSync(cfg2)
	moved := filepath.Join(repos, "work", "platform", "core", "app")
	if _, err := os.Stat(filepath.Join(moved, "local-note.txt")); err != nil {
		t.Fatalf("копия не переехала вслед за проектом: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repos, "work", "platform", "backend")); !os.IsNotExist(err) {
		t.Errorf("опустевший каталог группы не убран")
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	fc, exists, err := readConfig("gitscan.example.json")
	if err != nil || !exists {
		t.Fatalf("gitscan.example.json: %v", err)
	}
	if err := validateSources(fc.Sources); err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, s := range fc.Sources {
		types[s.Type]++
	}
	if types["github"] == 0 || types["gitlab"] == 0 {
		t.Fatalf("в примере должны быть и github, и gitlab: %v", types)
	}
}
