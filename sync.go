package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result — итог обработки одного репозитория.
type Result struct {
	Name     string
	Action   string // cloned | updated | up-to-date | skipped | failed
	Detail   string
	Err      error
	Duration time.Duration
}

// cmdToken показывает, откуда взяты токены источников, и проверяет их.
func cmdToken(cfg *Config) {
	for i, s := range cfg.Active() {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("=== %s — %s, %s ===\n", s.Name, s.kindTitle(), s.webBase())
		s.provider().Check(cfg)
	}
}

// cmdList печатает списки репозиториев источников, ничего не меняя.
func cmdList(cfg *Config) {
	total := 0
	for _, s := range cfg.Active() {
		repos, err := s.provider().List(cfg)
		if err != nil {
			log.Printf("ОШИБКА: %v", err)
			continue
		}
		repos = Filter(cfg, repos)
		fmt.Printf("\n=== %s — %s, %s ===\n", s.Name, s.kindTitle(), s.Display())
		own, forks, member, priv := 0, 0, 0, 0
		for _, r := range repos {
			kind := "свой"
			switch {
			case r.Fork:
				kind = "форк"
				forks++
			case !r.Mine:
				kind = "участник"
				member++
			default:
				own++
			}
			if r.Private {
				priv++
			}
			size := ""
			if r.Size > 0 {
				size = humanSize(r.Size)
			}
			fmt.Printf("%-50s %-9s %-8s %-10s %9s  %s\n",
				r.Path, kind, r.Visibility, r.DefaultBranch, size, oneLine(r.Description, 60))
		}
		fmt.Printf("Всего в %s: %d (своих %d, форков %d, чужих/групповых %d, закрытых %d)\n",
			s.Name, len(repos), own, forks, member, priv)
		total += len(repos)
	}
	if len(cfg.Active()) > 1 {
		fmt.Printf("\nВсего по источникам: %d\n", total)
	}
}

// cmdSync — основная команда: списки источников, клонирование новых, обновление старых.
func cmdSync(cfg *Config) {
	start := time.Now()
	cleanup := setupAskPass(cfg)
	defer cleanup()

	if err := os.MkdirAll(cfg.Repos(), 0o755); err != nil {
		fatal("не удалось создать %s: %v", cfg.Repos(), err)
	}
	migrateStrays(cfg)
	migrateLayout(cfg)
	log.Printf("Синхронизация -> %s, потоков: %d", cfg.Root, cfg.Jobs)

	listed := map[string][]Repo{} // источники, чей список удалось получить
	var queues [][]Repo
	for _, s := range cfg.Active() {
		auth := "без токена"
		if s.Token != "" {
			auth = "токен из " + s.TokenFrom
		}
		log.Printf("Источник %s (%s): %s, %s", s.Name, s.kindTitle(), s.webBase(), auth)
		repos, err := s.provider().List(cfg)
		if err != nil {
			log.Printf("ОШИБКА: %v — источник пропущен", err)
			continue
		}
		saveRepoCache(cfg, s, repos)
		applyMoves(cfg, s, repos)
		listed[s.Name] = repos
		todo := Filter(cfg, repos)
		log.Printf("  %s: получено репозиториев %d, к обработке %d", s.Name, len(repos), len(todo))
		queues = append(queues, todo)
	}
	if len(listed) == 0 {
		fatal("ни один источник не ответил")
	}
	repos := interleave(queues)

	if cfg.DryRun {
		sortRepos(repos)
		for _, r := range repos {
			state := "обновить"
			if !isGitRepo(cfg.RepoDir(r)) {
				state = "склонировать"
			}
			log.Printf("[план] %-12s %s", state, r.Key())
		}
		log.Printf("Пробный запуск, изменений не внесено")
		return
	}

	results := runParallel(cfg, repos, func(r Repo) Result {
		return syncRepo(cfg, r)
	})

	reportOrphans(cfg, listed)
	summarize(results, start)

	if !cfg.NoRegistry {
		log.Printf("Сборка реестра...")
		if err := BuildRegistry(cfg); err != nil {
			log.Printf("предупреждение: реестр собрать не удалось: %v", err)
		}
	}
}

// cmdScan — режим gitscan: обновить всё, что уже лежит в папке, без API.
func cmdScan(cfg *Config) {
	start := time.Now()
	cleanup := setupAskPass(cfg)
	defer cleanup()

	migrateStrays(cfg)
	migrateLayout(cfg)
	dirs := findGitDirs(cfg.Repos())
	log.Printf("Найдено git-репозиториев: %d в %s", len(dirs), cfg.Repos())

	known := map[string]Repo{}
	for _, s := range cfg.Sources {
		for _, r := range loadRepoCache(cfg, s) {
			known[strings.ToLower(r.Key())] = r
		}
	}

	var repos []Repo
	for _, d := range dirs {
		r := repoAt(cfg, d, known)
		if cfg.Only != "" && !strings.Contains(strings.ToLower(r.Key()), strings.ToLower(cfg.Only)) {
			continue
		}
		if !cfg.sourceAllowed(r.Source) {
			continue
		}
		repos = append(repos, r)
	}

	results := runParallel(cfg, repos, func(r Repo) Result {
		return updateRepo(cfg, r, cfg.RepoDir(r))
	})
	summarize(results, start)

	if !cfg.NoRegistry {
		if err := BuildRegistry(cfg); err != nil {
			log.Printf("предупреждение: реестр собрать не удалось: %v", err)
		}
	}
}

// repoAt описывает найденный на диске каталог: по кэшу источника, а если его
// там нет — по пути <источник>/<путь>.
func repoAt(cfg *Config, dir string, known map[string]Repo) Repo {
	rel, err := filepath.Rel(cfg.Repos(), dir)
	if err != nil {
		rel = filepath.Base(dir)
	}
	rel = filepath.ToSlash(rel)
	if r, ok := known[strings.ToLower(rel)]; ok {
		return r
	}
	r := Repo{Path: rel, Name: filepath.Base(dir)}
	if i := strings.Index(rel, "/"); i > 0 {
		if s := cfg.source(rel[:i]); s != nil {
			r.Source, r.Path, r.src = s.Name, rel[i+1:], s
		}
	}
	return r
}

func cmdRegistry(cfg *Config) {
	if err := BuildRegistry(cfg); err != nil {
		fatal("%v", err)
	}
}

// runParallel обрабатывает репозитории пулом воркеров. Если у источника
// задан предел jobs, с его сервером одновременно работает не больше jobs потоков.
func runParallel(cfg *Config, repos []Repo, fn func(Repo) Result) []Result {
	jobs := make(chan Repo)
	results := make([]Result, 0, len(repos))
	var mu sync.Mutex
	var wg sync.WaitGroup
	total := len(repos)
	done := 0

	for i := 0; i < cfg.Jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				res := func() Result {
					if r.src != nil && r.src.sem != nil {
						r.src.sem <- struct{}{}
						defer func() { <-r.src.sem }()
					}
					return fn(r)
				}()
				mu.Lock()
				done++
				n := done
				results = append(results, res)
				mu.Unlock()
				status := res.Action
				if res.Err != nil {
					status = "ОШИБКА"
				}
				log.Printf("[%d/%d] %-50s %-12s %s %s", n, total, res.Name, status,
					humanDuration(res.Duration), res.Detail)
				if res.Err != nil {
					log.Printf("        %v", res.Err)
				}
			}
		}()
	}
	for _, r := range repos {
		jobs <- r
	}
	close(jobs)
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Name) < strings.ToLower(results[j].Name)
	})
	return results
}

// syncRepo клонирует репозиторий, если его нет, иначе обновляет.
// Повреждённую копию (обрыв клонирования, битый .git) уводит в карантин
// и клонирует заново.
func syncRepo(cfg *Config, r Repo) Result {
	dir := cfg.RepoDir(r)
	if isGitRepo(dir) {
		if healthy, why := repoHealthy(cfg, dir); !healthy {
			if err := quarantine(cfg, dir, r.Key()); err != nil {
				return Result{Name: r.Key(), Action: "skipped",
					Detail: "повреждён (" + why + "), убрать не удалось", Err: err}
			}
			res := cloneRepo(cfg, r, dir)
			res.Detail = "копия была повреждена (" + why + "), убрана в " +
				brokenDirName + "; " + res.Detail
			return res
		}
		return updateRepo(cfg, r, dir)
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		if empty, _ := isEmptyDir(dir); empty {
			_ = os.Remove(dir)
		} else if len(findGitDirs(dir)) > 0 {
			// в GitLab проект и группа могут называться одинаково:
			// group/app и group/app/backend — второй уже лежит внутри
			return Result{Name: r.Key(), Action: "skipped",
				Detail: "в каталоге лежат другие репозитории (группа с тем же именем)"}
		} else {
			return Result{Name: r.Key(), Action: "skipped",
				Detail: "каталог существует, но это не git-репозиторий"}
		}
	}
	return cloneRepo(cfg, r, dir)
}

// repoHealthy проверяет, что с копией можно работать.
func repoHealthy(cfg *Config, dir string) (bool, string) {
	// зависшие lock-файлы от прерванной операции
	for _, lock := range []string{"config.lock", "index.lock", "HEAD.lock"} {
		p := filepath.Join(dir, ".git", lock)
		if st, err := os.Stat(p); err == nil {
			if time.Since(st.ModTime()) > time.Hour {
				if os.Remove(p) != nil {
					return false, "остался " + lock
				}
			} else {
				return false, "идёт другая git-операция (" + lock + ")"
			}
		}
	}
	if _, err := git(cfg, dir, "rev-parse", "--git-dir"); err != nil {
		return false, "не читается .git"
	}
	if _, err := git(cfg, dir, "remote", "get-url", "origin"); err != nil {
		return false, "нет remote origin"
	}
	return true, ""
}

// quarantine переносит повреждённую копию в __broken (не удаляя данные).
func quarantine(cfg *Config, dir, key string) error {
	if err := os.MkdirAll(cfg.Broken(), 0o755); err != nil {
		return err
	}
	base := strings.ReplaceAll(key, "/", "~") + "-" + time.Now().Format("20060102-150405")
	dst := filepath.Join(cfg.Broken(), base)
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(cfg.Broken(), fmt.Sprintf("%s-%d", base, i))
	}
	return os.Rename(dir, dst)
}

func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// migrateStrays переносит репозитории, лежащие прямо в корне,
// в папку repositories (поддержка старой раскладки).
func migrateStrays(cfg *Config) {
	if cfg.Repos() == cfg.Root {
		return
	}
	entries, err := os.ReadDir(cfg.Root)
	if err != nil {
		return
	}
	moved := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "__") ||
			name == filepath.Base(cfg.Repos()) {
			continue
		}
		src := filepath.Join(cfg.Root, name)
		if !isGitRepo(src) {
			continue
		}
		dst := filepath.Join(cfg.Repos(), name)
		if _, err := os.Stat(dst); err == nil {
			log.Printf("предупреждение: %s уже есть в %s, каталог в корне оставлен как есть",
				name, filepath.Base(cfg.Repos()))
			continue
		}
		if err := os.Rename(src, dst); err == nil {
			moved++
		}
	}
	if moved > 0 {
		log.Printf("Перенесено из корня в %s: %d репозиториев", filepath.Base(cfg.Repos()), moved)
	}
}

// cloneRepo — полный клон: все ветки, все теги, подмодули.
func cloneRepo(cfg *Config, r Repo, dir string) Result {
	start := time.Now()
	// подмодули выкачиваем отдельным шагом: сбой одного из них не должен
	// оставлять нас вообще без репозитория
	if r.CloneURL == "" {
		return Result{Name: r.Key(), Action: "skipped", Detail: "сервер не сообщил адрес для клонирования"}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Result{Name: r.Key(), Action: "failed", Err: err}
	}
	args := []string{"clone", "--origin", "origin", "--jobs", "4", r.URLFor(cfg), dir}
	if _, err := gitNet(cfg, r.src, cfg.Repos(), args...); err != nil {
		return Result{Name: r.Key(), Action: "failed", Err: err, Duration: time.Since(start)}
	}
	// подтянуть все теги и создать локальные ветки под каждую удалённую
	_, _ = gitNet(cfg, r.src, dir, "fetch", "origin", "--tags", "--prune", "--quiet")
	created := syncLocalBranches(cfg, dir)

	detail := fmt.Sprintf("веток: %d, %s", created, humanSize(dirSize(dir)))
	if r.Empty {
		detail = "пустой репозиторий"
	}
	if r.PendingDelete {
		detail += "; помечен на сервере к удалению"
	}
	if note := updateSubmodules(cfg, r.src, dir); note != "" {
		detail += "; " + note
	}
	if note := describeFork(syncFork(cfg, r, dir)); note != "" {
		detail += "; " + note
	}
	return Result{
		Name:     r.Key(),
		Action:   "cloned",
		Detail:   detail,
		Duration: time.Since(start),
	}
}

// updateRepo — полная синхронизация существующего клона.
func updateRepo(cfg *Config, r Repo, dir string) Result {
	start := time.Now()
	var notes []string

	if !isGitRepo(dir) {
		return Result{Name: r.Key(), Action: "skipped", Detail: "нет .git", Duration: time.Since(start)}
	}

	// адрес origin приводим к актуальному (репозиторий мог быть переименован)
	if r.CloneURL != "" {
		if cur, err := git(cfg, dir, "remote", "get-url", "origin"); err == nil {
			want := r.URLFor(cfg)
			if cur != want && !strings.Contains(cur, "@") {
				if _, err := git(cfg, dir, "remote", "set-url", "origin", want); err == nil && cur != want {
					notes = append(notes, "origin обновлён")
				}
			}
		}
	}

	before, _ := git(cfg, dir, "rev-parse", "--verify", "-q", "HEAD")

	// все ветки и все теги, с удалением исчезнувших на сервере
	fetchArgs := []string{"fetch", "origin", "--prune", "--tags", "--quiet",
		"+refs/heads/*:refs/remotes/origin/*"}
	if _, err := gitNet(cfg, r.src, dir, fetchArgs...); err != nil {
		return Result{Name: r.Key(), Action: "failed", Err: err, Duration: time.Since(start)}
	}
	_, _ = gitNet(cfg, r.src, dir, "remote", "set-head", "origin", "--auto")

	// неполная (shallow) копия — дотягиваем историю целиком
	if out, err := git(cfg, dir, "rev-parse", "--is-shallow-repository"); err == nil && out == "true" {
		if _, err := gitNet(cfg, r.src, dir, "fetch", "--unshallow", "--quiet", "origin"); err == nil {
			notes = append(notes, "история дозагружена до полной")
		}
	}

	// текущая ветка — быстрый forward, только если рабочая копия чистая
	cur := currentBranch(cfg, dir)
	if cur != "" {
		if _, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+cur); err == nil {
			switch {
			case !isClean(cfg, dir):
				notes = append(notes, "рабочая копия изменена, "+cur+" не тронута")
			default:
				if _, err := git(cfg, dir, "merge", "--ff-only", "origin/"+cur); err != nil {
					notes = append(notes, cur+": расхождение с origin")
				}
			}
		}
	} else {
		notes = append(notes, "detached HEAD")
	}

	// форк: подтягиваем свежие ветки из upstream (локально и, если можно, на GitHub)
	if note := describeFork(syncFork(cfg, r, dir)); note != "" {
		notes = append(notes, note)
	}

	// остальные локальные ветки двигаем вперёд по refs
	updated := syncLocalBranches(cfg, dir)
	if updated > 0 {
		notes = append(notes, fmt.Sprintf("веток обновлено/создано: %d", updated))
	}

	if note := updateSubmodules(cfg, r.src, dir); note != "" {
		notes = append(notes, note)
	}
	if r.PendingDelete {
		notes = append(notes, "помечен на сервере к удалению")
	}

	after, _ := git(cfg, dir, "rev-parse", "--verify", "-q", "HEAD")
	action := "up-to-date"
	if before != after || updated > 0 {
		action = "updated"
	}
	return Result{
		Name:     r.Key(),
		Action:   action,
		Detail:   strings.Join(notes, "; "),
		Duration: time.Since(start),
	}
}

// updateSubmodules инициализирует и обновляет подмодули. Недоступный
// подмодуль (сервер лежит, репозиторий переехал) не считается фатальным:
// сам репозиторий уже выкачан и полезен.
func updateSubmodules(cfg *Config, s *Source, dir string) string {
	if !cfg.Submodules || !hasSubmodules(dir) {
		return ""
	}
	_, _ = git(cfg, dir, "submodule", "sync", "--recursive")
	if _, err := gitNet(cfg, s, dir, "submodule", "update", "--init", "--recursive", "--jobs", "4"); err != nil {
		return "подмодули выкачаны не полностью: " + shortErr(err)
	}
	return "подмодули обновлены"
}

// syncLocalBranches создаёт локальные ветки под все удалённые и продвигает
// существующие вперёд, если это перемотка без потери коммитов.
func syncLocalBranches(cfg *Config, dir string) int {
	cur := currentBranch(cfg, dir)
	changed := 0
	for _, b := range remoteBranches(cfg, dir) {
		if b == cur {
			continue // ею занимается merge --ff-only выше
		}
		remoteSHA, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+b)
		if err != nil || remoteSHA == "" {
			continue
		}
		localSHA, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/heads/"+b)
		if err != nil || localSHA == "" {
			if _, err := git(cfg, dir, "branch", "--quiet", "--track", b, "origin/"+b); err == nil {
				changed++
			}
			continue
		}
		if localSHA == remoteSHA {
			continue
		}
		// перематываем только если локальный коммит — предок удалённого
		if _, err := git(cfg, dir, "merge-base", "--is-ancestor", localSHA, remoteSHA); err != nil {
			continue // ветка разошлась — не трогаем
		}
		if _, err := git(cfg, dir, "update-ref", "refs/heads/"+b, remoteSHA, localSHA); err == nil {
			changed++
		}
	}
	return changed
}

// findGitDirs рекурсивно ищет git-репозитории (логика gitscan: служебные
// каталоги — начинающиеся с "__" или с точки — пропускаются, внутрь
// найденного репозитория не заходим).
func findGitDirs(root string) []string {
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 12 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		if isGitRepo(dir) && dir != root {
			out = append(out, dir)
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), "__") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	walk(root, 0)
	sort.Strings(out)
	return out
}

// reportOrphans сообщает о локальных каталогах, которых больше нет на сервере.
func reportOrphans(cfg *Config, listed map[string][]Repo) {
	for _, name := range sortedKeys(listed) {
		known := map[string]bool{}
		for _, r := range listed[name] {
			known[strings.ToLower(r.Path)] = true
		}
		base := filepath.Join(cfg.Repos(), name)
		var orphans []string
		for _, d := range findGitDirs(base) {
			rel, err := filepath.Rel(base, d)
			if err != nil {
				continue
			}
			if rel = filepath.ToSlash(rel); !known[strings.ToLower(rel)] {
				orphans = append(orphans, rel)
			}
		}
		if len(orphans) > 0 {
			log.Printf("%s: локально есть, но на сервере не найдено (не удаляю): %s",
				name, strings.Join(orphans, ", "))
		}
	}
	entries, err := os.ReadDir(cfg.Repos())
	if err != nil {
		return
	}
	var stray []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "__") && cfg.source(n) == nil {
			stray = append(stray, n)
		}
	}
	if len(stray) > 0 {
		log.Printf("Каталоги вне источников (не трогаю): %s", strings.Join(stray, ", "))
	}
}

func summarize(results []Result, start time.Time) {
	counts := map[string]int{}
	var failed []string
	for _, r := range results {
		counts[r.Action]++
		if r.Err != nil {
			failed = append(failed, r.Name)
		}
	}
	log.Printf("Готово за %s: склонировано %d, обновлено %d, без изменений %d, пропущено %d, ошибок %d",
		humanDuration(time.Since(start)),
		counts["cloned"], counts["updated"], counts["up-to-date"], counts["skipped"], counts["failed"])
	if len(failed) > 0 {
		log.Printf("С ошибками: %s", strings.Join(failed, ", "))
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	r := []rune(s)
	if max > 0 && len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
