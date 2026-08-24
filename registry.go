package main

import (
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const fieldSep = "\x1f"

// BranchInfo — ветка и её отличие от ветки по умолчанию.
type BranchInfo struct {
	Name       string `json:"name"`
	SHA        string `json:"sha"`
	IsDefault  bool   `json:"is_default"`
	IsCurrent  bool   `json:"is_current"`
	OnRemote   bool   `json:"on_remote"`
	Ahead      int    `json:"ahead"`  // коммитов есть в ветке, но нет в ветке по умолчанию
	Behind     int    `json:"behind"` // коммитов есть в ветке по умолчанию, но нет здесь
	LastCommit string `json:"last_commit"`
	Author     string `json:"author"`
	Subject    string `json:"subject"`
	// относительно одноимённой ветки upstream (только для форков)
	HasUpstream bool `json:"has_upstream"`
	UpAhead     int  `json:"upstream_ahead"`
	UpBehind    int  `json:"upstream_behind"`
}

// UpstreamDiff — расхождение с upstream словами.
func (b BranchInfo) UpstreamDiff() string {
	if !b.HasUpstream {
		return "—"
	}
	switch {
	case b.UpAhead == 0 && b.UpBehind == 0:
		return "синхронизирован"
	case b.UpAhead > 0 && b.UpBehind > 0:
		return fmt.Sprintf("+%d / −%d (разошлись)", b.UpAhead, b.UpBehind)
	case b.UpAhead > 0:
		return fmt.Sprintf("+%d свои коммиты", b.UpAhead)
	default:
		return fmt.Sprintf("−%d отстаёт", b.UpBehind)
	}
}

// Diff — краткое описание расхождения с веткой по умолчанию.
func (b BranchInfo) Diff() string {
	switch {
	case b.IsDefault:
		return "— (ветка по умолчанию)"
	case b.Ahead == 0 && b.Behind == 0:
		return "совпадает"
	case b.Ahead > 0 && b.Behind > 0:
		return fmt.Sprintf("+%d / −%d (разошлись)", b.Ahead, b.Behind)
	case b.Ahead > 0:
		return fmt.Sprintf("+%d впереди", b.Ahead)
	default:
		return fmt.Sprintf("−%d отстаёт", b.Behind)
	}
}

// TagInfo — тег.
type TagInfo struct {
	Name       string `json:"name"`
	SHA        string `json:"sha"`
	Date       string `json:"date"`
	Annotated  bool   `json:"annotated"`
	Subject    string `json:"subject"`
	OnDefault  bool   `json:"on_default"`
	BranchHint string `json:"branch_hint,omitempty"`
}

// SubmoduleInfo — подмодуль.
type SubmoduleInfo struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	SHA    string `json:"sha,omitempty"`
	Cloned bool   `json:"cloned"`
}

// RepoEntry — запись реестра.
type RepoEntry struct {
	Name          string          `json:"name"`
	FullName      string          `json:"full_name"`
	URL           string          `json:"url"`
	Description   string          `json:"description"`
	DescSource    string          `json:"description_source"` // github | readme | —
	Language      string          `json:"language"`
	Topics        []string        `json:"topics,omitempty"`
	License       string          `json:"license,omitempty"`
	Homepage      string          `json:"homepage,omitempty"`
	Owner         string          `json:"owner"`
	Kind          string          `json:"kind"`       // own | fork | member
	Visibility    string          `json:"visibility"` // public | private
	Archived      bool            `json:"archived"`
	ParentRepo    string          `json:"parent_repo,omitempty"`
	UpstreamURL   string          `json:"upstream_url,omitempty"`
	UpstreamState string          `json:"upstream_state,omitempty"`
	Stars         int             `json:"stars"`
	CreatedAt     string          `json:"created_at,omitempty"`
	PushedAt      string          `json:"pushed_at,omitempty"`
	Path          string          `json:"path"`
	Cloned        bool            `json:"cloned"`
	Clean         bool            `json:"clean"`
	DefaultBranch string          `json:"default_branch"`
	CurrentBranch string          `json:"current_branch"`
	BranchCount   int             `json:"branch_count"`
	TagCount      int             `json:"tag_count"`
	LatestTag     string          `json:"latest_tag,omitempty"`
	LatestTagDate string          `json:"latest_tag_date,omitempty"`
	CommitCount   int             `json:"commit_count"`
	LastCommit    string          `json:"last_commit,omitempty"`
	LastCommitSHA string          `json:"last_commit_sha,omitempty"`
	LastSubject   string          `json:"last_subject,omitempty"`
	DiskBytes     int64           `json:"disk_bytes"`
	Branches      []BranchInfo    `json:"branches"`
	Tags          []TagInfo       `json:"tags"`
	Submodules    []SubmoduleInfo `json:"submodules,omitempty"`
	Diverged      []string        `json:"diverged_branches,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// Registry — весь реестр.
type Registry struct {
	Generated   string      `json:"generated"`
	User        string      `json:"user"`
	Root        string      `json:"root"`
	ReposDir    string      `json:"repos_dir"`
	Tool        string      `json:"tool"`
	Total       int         `json:"total"`
	Own         int         `json:"own"`
	Forks       int         `json:"forks"`
	Member      int         `json:"member"`
	Private     int         `json:"private"`
	Archived    int         `json:"archived"`
	NotCloned   int         `json:"not_cloned"`
	TotalTags   int         `json:"total_tags"`
	TotalBranch int         `json:"total_branches"`
	DiskBytes   int64       `json:"disk_bytes"`
	Repos       []RepoEntry `json:"repos"`
}

// ---------- кэш описаний ----------

type cachedEntry struct {
	Fingerprint string    `json:"fingerprint"`
	Entry       RepoEntry `json:"entry"`
}

func regCachePath(cfg *Config) string {
	return filepath.Join(cfg.Root, syncDirName, "registry-cache.json")
}

func loadRegCache(cfg *Config) map[string]cachedEntry {
	out := map[string]cachedEntry{}
	if b, err := os.ReadFile(regCachePath(cfg)); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveRegCache(cfg *Config, m map[string]cachedEntry) {
	if b, err := json.Marshal(m); err == nil {
		_ = os.MkdirAll(filepath.Join(cfg.Root, syncDirName), 0o755)
		_ = os.WriteFile(regCachePath(cfg), b, 0o644)
	}
}

// fingerprint — отпечаток состояния репозитория: меняется при любом
// изменении веток, тегов или HEAD. Дешевле, чем пересобирать описание.
func fingerprint(cfg *Config, dir string) string {
	out, err := git(cfg, dir, "for-each-ref", "--format=%(objectname)%(refname)", "refs/")
	if err != nil {
		return ""
	}
	head, _ := git(cfg, dir, "rev-parse", "--verify", "-q", "HEAD")
	sum := sha1.Sum([]byte(out + "|" + head + "|" + version))
	return hex.EncodeToString(sum[:])
}

// BuildRegistry собирает реестр по локальному состоянию + кэшу GitHub API.
func BuildRegistry(cfg *Config) error {
	meta := map[string]Repo{}
	cached := loadRepoCache(cfg)
	if len(cached) == 0 {
		if repos, err := FetchRepos(cfg); err == nil {
			cached = repos
			saveRepoCache(cfg, repos)
		} else {
			log.Printf("предупреждение: метаданные GitHub недоступны (%v), реестр будет только по локальным данным", err)
		}
	}
	for _, r := range cached {
		meta[strings.ToLower(r.Name)] = r
	}

	// имена: всё из API + всё, что реально лежит на диске
	names := map[string]bool{}
	for _, r := range cached {
		if r.Fork && !cfg.Forks {
			continue
		}
		names[r.Name] = true
	}
	if entries, err := os.ReadDir(cfg.Repos()); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "__") &&
				isGitRepo(filepath.Join(cfg.Repos(), e.Name())) {
				names[e.Name()] = true
			}
		}
	}

	// реестр всегда описывает папку целиком, фильтр -only на него не влияет
	var list []string
	for n := range names {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i]) < strings.ToLower(list[j]) })

	reg := &Registry{
		Generated: time.Now().Format("2006-01-02 15:04:05 -07:00"),
		User:      cfg.User,
		Root:      cfg.Root,
		ReposDir:  cfg.Repos(),
		Tool:      toolName + " " + version,
	}

	cache := loadRegCache(cfg)
	var cacheMu sync.Mutex
	var deadline time.Time
	if cfg.Budget > 0 {
		deadline = time.Now().Add(cfg.Budget)
	}
	reused, rebuilt, deferred := 0, 0, 0

	type job struct {
		idx   int
		entry RepoEntry
	}
	entries := make([]RepoEntry, len(list))
	ch := make(chan int)
	done := make(chan job)
	workers := cfg.Jobs
	if workers > 4 {
		workers = 4
	}
	for w := 0; w < workers; w++ {
		go func() {
			for i := range ch {
				name := list[i]
				dir := filepath.Join(cfg.Repos(), name)
				fp := ""
				if isGitRepo(dir) {
					fp = fingerprint(cfg, dir)
				}
				cacheMu.Lock()
				c, ok := cache[name]
				cacheMu.Unlock()
				if ok && fp != "" && c.Fingerprint == fp {
					cacheMu.Lock()
					reused++
					cacheMu.Unlock()
					done <- job{i, c.Entry}
					continue
				}
				if !deadline.IsZero() && time.Now().After(deadline) {
					cacheMu.Lock()
					deferred++
					cacheMu.Unlock()
					var e RepoEntry
					if ok {
						e = c.Entry
						e.Error = "описание устарело, будет обновлено при следующем запуске"
					} else {
						e = shallowEntry(cfg, name, meta[strings.ToLower(name)])
					}
					done <- job{i, e}
					continue
				}
				e := describeRepo(cfg, name, meta[strings.ToLower(name)])
				cacheMu.Lock()
				rebuilt++
				if fp != "" {
					cache[name] = cachedEntry{Fingerprint: fp, Entry: e}
				}
				cacheMu.Unlock()
				done <- job{i, e}
			}
		}()
	}
	go func() {
		for i := range list {
			ch <- i
		}
		close(ch)
	}()
	for range list {
		j := <-done
		entries[j.idx] = j.entry
	}

	for _, e := range entries {
		reg.Total++
		switch e.Kind {
		case "fork":
			reg.Forks++
		case "member":
			reg.Member++
		default:
			reg.Own++
		}
		if e.Visibility == "private" {
			reg.Private++
		}
		if e.Archived {
			reg.Archived++
		}
		if !e.Cloned {
			reg.NotCloned++
		}
		reg.TotalTags += e.TagCount
		reg.TotalBranch += e.BranchCount
		reg.DiskBytes += e.DiskBytes
	}
	reg.Repos = entries

	if err := writeJSON(cfg, reg); err != nil {
		return err
	}
	if err := writeCSV(cfg, reg); err != nil {
		return err
	}
	if err := writeMarkdown(cfg, reg); err != nil {
		return err
	}
	saveRegCache(cfg, cache)
	log.Printf("Реестр обновлён: %d репозиториев, %d веток, %d тегов, %s на диске "+
		"(описано заново: %d, из кэша: %d, отложено: %d)",
		reg.Total, reg.TotalBranch, reg.TotalTags, humanSize(reg.DiskBytes),
		rebuilt, reused, deferred)
	if deferred > 0 {
		log.Printf("Не хватило бюджета времени на %d репозиториев — запустите `%s registry` ещё раз", deferred, toolName)
	}
	return nil
}

// shallowEntry — запись только по метаданным GitHub, без обхода репозитория.
// Используется, когда не хватило бюджета времени: строка в реестре есть,
// подробности появятся при следующем запуске.
func shallowEntry(cfg *Config, name string, m Repo) RepoEntry {
	e := baseEntry(cfg, name, m)
	e.Cloned = isGitRepo(filepath.Join(cfg.Repos(), name))
	if e.Cloned {
		e.Error = "описание ещё не собрано — запустите `" + toolName + " registry` ещё раз"
	} else {
		e.Error = "не склонирован"
	}
	return e
}

// baseEntry заполняет то, что известно из метаданных GitHub, без обращения к git.
func baseEntry(cfg *Config, name string, m Repo) RepoEntry {
	dir := filepath.Join(cfg.Repos(), name)
	e := RepoEntry{
		Name:        name,
		FullName:    m.FullName,
		URL:         m.HTMLURL,
		Description: strings.TrimSpace(m.Description),
		DescSource:  "github",
		Language:    m.Language,
		Topics:      m.Topics,
		Homepage:    m.Homepage,
		Kind:        "own",
		Visibility:  "public",
		Archived:    m.Archived,
		Stars:       m.Stars,
		CreatedAt:   shortDate(m.CreatedAt),
		PushedAt:    shortDate(m.PushedAt),
		Path:        dir,
	}
	if e.FullName == "" {
		e.FullName = cfg.User + "/" + name
	}
	if e.URL == "" {
		e.URL = "https://github.com/" + e.FullName
	}
	e.Owner = m.Owner.Login
	if e.Owner == "" {
		e.Owner = cfg.User
	}
	if m.Fork {
		e.Kind = "fork"
	}
	if !strings.EqualFold(e.Owner, cfg.User) {
		e.Kind = "member" // репозиторий чужого владельца, пользователь — участник
	}
	if m.Private {
		e.Visibility = "private"
	}
	if m.License != nil && m.License.SPDXID != "" && m.License.SPDXID != "NOASSERTION" {
		e.License = m.License.SPDXID
	}
	if m.Parent != nil {
		e.ParentRepo = m.Parent.FullName
	}
	e.DefaultBranch = m.DefaultBranch
	return e
}

// describeRepo собирает полное описание одного репозитория.
func describeRepo(cfg *Config, name string, m Repo) RepoEntry {
	dir := filepath.Join(cfg.Repos(), name)
	e := baseEntry(cfg, name, m)

	if !isGitRepo(dir) {
		e.Error = "не склонирован"
		return e
	}
	e.Cloned = true
	e.DiskBytes = dirSize(dir)
	e.Clean = isClean(cfg, dir)
	e.CurrentBranch = currentBranch(cfg, dir)
	e.DefaultBranch = defaultBranch(cfg, dir, m.DefaultBranch)

	if e.Description == "" {
		if d := descriptionFromReadme(dir); d != "" {
			e.Description = d
			e.DescSource = "readme"
		} else {
			e.DescSource = "—"
		}
	}

	// последний коммит
	if out, err := git(cfg, dir, "log", "-1", "--format=%h"+fieldSep+"%cI"+fieldSep+"%s"); err == nil {
		p := strings.Split(out, fieldSep)
		if len(p) == 3 {
			e.LastCommitSHA, e.LastCommit, e.LastSubject = p[0], shortDate(p[1]), oneLine(p[2], 100)
		}
	}
	if out, err := git(cfg, dir, "rev-list", "--count", "HEAD"); err == nil {
		e.CommitCount, _ = strconv.Atoi(out)
	}

	if up, err := git(cfg, dir, "remote", "get-url", "upstream"); err == nil && up != "" {
		e.UpstreamURL = up
		if e.ParentRepo == "" {
			e.ParentRepo = strings.TrimSuffix(strings.TrimPrefix(up, "https://github.com/"), ".git")
		}
	}

	e.Branches = collectBranches(cfg, dir, e.DefaultBranch, e.CurrentBranch)
	e.BranchCount = len(e.Branches)
	if e.UpstreamURL != "" {
		upRefs, _ := gitLines(cfg, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/upstream")
		if len(upRefs) == 0 {
			e.UpstreamState = "ещё не выкачан — появится после `./" + toolName + "`"
		} else {
			e.UpstreamState = summarizeUpstream(e.Branches, true)
		}
	}
	for _, b := range e.Branches {
		if !b.IsDefault && b.Ahead > 0 && b.Behind > 0 {
			e.Diverged = append(e.Diverged, b.Name)
		}
	}
	e.Tags = collectTags(cfg, dir, e.DefaultBranch)
	e.TagCount = len(e.Tags)
	if e.TagCount > 0 {
		e.LatestTag = e.Tags[0].Name
		e.LatestTagDate = e.Tags[0].Date
	}
	e.Submodules = collectSubmodules(cfg, dir)
	return e
}

func collectBranches(cfg *Config, dir, def, cur string) []BranchInfo {
	lines, err := gitLines(cfg, dir, "for-each-ref",
		"--format=%(refname:strip=2)"+fieldSep+"%(objectname:short)"+fieldSep+"%(committerdate:iso8601)"+fieldSep+"%(authorname)"+fieldSep+"%(contents:subject)",
		"refs/heads")
	if err != nil {
		return nil
	}
	remote := map[string]bool{}
	for _, b := range remoteBranches(cfg, dir) {
		remote[b] = true
	}
	var out []BranchInfo
	for _, l := range lines {
		p := strings.Split(l, fieldSep)
		if len(p) < 5 {
			continue
		}
		b := BranchInfo{
			Name:       p[0],
			SHA:        p[1],
			LastCommit: shortDate(p[2]),
			Author:     p[3],
			Subject:    oneLine(p[4], 80),
			IsDefault:  p[0] == def,
			IsCurrent:  p[0] == cur,
			OnRemote:   remote[p[0]],
		}
		if !b.IsDefault && def != "" {
			if o, err := git(cfg, dir, "rev-list", "--left-right", "--count", def+"..."+b.Name); err == nil {
				f := strings.Fields(o)
				if len(f) == 2 {
					b.Behind, _ = strconv.Atoi(f[0])
					b.Ahead, _ = strconv.Atoi(f[1])
				}
			}
		}
		if _, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/upstream/"+b.Name); err == nil {
			b.HasUpstream = true
			if o, err := git(cfg, dir, "rev-list", "--left-right", "--count",
				"refs/remotes/upstream/"+b.Name+"..."+b.Name); err == nil {
				f := strings.Fields(o)
				if len(f) == 2 {
					b.UpBehind, _ = strconv.Atoi(f[0])
					b.UpAhead, _ = strconv.Atoi(f[1])
				}
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].LastCommit > out[j].LastCommit
	})
	return out
}

// summarizeUpstream — одна строка о состоянии форка относительно upstream.
func summarizeUpstream(branches []BranchInfo, hasUpstream bool) string {
	if !hasUpstream {
		return ""
	}
	tracked, synced, behind, diverged, ahead := 0, 0, 0, 0, 0
	for _, b := range branches {
		if !b.HasUpstream {
			continue
		}
		tracked++
		switch {
		case b.UpAhead == 0 && b.UpBehind == 0:
			synced++
		case b.UpAhead > 0 && b.UpBehind > 0:
			diverged++
		case b.UpBehind > 0:
			behind++
		default:
			ahead++
		}
	}
	if tracked == 0 {
		return "общих веток с upstream нет"
	}
	var parts []string
	if synced > 0 {
		parts = append(parts, fmt.Sprintf("синхронизировано %d из %d", synced, tracked))
	}
	if behind > 0 {
		parts = append(parts, fmt.Sprintf("отстаёт %d", behind))
	}
	if ahead > 0 {
		parts = append(parts, fmt.Sprintf("со своими коммитами %d", ahead))
	}
	if diverged > 0 {
		parts = append(parts, fmt.Sprintf("разошлось %d", diverged))
	}
	return strings.Join(parts, ", ")
}

func collectTags(cfg *Config, dir, def string) []TagInfo {
	lines, err := gitLines(cfg, dir, "for-each-ref",
		"--format=%(refname:strip=2)"+fieldSep+"%(objectname:short)"+fieldSep+"%(creatordate:iso8601)"+fieldSep+"%(objecttype)"+fieldSep+"%(contents:subject)",
		"--sort=-creatordate", "refs/tags")
	if err != nil {
		return nil
	}
	var out []TagInfo
	for _, l := range lines {
		p := strings.Split(l, fieldSep)
		if len(p) < 5 {
			continue
		}
		out = append(out, TagInfo{
			Name:      p[0],
			SHA:       p[1],
			Date:      shortDate(p[2]),
			Annotated: p[3] == "tag",
			Subject:   oneLine(p[4], 80),
		})
	}
	// пометка «тег на ветке по умолчанию» — только для первых 30, чтобы не тормозить
	limit := len(out)
	if limit > 30 {
		limit = 30
	}
	if def != "" {
		for i := 0; i < limit; i++ {
			if _, err := git(cfg, dir, "merge-base", "--is-ancestor", out[i].Name+"^{commit}", def); err == nil {
				out[i].OnDefault = true
			}
		}
	}
	return out
}

func collectSubmodules(cfg *Config, dir string) []SubmoduleInfo {
	if !hasSubmodules(dir) {
		return nil
	}
	lines, err := gitLines(cfg, dir, "config", "-f", ".gitmodules", "--get-regexp", `submodule\..*\.path`)
	if err != nil {
		return nil
	}
	var out []SubmoduleInfo
	for _, l := range lines {
		f := strings.SplitN(l, " ", 2)
		if len(f) != 2 {
			continue
		}
		key := strings.TrimSuffix(f[0], ".path")
		path := strings.TrimSpace(f[1])
		url, _ := git(cfg, dir, "config", "-f", ".gitmodules", "--get", key+".url")
		sm := SubmoduleInfo{Path: path, URL: url}
		sm.Cloned = isGitRepo(filepath.Join(dir, path))
		if sha, err := git(cfg, dir, "rev-parse", "-q", "--verify", "HEAD:"+path); err == nil && len(sha) >= 7 {
			sm.SHA = sha[:7]
		}
		out = append(out, sm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

var (
	reBadge    = regexp.MustCompile(`^\s*[\[!]`)
	reMdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	reHTMLTag  = regexp.MustCompile(`<[^>]+>`)
	reMdAccent = regexp.MustCompile("[*_`#>]+")
)

// descriptionFromReadme достаёт первый осмысленный абзац README.
func descriptionFromReadme(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var readme string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(e.Name()), "README") {
			readme = filepath.Join(dir, e.Name())
			break
		}
	}
	if readme == "" {
		return ""
	}
	b, err := os.ReadFile(readme)
	if err != nil {
		return ""
	}
	if len(b) > 64*1024 {
		b = b[:64*1024]
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || reBadge.MatchString(l) || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "===") {
			continue
		}
		if strings.HasPrefix(l, "#") {
			continue // заголовок обычно дублирует имя репозитория
		}
		l = reMdLink.ReplaceAllString(l, "$1")
		l = reHTMLTag.ReplaceAllString(l, "")
		l = reMdAccent.ReplaceAllString(l, "")
		l = strings.TrimSpace(l)
		if len([]rune(l)) < 15 {
			continue
		}
		return oneLine(l, 180)
	}
	return ""
}

func shortDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// ---------- вывод ----------

func writeJSON(cfg *Config, reg *Registry) error {
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Root, "registry.json"), append(b, '\n'), 0o644)
}

func writeCSV(cfg *Config, reg *Registry) error {
	f, err := os.Create(filepath.Join(cfg.Root, "registry.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	_, _ = f.WriteString("\ufeff") // BOM, чтобы Excel не ломал кириллицу
	w := csv.NewWriter(f)
	_ = w.Write([]string{"name", "full_name", "owner", "kind", "visibility", "archived", "language",
		"default_branch", "current_branch", "branches", "tags", "latest_tag", "latest_tag_date",
		"commits", "last_commit", "last_commit_sha", "diverged_branches", "submodules",
		"upstream", "upstream_state", "disk_bytes", "disk_human", "stars", "url", "description"})
	for _, e := range reg.Repos {
		_ = w.Write([]string{
			e.Name, e.FullName, e.Owner, e.Kind, e.Visibility, strconv.FormatBool(e.Archived), e.Language,
			e.DefaultBranch, e.CurrentBranch, strconv.Itoa(e.BranchCount), strconv.Itoa(e.TagCount),
			e.LatestTag, e.LatestTagDate, strconv.Itoa(e.CommitCount), e.LastCommit, e.LastCommitSHA,
			strings.Join(e.Diverged, " "), strconv.Itoa(len(e.Submodules)),
			e.ParentRepo, e.UpstreamState,
			strconv.FormatInt(e.DiskBytes, 10), humanSize(e.DiskBytes), strconv.Itoa(e.Stars),
			e.URL, e.Description,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return writeBranchCSV(cfg, reg)
}

func writeBranchCSV(cfg *Config, reg *Registry) error {
	f, err := os.Create(filepath.Join(cfg.Root, "registry-branches.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	_, _ = f.WriteString("\ufeff")
	w := csv.NewWriter(f)
	_ = w.Write([]string{"repo", "branch", "is_default", "on_remote", "ahead", "behind",
		"difference", "upstream_ahead", "upstream_behind", "upstream_difference",
		"last_commit", "author", "sha", "subject"})
	for _, e := range reg.Repos {
		for _, b := range e.Branches {
			_ = w.Write([]string{e.Name, b.Name, strconv.FormatBool(b.IsDefault),
				strconv.FormatBool(b.OnRemote), strconv.Itoa(b.Ahead), strconv.Itoa(b.Behind),
				b.Diff(), strconv.Itoa(b.UpAhead), strconv.Itoa(b.UpBehind), b.UpstreamDiff(),
				b.LastCommit, b.Author, b.SHA, b.Subject})
		}
	}
	w.Flush()
	return w.Error()
}

func writeMarkdown(cfg *Config, reg *Registry) error {
	var s strings.Builder
	s.WriteString("# Реестр репозиториев " + reg.User + "\n\n")
	s.WriteString("Файл сгенерирован автоматически (`" + toolName + " registry`) — править вручную не нужно.\n\n")
	s.WriteString(fmt.Sprintf("**Обновлено:** %s  \n", reg.Generated))
	s.WriteString(fmt.Sprintf("**Всего репозиториев:** %d (своих %d, форков %d, чужих с участием %d, приватных %d, архивных %d)  \n",
		reg.Total, reg.Own, reg.Forks, reg.Member, reg.Private, reg.Archived))
	s.WriteString(fmt.Sprintf("**Веток:** %d · **Тегов:** %d · **На диске:** %s\n",
		reg.TotalBranch, reg.TotalTags, humanSize(reg.DiskBytes)))
	if reg.NotCloned > 0 {
		s.WriteString(fmt.Sprintf("\n> Не склонировано: %d — запустите `./%s`\n", reg.NotCloned, toolName))
	}
	s.WriteString("\n---\n\n")

	writeTable := func(title string, kind string) int {
		var rows []RepoEntry
		for _, e := range reg.Repos {
			if e.Kind == kind {
				rows = append(rows, e)
			}
		}
		if len(rows) == 0 {
			return 0
		}
		s.WriteString("## " + title + " (" + strconv.Itoa(len(rows)) + ")\n\n")
		s.WriteString("| Репозиторий | Язык | Ветка по умолчанию | Ветки | Теги | Последний тег | Последний коммит | Размер | Описание |\n")
		s.WriteString("|---|---|---|---:|---:|---|---|---:|---|\n")
		for _, e := range rows {
			name := fmt.Sprintf("[%s](%s)", mdEscape(e.Name), e.URL)
			flags := ""
			if e.Archived {
				flags += " 🗄"
			}
			if e.Visibility == "private" {
				flags += " 🔒"
			}
			if !e.Cloned {
				flags += " ⚠"
			}
			if len(e.Submodules) > 0 {
				flags += fmt.Sprintf(" ⊞%d", len(e.Submodules))
			}
			latest := e.LatestTag
			if latest != "" && e.LatestTagDate != "" {
				latest = fmt.Sprintf("`%s` (%s)", mdEscape(latest), e.LatestTagDate)
			}
			s.WriteString(fmt.Sprintf("| %s%s | %s | `%s` | %d | %d | %s | %s | %s | %s |\n",
				name, flags, dash(e.Language), dash(e.DefaultBranch), e.BranchCount, e.TagCount,
				dash(latest), dash(e.LastCommit), humanSize(e.DiskBytes),
				mdEscape(oneLine(e.Description, 90))))
		}
		s.WriteString("\nОбозначения: 🔒 приватный · 🗄 архивный · ⚠ не склонирован · ⊞N подмодулей\n\n")
		return len(rows)
	}

	writeTable("Свои репозитории", "own")
	writeTable("Форки", "fork")
	writeTable("Чужие репозитории с участием "+reg.User, "member")

	s.WriteString("---\n\n## Подробно по репозиториям\n\n")
	for _, e := range reg.Repos {
		s.WriteString("### " + mdEscape(e.Name) + "\n\n")
		kind := "свой репозиторий"
		switch e.Kind {
		case "fork":
			kind = "форк"
			if e.ParentRepo != "" {
				kind += " от `" + e.ParentRepo + "`"
			}
		case "member":
			kind = "владелец `" + e.Owner + "`, " + reg.User + " — участник"
		}
		s.WriteString(fmt.Sprintf("%s · %s · %s\n\n", "["+e.FullName+"]("+e.URL+")", kind, e.Visibility))
		if e.Description != "" {
			src := ""
			if e.DescSource == "readme" {
				src = " _(из README)_"
			}
			s.WriteString("> " + mdEscape(oneLine(e.Description, 300)) + src + "\n\n")
		}
		if !e.Cloned {
			s.WriteString("**Локальной копии нет.** Ветка по умолчанию на GitHub: `" +
				dash(e.DefaultBranch) + "`. Запустите `./" + toolName + "`.\n\n")
			continue
		}
		s.WriteString(fmt.Sprintf("- Ветка по умолчанию: `%s`", dash(e.DefaultBranch)))
		if e.CurrentBranch != "" && e.CurrentBranch != e.DefaultBranch {
			s.WriteString(fmt.Sprintf(" (в рабочей копии выбрана `%s`)", e.CurrentBranch))
		}
		s.WriteString("\n")
		if e.Language != "" {
			s.WriteString("- Основной язык: " + e.Language + "\n")
		}
		if e.License != "" {
			s.WriteString("- Лицензия: " + e.License + "\n")
		}
		if len(e.Topics) > 0 {
			s.WriteString("- Темы: " + strings.Join(e.Topics, ", ") + "\n")
		}
		s.WriteString(fmt.Sprintf("- Коммитов в `%s`: %d · последний: `%s` от %s — %s\n",
			dash(e.DefaultBranch), e.CommitCount, e.LastCommitSHA, e.LastCommit, mdEscape(e.LastSubject)))
		s.WriteString(fmt.Sprintf("- Веток: %d · тегов: %d · на диске: %s\n",
			e.BranchCount, e.TagCount, humanSize(e.DiskBytes)))
		if !e.Clean {
			s.WriteString("- ⚠ В рабочей копии есть незакоммиченные изменения — синхронизация её не трогает\n")
		}
		if len(e.Diverged) > 0 {
			s.WriteString("- ⚠ Разошлись с веткой по умолчанию: `" + strings.Join(e.Diverged, "`, `") + "`\n")
		}
		if e.ParentRepo != "" && e.UpstreamURL != "" {
			s.WriteString(fmt.Sprintf("- Upstream: [%s](https://github.com/%s) — %s\n",
				e.ParentRepo, e.ParentRepo, dash(e.UpstreamState)))
		}
		s.WriteString("\n")

		if len(e.Branches) > 0 {
			withUp := e.UpstreamURL != ""
			s.WriteString("**Ветки**\n\n")
			s.WriteString("| Ветка | Отличие от `" + dash(e.DefaultBranch) + "` |")
			if withUp {
				s.WriteString(" Отн. upstream |")
			}
			s.WriteString(" Последний коммит | Автор | На origin |\n")
			s.WriteString("|---|---|")
			if withUp {
				s.WriteString("---|")
			}
			s.WriteString("---|---|:---:|\n")
			limit := len(e.Branches)
			hidden := 0
			if limit > 25 {
				hidden = limit - 25
				limit = 25
			}
			for i := 0; i < limit; i++ {
				b := e.Branches[i]
				mark := "`" + b.Name + "`"
				if b.IsCurrent {
					mark += " ←"
				}
				yes := "—"
				if b.OnRemote {
					yes = "да"
				}
				up := ""
				if withUp {
					up = " " + b.UpstreamDiff() + " |"
				}
				s.WriteString(fmt.Sprintf("| %s | %s |%s %s | %s | %s |\n",
					mark, b.Diff(), up, b.LastCommit, mdEscape(oneLine(b.Author, 24)), yes))
			}
			if hidden > 0 {
				s.WriteString(fmt.Sprintf("\n_…и ещё %d веток — полный список в `registry.json` и `registry-branches.csv`._\n", hidden))
			}
			s.WriteString("\n")
		}

		if len(e.Tags) > 0 {
			s.WriteString("**Теги** (" + strconv.Itoa(len(e.Tags)) + ")\n\n")
			limit := len(e.Tags)
			if limit > 15 {
				limit = 15
			}
			var parts []string
			for i := 0; i < limit; i++ {
				parts = append(parts, fmt.Sprintf("`%s` (%s)", e.Tags[i].Name, e.Tags[i].Date))
			}
			s.WriteString(strings.Join(parts, " · "))
			if len(e.Tags) > limit {
				s.WriteString(fmt.Sprintf(" · …ещё %d", len(e.Tags)-limit))
			}
			s.WriteString("\n\n")
		}

		if len(e.Submodules) > 0 {
			s.WriteString("**Подмодули**\n\n")
			s.WriteString("| Путь | Источник | Коммит | Выкачан |\n|---|---|---|:---:|\n")
			for _, sm := range e.Submodules {
				ok := "нет"
				if sm.Cloned {
					ok = "да"
				}
				s.WriteString(fmt.Sprintf("| `%s` | %s | `%s` | %s |\n",
					sm.Path, mdEscape(oneLine(sm.URL, 70)), sm.SHA, ok))
			}
			s.WriteString("\n")
		}
	}

	return os.WriteFile(filepath.Join(cfg.Root, "REGISTRY.md"), []byte(s.String()), 0o644)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func mdEscape(s string) string {
	r := strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
