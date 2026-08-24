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

// cmdToken показывает, откуда взят токен, и проверяет его на GitHub.
func cmdToken(cfg *Config) {
	fmt.Println("Где искался токен:")
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

	if cfg.Token == "" {
		fmt.Println("\nТокен не найден — доступны только публичные репозитории,")
		fmt.Println("лимит GitHub API 60 запросов в час, форки на GitHub обновляться не будут.")
		fmt.Printf("Проще всего: создайте файл %s со строкой\n  GITHUB_TOKEN=ghp_...\n",
			filepath.Join(cfg.Root, ".env"))
		return
	}

	fmt.Printf("\nИспользуется: %s\n", maskToken(cfg.Token))
	fmt.Printf("Источник:     %s\n", cfg.TokenFrom)

	var who struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	resp, err := apiGet(cfg, apiBase+"/user", &who)
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
	if !strings.EqualFold(who.Login, cfg.User) {
		fmt.Printf("\n⚠ Токен принадлежит %s, а синхронизируется аккаунт %s.\n", who.Login, cfg.User)
		fmt.Printf("  Если это не нарочно — укажите -user %s\n", who.Login)
	}
}

// cmdList печатает список репозиториев на GitHub.
func cmdList(cfg *Config) {
	repos, err := FetchRepos(cfg)
	if err != nil {
		fatal("%v", err)
	}
	repos = Filter(cfg, repos)
	own, forks, member, priv := 0, 0, 0, 0
	for _, r := range repos {
		kind := "свой"
		switch {
		case !strings.EqualFold(r.Owner.Login, cfg.User):
			kind = "участник"
			member++
		case r.Fork:
			kind = "форк"
			forks++
		default:
			own++
		}
		vis := "public"
		if r.Private {
			vis = "private"
			priv++
		}
		fmt.Printf("%-40s %-9s %-8s %-10s %9s  %s\n",
			r.Name, kind, vis, r.DefaultBranch, humanSize(r.Size*1024), oneLine(r.Description, 60))
	}
	fmt.Printf("\nВсего: %d (своих %d, форков %d, чужих с участием %d, приватных %d)\n",
		len(repos), own, forks, member, priv)
}

// cmdSync — основная команда: список с GitHub, клонирование новых, обновление старых.
func cmdSync(cfg *Config) {
	start := time.Now()
	cleanup := setupAskPass(cfg)
	defer cleanup()

	if err := os.MkdirAll(cfg.Repos(), 0o755); err != nil {
		fatal("не удалось создать %s: %v", cfg.Repos(), err)
	}
	migrateStrays(cfg)

	auth := "без токена (только публичные репозитории)"
	if cfg.Token != "" {
		auth = "с токеном из " + cfg.TokenFrom
	}
	log.Printf("Синхронизация %s -> %s, %s, потоков: %d", cfg.User, cfg.Root, auth, cfg.Jobs)

	repos, err := FetchRepos(cfg)
	if err != nil {
		fatal("%v", err)
	}
	repos = DedupeByDir(cfg, repos)
	repos = EnrichForks(cfg, repos)
	saveRepoCache(cfg, repos)
	all := repos
	repos = Filter(cfg, repos)
	log.Printf("Получено репозиториев: %d, к обработке: %d", len(all), len(repos))

	if cfg.DryRun {
		for _, r := range repos {
			dir := filepath.Join(cfg.Repos(), r.Name)
			state := "обновить"
			if !isGitRepo(dir) {
				state = "склонировать"
			}
			log.Printf("[план] %-12s %s", state, r.Name)
		}
		log.Printf("Пробный запуск, изменений не внесено")
		return
	}

	results := runParallel(cfg, repos, func(r Repo) Result {
		return syncRepo(cfg, r)
	})

	reportOrphans(cfg, all)
	summarize(results, start)

	if !cfg.NoRegistry {
		log.Printf("Сборка реестра...")
		if err := BuildRegistry(cfg); err != nil {
			log.Printf("предупреждение: реестр собрать не удалось: %v", err)
		}
	}
}

// cmdScan — режим gitscan: обновить всё, что уже лежит в папке, без GitHub API.
func cmdScan(cfg *Config) {
	start := time.Now()
	cleanup := setupAskPass(cfg)
	defer cleanup()

	migrateStrays(cfg)
	dirs := findGitDirs(cfg.Repos())
	log.Printf("Найдено git-репозиториев: %d в %s", len(dirs), cfg.Repos())

	cache := map[string]Repo{}
	for _, r := range loadRepoCache(cfg) {
		cache[strings.ToLower(r.Name)] = r
	}

	var repos []Repo
	for _, d := range dirs {
		name := filepath.Base(d)
		if cfg.Only != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(cfg.Only)) {
			continue
		}
		if r, ok := cache[strings.ToLower(name)]; ok {
			repos = append(repos, r)
			continue
		}
		repos = append(repos, Repo{Name: name})
	}

	results := runParallel(cfg, repos, func(r Repo) Result {
		return updateRepo(cfg, r, filepath.Join(cfg.Repos(), r.Name))
	})
	summarize(results, start)

	if !cfg.NoRegistry {
		if err := BuildRegistry(cfg); err != nil {
			log.Printf("предупреждение: реестр собрать не удалось: %v", err)
		}
	}
}

func cmdRegistry(cfg *Config) {
	if err := BuildRegistry(cfg); err != nil {
		fatal("%v", err)
	}
}

// runParallel обрабатывает репозитории пулом воркеров.
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
				res := fn(r)
				mu.Lock()
				done++
				n := done
				results = append(results, res)
				mu.Unlock()
				status := res.Action
				if res.Err != nil {
					status = "ОШИБКА"
				}
				log.Printf("[%d/%d] %-38s %-12s %s %s", n, total, res.Name, status,
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
	dir := filepath.Join(cfg.Repos(), r.Name)
	if isGitRepo(dir) {
		if healthy, why := repoHealthy(cfg, dir); !healthy {
			if err := quarantine(cfg, dir); err != nil {
				return Result{Name: r.Name, Action: "skipped",
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
		} else {
			return Result{Name: r.Name, Action: "skipped",
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
func quarantine(cfg *Config, dir string) error {
	if err := os.MkdirAll(cfg.Broken(), 0o755); err != nil {
		return err
	}
	base := filepath.Base(dir) + "-" + time.Now().Format("20060102-150405")
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
	args := []string{"clone", "--origin", "origin", "--jobs", "4", r.URLFor(cfg), dir}
	if _, err := git(cfg, cfg.Repos(), args...); err != nil {
		return Result{Name: r.Name, Action: "failed", Err: err, Duration: time.Since(start)}
	}
	// подтянуть все теги и создать локальные ветки под каждую удалённую
	_, _ = git(cfg, dir, "fetch", "origin", "--tags", "--prune", "--quiet")
	created := syncLocalBranches(cfg, dir)

	detail := fmt.Sprintf("веток: %d, %s", created, humanSize(dirSize(dir)))
	if note := updateSubmodules(cfg, dir); note != "" {
		detail += "; " + note
	}
	if note := describeFork(syncFork(cfg, r, dir)); note != "" {
		detail += "; " + note
	}
	return Result{
		Name:     r.Name,
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
		return Result{Name: r.Name, Action: "skipped", Detail: "нет .git", Duration: time.Since(start)}
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
	if _, err := git(cfg, dir, fetchArgs...); err != nil {
		// --prune-tags поддерживается не везде; повторяем без него уже внутри fetchArgs,
		// поэтому здесь ошибка означает реальную проблему связи/доступа
		return Result{Name: r.Name, Action: "failed", Err: err, Duration: time.Since(start)}
	}
	_, _ = git(cfg, dir, "remote", "set-head", "origin", "--auto")

	// неполная (shallow) копия — дотягиваем историю целиком
	if out, err := git(cfg, dir, "rev-parse", "--is-shallow-repository"); err == nil && out == "true" {
		if _, err := git(cfg, dir, "fetch", "--unshallow", "--quiet", "origin"); err == nil {
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

	if note := updateSubmodules(cfg, dir); note != "" {
		notes = append(notes, note)
	}

	after, _ := git(cfg, dir, "rev-parse", "--verify", "-q", "HEAD")
	action := "up-to-date"
	if before != after || updated > 0 {
		action = "updated"
	}
	return Result{
		Name:     r.Name,
		Action:   action,
		Detail:   strings.Join(notes, "; "),
		Duration: time.Since(start),
	}
}

// updateSubmodules инициализирует и обновляет подмодули. Недоступный
// подмодуль (сервер лежит, репозиторий переехал) не считается фатальным:
// сам репозиторий уже выкачан и полезен.
func updateSubmodules(cfg *Config, dir string) string {
	if !cfg.Submodules || !hasSubmodules(dir) {
		return ""
	}
	_, _ = git(cfg, dir, "submodule", "sync", "--recursive")
	if _, err := git(cfg, dir, "submodule", "update", "--init", "--recursive", "--jobs", "4"); err != nil {
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
		if depth > 4 {
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

// reportOrphans сообщает о локальных каталогах, которых нет на GitHub.
func reportOrphans(cfg *Config, repos []Repo) {
	known := map[string]bool{}
	for _, r := range repos {
		known[strings.ToLower(r.Name)] = true
	}
	entries, err := os.ReadDir(cfg.Repos())
	if err != nil {
		return
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "__") {
			continue
		}
		if !known[strings.ToLower(e.Name())] && isGitRepo(filepath.Join(cfg.Repos(), e.Name())) {
			orphans = append(orphans, e.Name())
		}
	}
	if len(orphans) > 0 {
		log.Printf("Локально есть, но на GitHub у %s не найдено (не удаляю): %s",
			cfg.User, strings.Join(orphans, ", "))
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
