package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source — один источник репозиториев: аккаунт GitHub или инстанс GitLab.
type Source struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`                 // github | gitlab
	URL       string   `json:"url,omitempty"`        // адрес инстанса; для github.com не нужен
	User      string   `json:"user,omitempty"`       // GitHub: чьи репозитории; GitLab: определяется по токену
	TokenEnv  string   `json:"token_env,omitempty"`  // переменная (окружение или .env) с токеном
	TokenFile string   `json:"token_file,omitempty"` // файл с токеном
	Scope     string   `json:"scope,omitempty"`      // GitLab: membership (по умолчанию) | owned | all
	Groups    []string `json:"groups,omitempty"`     // GitLab: только эти группы (с подгруппами)
	CAFile    string   `json:"ca_file,omitempty"`    // дополнительный корневой сертификат (PEM)
	Insecure  bool     `json:"insecure,omitempty"`   // не проверять TLS-сертификат
	SSH       *bool    `json:"ssh,omitempty"`        // клонировать по ssh (иначе как флаг -ssh)
	Jobs      int      `json:"jobs,omitempty"`       // предел параллельных операций с этим сервером
	Disabled  bool     `json:"disabled,omitempty"`

	Token     string `json:"-"`
	TokenFrom string `json:"-"`

	legacy bool          // основной GitHub-источник: флаги -user/-token и старые места поиска токена
	hosts  []string      // хосты, которым можно отдавать токен
	bases  []string      // scheme://host[:port]/ для настроек http.<url>.* в git
	client *http.Client  // HTTP-клиент с учётом CA и insecure
	sem    chan struct{} // ограничение параллельности по Jobs
}

const configName = "gitscan.json"

type fileConfig struct {
	Sources []*Source `json:"sources"`
}

var reSourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ConfigPath — путь к файлу с описанием источников.
func (c *Config) ConfigPath() string {
	if c.ConfigFile != "" {
		if abs, err := filepath.Abs(expandHome(c.ConfigFile)); err == nil {
			return abs
		}
		return c.ConfigFile
	}
	return filepath.Join(c.Root, configName)
}

// readConfig читает файл источников. Если файла нет — один GitHub-источник,
// как в версиях до 2.0.
func readConfig(path string) (*fileConfig, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &fileConfig{Sources: []*Source{{Name: "github", Type: "github"}}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, true, fmt.Errorf("%s: %v", path, err)
	}
	return &fc, true, nil
}

func writeConfig(path string, fc *fileConfig) error {
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func validateSources(list []*Source) error {
	seen := map[string]bool{}
	for _, s := range list {
		s.Type = strings.ToLower(strings.TrimSpace(s.Type))
		s.URL = strings.TrimRight(strings.TrimSpace(s.URL), "/")
		switch {
		case !reSourceName.MatchString(s.Name):
			return fmt.Errorf("недопустимое имя источника %q: только латиница, цифры, . _ -", s.Name)
		case seen[strings.ToLower(s.Name)]:
			return fmt.Errorf("источник %q описан дважды", s.Name)
		case s.Type != "github" && s.Type != "gitlab":
			return fmt.Errorf("источник %s: неизвестный тип %q (github или gitlab)", s.Name, s.Type)
		case s.Type == "gitlab" && s.URL == "":
			return fmt.Errorf("источник %s: для GitLab нужен url", s.Name)
		}
		if s.URL != "" {
			u, err := url.Parse(s.URL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return fmt.Errorf("источник %s: url должен быть вида https://host[/путь], а не %q", s.Name, s.URL)
			}
		}
		switch s.Scope {
		case "", "membership", "owned", "all":
		default:
			return fmt.Errorf("источник %s: scope может быть membership, owned или all", s.Name)
		}
		seen[strings.ToLower(s.Name)] = true
	}
	return nil
}

// loadSources читает конфиг, подставляет флаги и находит токены.
func loadSources(cfg *Config, explicit map[string]bool) error {
	fc, _, err := readConfig(cfg.ConfigPath())
	if err != nil {
		return err
	}
	if err := validateSources(fc.Sources); err != nil {
		return err
	}
	cfg.Sources = fc.Sources
	for _, s := range cfg.Sources {
		if s.Type == "github" && s.URL == "" {
			s.legacy = true
			if explicit["user"] || s.User == "" {
				s.User = cfg.User
			}
			break
		}
	}
	for _, s := range cfg.Sources {
		t := resolveToken(cfg, s)
		s.Token, s.TokenFrom = t.Value, t.Where
		s.client = newHTTPClient(s)
		s.addHost(s.webBase())
		if s.Jobs > 0 {
			s.sem = make(chan struct{}, s.Jobs)
		}
	}
	if cfg.SourceFilter != "" {
		for _, n := range splitList(cfg.SourceFilter) {
			if cfg.source(n) == nil {
				return fmt.Errorf("нет источника %q (есть: %s)", n, strings.Join(cfg.sourceNames(), ", "))
			}
		}
	}
	return nil
}

func (c *Config) source(name string) *Source {
	for _, s := range c.Sources {
		if strings.EqualFold(s.Name, name) {
			return s
		}
	}
	return nil
}

func (c *Config) sourceNames() []string {
	var out []string
	for _, s := range c.Sources {
		out = append(out, s.Name)
	}
	return out
}

// Active — источники, с которыми работаем в этом запуске (не выключены и подходят под -source).
func (c *Config) Active() []*Source {
	want := map[string]bool{}
	for _, n := range splitList(c.SourceFilter) {
		want[strings.ToLower(n)] = true
	}
	var out []*Source
	for _, s := range c.Sources {
		if len(want) > 0 {
			if want[strings.ToLower(s.Name)] {
				out = append(out, s)
			}
			continue
		}
		if !s.Disabled {
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) sourceAllowed(name string) bool {
	if c.SourceFilter == "" {
		return true
	}
	for _, n := range splitList(c.SourceFilter) {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- свойства источника ----------

func (s *Source) kindTitle() string {
	if s.Type == "gitlab" {
		return "GitLab"
	}
	return "GitHub"
}

// webBase — адрес веб-интерфейса без завершающего слэша.
func (s *Source) webBase() string {
	if s.URL != "" {
		return s.URL
	}
	return "https://github.com"
}

// Display — короткое описание для лога.
func (s *Source) Display() string {
	d := s.webBase()
	if s.User != "" {
		d += ", пользователь " + s.User
	}
	return d
}

func (s *Source) stateDir(cfg *Config) string {
	return filepath.Join(cfg.Root, syncDirName, "sources", s.Name)
}

func (s *Source) useSSH(cfg *Config) bool {
	if s != nil && s.SSH != nil {
		return *s.SSH
	}
	return cfg.SSH
}

// pathPrefix — путь, под которым GitLab опубликован (https://host/gitlab → "gitlab").
func (s *Source) pathPrefix() string {
	u, err := url.Parse(s.webBase())
	if err != nil {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

// addHost запоминает хост из адреса репозитория: токен можно отдавать только ему,
// а TLS-настройки git нужно применять к нему же.
func (s *Source) addHost(raw string) {
	host, _ := parseRemote(raw)
	if host != "" && !containsFold(s.hosts, host) {
		s.hosts = append(s.hosts, host)
	}
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		base := u.Scheme + "://" + u.Host + "/"
		if !containsFold(s.bases, base) {
			s.bases = append(s.bases, base)
		}
	}
}

// attach связывает репозитории с источником.
func (s *Source) attach(repos []Repo) {
	for i := range repos {
		repos[i].src = s
		repos[i].Source = s.Name
		s.addHost(repos[i].CloneURL)
		s.addHost(repos[i].SSHURL)
	}
}

// gitArgs — параметры git, действующие только для адресов этого источника.
func (s *Source) gitArgs() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, b := range s.bases {
		if s.CAFile != "" {
			out = append(out, "-c", "http."+b+".sslCAInfo="+expandHome(s.CAFile))
		}
		if s.Insecure {
			out = append(out, "-c", "http."+b+".sslVerify=false")
		}
		if s.Token != "" {
			// токен источника важнее сохранённого в связке ключей,
			// и в связку ключей он попадать не должен
			out = append(out, "-c", "credential."+b+".helper=")
		}
	}
	return out
}

func newHTTPClient(s *Source) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if s.CAFile != "" || s.Insecure {
		tc := &tls.Config{InsecureSkipVerify: s.Insecure}
		if s.CAFile != "" {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if pem, err := os.ReadFile(expandHome(s.CAFile)); err != nil {
				log.Printf("предупреждение: %s: не читается ca_file: %v", s.Name, err)
			} else if !pool.AppendCertsFromPEM(pem) {
				log.Printf("предупреждение: %s: в %s нет сертификатов PEM", s.Name, s.CAFile)
			}
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// ---------- токены ----------

// defaultTokenKey — имя переменной по умолчанию: GITLAB_TOKEN_WORK для источника work.
func defaultTokenKey(s *Source) string {
	name := strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(s.Name, "_"))
	return strings.ToUpper(s.Type) + "_TOKEN_" + name
}

func tokenFilePath(cfg *Config, s *Source) string {
	return filepath.Join(cfg.Root, syncDirName, "tokens", s.Name)
}

// lookupVar ищет переменную в окружении, затем в файлах .env.
func lookupVar(cfg *Config, key string) TokenSource {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return TokenSource{v, "переменная окружения " + key}
	}
	for _, p := range envFileCandidates(cfg) {
		if vars, err := parseDotenv(p); err == nil {
			if v := strings.TrimSpace(vars[key]); v != "" {
				return TokenSource{v, p + " (" + key + ")"}
			}
		}
	}
	return TokenSource{}
}

func readTokenFile(p string) TokenSource {
	b, err := os.ReadFile(p)
	if err != nil {
		return TokenSource{}
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return TokenSource{v, p}
	}
	return TokenSource{}
}

// resolveToken ищет токен источника:
//  1. флаг -token (только основной GitHub-источник)
//  2. token_env из конфига — окружение, затем .env
//  3. token_file из конфига
//  4. <папка>/.sync/tokens/<имя> (туда пишет `source add -token`)
//  5. основной GitHub-источник — старые места (GITHUB_TOKEN, .env, ~/.github_token...),
//     остальные — переменная вида GITLAB_TOKEN_<ИМЯ>
func resolveToken(cfg *Config, s *Source) TokenSource {
	if s.legacy && strings.TrimSpace(cfg.Token) != "" {
		return TokenSource{strings.TrimSpace(cfg.Token), "флаг -token"}
	}
	if s.TokenEnv != "" {
		if t := lookupVar(cfg, s.TokenEnv); t.Value != "" {
			return t
		}
	}
	if s.TokenFile != "" {
		if t := readTokenFile(expandHome(s.TokenFile)); t.Value != "" {
			return t
		}
	}
	if t := readTokenFile(tokenFilePath(cfg, s)); t.Value != "" {
		return t
	}
	if s.legacy {
		return FindToken(cfg)
	}
	return lookupVar(cfg, defaultTokenKey(s))
}

// printTokenSearch показывает, где искался токен источника.
func printTokenSearch(cfg *Config, s *Source) {
	fmt.Println("Где искался токен:")
	if s.TokenEnv != "" {
		fmt.Printf("  token_env    %s\n", s.TokenEnv)
	}
	if s.TokenFile != "" {
		fmt.Printf("  token_file   %s\n", expandHome(s.TokenFile))
	}
	fmt.Printf("  файл         %s\n", tokenFilePath(cfg, s))
	if !s.legacy {
		fmt.Printf("  переменная   %s (окружение и .env)\n", defaultTokenKey(s))
		return
	}
	for i, k := range tokenKeys {
		mark := "—"
		if os.Getenv(k) != "" {
			mark = "есть"
		}
		fmt.Printf("  %d. переменная окружения %-22s %s\n", i+1, k, mark)
	}
	for _, p := range envFileCandidates(cfg) {
		state := "нет файла"
		if vars, err := parseDotenv(p); err == nil {
			state = "есть файл, токена в нём нет"
			for _, k := range tokenKeys {
				if strings.TrimSpace(vars[k]) != "" {
					state = "токен найден (" + k + ")"
					break
				}
			}
		}
		fmt.Printf("     .env %-52s %s\n", p, state)
	}
}

// ---------- адреса репозиториев ----------

// parseRemote разбирает адрес git-репозитория: https://host/a/b.git,
// ssh://git@host:2222/a/b.git, git@host:a/b.git. Возвращает хост без порта
// и путь без .git.
func parseRemote(raw string) (host, path string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", ""
		}
		host, path = u.Hostname(), u.Path
	} else if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		h := raw[:i]
		if j := strings.LastIndex(h, "@"); j >= 0 {
			h = h[j+1:]
		}
		host, path = h, raw[i+1:]
	} else {
		return "", ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return strings.ToLower(host), path
}

// matchRemote определяет источник и путь репозитория по адресу origin.
func (c *Config) matchRemote(raw string) (*Source, string) {
	host, p := parseRemote(raw)
	if host == "" || p == "" {
		return nil, ""
	}
	for _, s := range c.Sources {
		if !containsFold(s.hosts, host) {
			continue
		}
		rel := p
		if pre := s.pathPrefix(); pre != "" && strings.HasPrefix(rel, pre+"/") {
			rel = rel[len(pre)+1:]
		}
		if rel != "" {
			return s, rel
		}
	}
	return nil, ""
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// ---------- перемещение каталогов ----------

// moveDir переносит каталог, создавая родителей. Через промежуточное имя,
// чтобы работали перенос внутрь самого себя (repositories/github →
// repositories/github/Pastor/github) и смена регистра на macOS.
func moveDir(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if ti, err := os.Stat(to); err == nil && !os.SameFile(fi, ti) {
		return fmt.Errorf("%s уже существует", to)
	}
	tmp := from + ".gitscan-moving"
	if err := os.Rename(from, tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		_ = os.Rename(tmp, from)
		return err
	}
	if err := os.Rename(tmp, to); err != nil {
		_ = os.Rename(tmp, from)
		return err
	}
	return nil
}

// removeEmptyParents убирает опустевшие каталоги групп вверх до stop.
func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// migrateLayout переносит репозитории старой плоской раскладки
// (repositories/<имя>) в repositories/<источник>/<путь> по адресу origin.
func migrateLayout(cfg *Config) {
	migrateLegacyCache(cfg)
	for _, s := range cfg.Sources {
		loadRepoCache(cfg, s) // хосты ssh и клонирования из прошлого списка
	}
	entries, err := os.ReadDir(cfg.Repos())
	if err != nil {
		return
	}
	moved := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "__") {
			continue
		}
		dir := filepath.Join(cfg.Repos(), name)
		if !isGitRepo(dir) {
			continue
		}
		origin, err := git(cfg, dir, "remote", "get-url", "origin")
		if err != nil {
			log.Printf("предупреждение: у %s нет origin — источник не определить, оставлен на месте", name)
			continue
		}
		s, p := cfg.matchRemote(origin)
		if s == nil {
			log.Printf("предупреждение: %s (%s) не относится ни к одному источнику, оставлен на месте", name, origin)
			continue
		}
		dst := filepath.Join(cfg.Repos(), s.Name, filepath.FromSlash(p))
		if cfg.DryRun {
			log.Printf("[план] перенести %s → %s/%s", name, s.Name, p)
			continue
		}
		if err := moveDir(dir, dst); err != nil {
			log.Printf("предупреждение: %s не перенесён в %s/%s: %v", name, s.Name, p, err)
			continue
		}
		moved++
	}
	if moved > 0 {
		log.Printf("Раскладка обновлена: %d репозиториев перенесено в <источник>/<путь>", moved)
	}
}

// migrateLegacyCache переносит кэш родителей форков версии 1.x
// в каталог основного GitHub-источника.
func migrateLegacyCache(cfg *Config) {
	for _, s := range cfg.Sources {
		if !s.legacy {
			continue
		}
		old := filepath.Join(cfg.Root, syncDirName, "parents.json")
		dst := filepath.Join(s.stateDir(cfg), "parents.json")
		if _, err := os.Stat(old); err != nil {
			return
		}
		if _, err := os.Stat(dst); err == nil {
			return
		}
		if os.MkdirAll(s.stateDir(cfg), 0o755) == nil {
			_ = os.Rename(old, dst)
		}
		return
	}
}

// applyMoves следит за переименованиями: у репозитория стабильный ID, поэтому
// если путь на сервере сменился (переименование, перенос в другую группу),
// локальная копия переезжает вслед за ним, а не клонируется заново.
func applyMoves(cfg *Config, s *Source, repos []Repo) {
	statePath := filepath.Join(s.stateDir(cfg), "paths.json")
	old := map[string]string{}
	if b, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	root := filepath.Join(cfg.Repos(), s.Name)
	for _, r := range repos {
		prev, ok := old[r.ID]
		if r.ID == "" || !ok || prev == r.Path {
			continue
		}
		from := filepath.Join(root, filepath.FromSlash(prev))
		if !isGitRepo(from) {
			continue
		}
		if cfg.DryRun {
			log.Printf("[план] переместить %s/%s → %s", s.Name, prev, r.Key())
			continue
		}
		if err := moveDir(from, cfg.RepoDir(r)); err != nil {
			log.Printf("предупреждение: %s/%s переименован на сервере в %s, но перенести не удалось: %v",
				s.Name, prev, r.Path, err)
			continue
		}
		removeEmptyParents(filepath.Dir(from), root)
		log.Printf("Перемещён вслед за сервером: %s/%s → %s", s.Name, prev, r.Key())
	}
	if cfg.DryRun {
		return
	}
	cur := map[string]string{}
	for _, r := range repos {
		if r.ID != "" {
			cur[r.ID] = r.Path
		}
	}
	if b, err := json.MarshalIndent(cur, "", "  "); err == nil {
		_ = os.MkdirAll(s.stateDir(cfg), 0o755)
		_ = os.WriteFile(statePath, b, 0o644)
	}
}

// ---------- команды ----------

func cmdSources(cfg *Config) {
	fmt.Printf("Источники (%s):\n\n", cfg.ConfigPath())
	for _, s := range cfg.Sources {
		state := ""
		if s.Disabled {
			state = " [выключен]"
		}
		fmt.Printf("%s%s — %s, %s\n", s.Name, state, s.kindTitle(), s.Display())
		if s.Token != "" {
			fmt.Printf("    токен %s из %s\n", maskToken(s.Token), s.TokenFrom)
		} else {
			fmt.Printf("    токена нет\n")
		}
		if len(s.Groups) > 0 {
			fmt.Printf("    группы: %s\n", strings.Join(s.Groups, ", "))
		} else if s.Type == "gitlab" {
			scope := s.Scope
			if scope == "" {
				scope = "membership"
			}
			fmt.Printf("    выборка: %s\n", scope)
		}
		fmt.Printf("    каталог: %s\n", filepath.Join(cfg.Repos(), s.Name))
	}
}

// cmdSource — `source add|remove|list`.
func cmdSource(args []string, cwd string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	cfg := &Config{User: envOr("GITHUB_USER", "Pastor")}
	s := &Source{}
	var groups, token string
	var ssh bool
	fs := flag.NewFlagSet(toolName+" source "+sub, flag.ExitOnError)
	fs.StringVar(&cfg.Root, "dir", cwd, "корневая папка")
	fs.StringVar(&cfg.ConfigFile, "config", "", "файл источников (по умолчанию <папка>/"+configName+")")
	fs.StringVar(&cfg.ReposDir, "repos", defaultRepos, "подпапка с репозиториями")
	fs.StringVar(&s.Name, "name", "", "имя источника — оно же имя каталога в repositories/")
	fs.StringVar(&s.Type, "type", "gitlab", "тип: gitlab или github")
	fs.StringVar(&s.URL, "url", "", "адрес инстанса, например https://git.company.ru")
	fs.StringVar(&token, "token", "", "токен; будет сохранён в <папка>/.sync/tokens/<имя> с правами 0600")
	fs.StringVar(&s.TokenEnv, "token-env", "", "вместо -token: имя переменной с токеном (окружение или .env)")
	fs.StringVar(&s.TokenFile, "token-file", "", "вместо -token: файл с токеном")
	fs.StringVar(&s.User, "user", "", "GitHub: чьи репозитории синхронизировать")
	fs.StringVar(&groups, "groups", "", "GitLab: только эти группы, через запятую (с подгруппами)")
	fs.StringVar(&s.Scope, "scope", "", "GitLab: membership (по умолчанию), owned или all")
	fs.StringVar(&s.CAFile, "ca-file", "", "корневой сертификат инстанса (PEM)")
	fs.BoolVar(&s.Insecure, "insecure", false, "не проверять TLS-сертификат (небезопасно)")
	fs.BoolVar(&ssh, "ssh", false, "клонировать этот источник по ssh")
	fs.IntVar(&s.Jobs, "jobs", 0, "не больше N одновременных операций с этим сервером")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && s.Name == "" {
		s.Name, args = args[0], args[1:] // `source remove work`
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if abs, err := filepath.Abs(cfg.Root); err == nil {
		cfg.Root = abs
	}
	path := cfg.ConfigPath()
	fc, exists, err := readConfig(path)
	if err != nil {
		fatal("%v", err)
	}

	switch sub {
	case "list", "ls":
		if err := loadSources(cfg, map[string]bool{}); err != nil {
			fatal("%v", err)
		}
		cmdSources(cfg)
	case "add":
		if s.Name == "" {
			fatal("укажите -name")
		}
		if ssh {
			s.SSH = &ssh
		}
		s.Groups = splitList(groups)
		for _, x := range fc.Sources {
			if strings.EqualFold(x.Name, s.Name) {
				fatal("источник %q уже есть в %s", s.Name, path)
			}
		}
		fc.Sources = append(fc.Sources, s)
		if err := validateSources(fc.Sources); err != nil {
			fatal("%v", err)
		}
		if token != "" {
			p := tokenFilePath(cfg, s)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				fatal("%v", err)
			}
			if err := os.WriteFile(p, []byte(strings.TrimSpace(token)+"\n"), 0o600); err != nil {
				fatal("%v", err)
			}
			fmt.Printf("Токен сохранён в %s\n", p)
		}
		if err := writeConfig(path, fc); err != nil {
			fatal("%v", err)
		}
		if !exists {
			fmt.Printf("Создан %s (GitHub-источник из прежних настроек сохранён)\n", path)
		}
		fmt.Printf("Источник %s добавлен.\n\n", s.Name)
		if err := loadSources(cfg, map[string]bool{}); err != nil {
			fatal("%v", err)
		}
		added := cfg.source(s.Name)
		added.provider().Check(cfg)
	case "remove", "rm":
		if s.Name == "" {
			fatal("укажите имя источника")
		}
		if !exists {
			fatal("файла %s нет — удалять нечего", path)
		}
		var keep []*Source
		for _, x := range fc.Sources {
			if !strings.EqualFold(x.Name, s.Name) {
				keep = append(keep, x)
			}
		}
		if len(keep) == len(fc.Sources) {
			fatal("источника %q нет в %s", s.Name, path)
		}
		fc.Sources = keep
		if err := writeConfig(path, fc); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("Источник %s убран из %s.\n", s.Name, path)
		fmt.Printf("Локальные копии в %s не тронуты", filepath.Join(cfg.Repos(), s.Name))
		if _, err := os.Stat(tokenFilePath(cfg, s)); err == nil {
			fmt.Printf(", файл токена %s оставлен", tokenFilePath(cfg, s))
		}
		fmt.Println(".")
	default:
		fatal("неизвестная команда source %s (add, remove, list)", sub)
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// sortedKeys — ключи карты по алфавиту.
func sortedKeys(m map[string][]Repo) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
