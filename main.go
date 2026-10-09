// pastor-sync — полная синхронизация репозиториев GitHub и GitLab с локальной папкой
// и ведение реестра репозиториев.
//
// Основан на https://github.com/Pastor/gitscan (рекурсивный обход .git-каталогов
// и обновление их через git fetch/pull/submodule), расширен до:
//   - нескольких источников: github.com и любое число инстансов GitLab (gitscan.json);
//   - получения списка репозиториев через API (включая приватные, по токену);
//   - клонирования отсутствующих репозиториев;
//   - полной синхронизации: все ветки, все теги, все подмодули;
//   - генерации реестра registry.json / REGISTRY.md / registry.csv.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const version = "2.0.0"

// toolName — имя, под которым инструмент запущен. Один и тот же исходник
// собирается и как gitscan, и как pastor-sync; в подсказках показываем то,
// что пользователь реально набрал.
var toolName = execName()

func execName() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "gitscan"
	}
	n := filepath.Base(os.Args[0])
	return strings.TrimSuffix(n, ".exe")
}

// Config — параметры запуска.
type Config struct {
	Root         string // корневая папка (реестр, бинарник, служебные каталоги)
	ReposDir     string // подпапка с репозиториями, относительно Root
	User         string // владелец репозиториев на GitHub (основной GitHub-источник)
	Token        string // токен GitHub из флага -token (основной GitHub-источник)
	ConfigFile   string // файл источников, по умолчанию <Root>/gitscan.json
	SourceFilter string // работать только с этими источниками (через запятую)
	Sources      []*Source
	Jobs         int    // число параллельных потоков
	Forks        bool   // включать форки
	Member       bool   // включать репозитории, где пользователь участник, а не владелец
	Archived     bool   // включать архивные репозитории
	Submodules   bool   // выкачивать подмодули
	ForkSync     bool   // подтягивать в форки изменения из upstream
	ForkPush     bool   // обновлять форк и на GitHub (нужен токен)
	ForkTags     bool   // забирать в форк ещё и теги upstream
	SSH          bool   // клонировать по ssh вместо https
	DryRun       bool   // ничего не менять, только показать план
	Only         string // фильтр по подстроке в имени репозитория
	EnvFile      string // путь к .env, откуда брать токен
	NoRegistry   bool   // не генерировать реестр после синхронизации
	Timeout      time.Duration
	Verbose      bool
	Budget       time.Duration // ограничение времени на пересборку описаний реестра
}

// Служебные каталоги. Каталоги, начинающиеся с точки или с "__",
// при обходе пропускаются (соглашение унаследовано от gitscan).
const (
	syncDirName   = ".sync"
	brokenDirName = "__broken"
	defaultRepos  = "repositories"
)

// Repos возвращает абсолютный путь к папке с репозиториями.
func (c *Config) Repos() string {
	if filepath.IsAbs(c.ReposDir) {
		return c.ReposDir
	}
	if c.ReposDir == "" || c.ReposDir == "." {
		return c.Root
	}
	return filepath.Join(c.Root, c.ReposDir)
}

// Broken — каталог карантина для повреждённых копий.
func (c *Config) Broken() string { return filepath.Join(c.Root, brokenDirName) }

func usage() {
	fmt.Fprintf(os.Stderr, `%s %s — синхронизация репозиториев GitHub и GitLab с локальной папкой

Использование:
  %[1]s [команда] [флаги]

Команды:
  sync       (по умолчанию) получить списки репозиториев всех источников, склонировать
             новые, обновить существующие (все ветки, теги, подмодули) и пересобрать реестр
  scan       режим gitscan: обновить все git-репозитории, найденные в папке,
             без обращения к API
  registry   только пересобрать реестр по локальному состоянию
  token      показать, откуда взяты токены источников, и проверить их
  list       показать списки репозиториев на серверах, ничего не меняя
  sources    показать настроенные источники
  source add -name work -url https://git.company.ru -token glpat-...
             добавить источник GitLab (флаги: %[1]s source add -h)
  source remove work
             убрать источник из конфига (локальные копии остаются)
  version    версия

Флаги:
`, toolName, version)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Структура папки:
  <папка>/gitscan.json                  источники (без файла — только GitHub)
  <папка>/repositories/<источник>/<путь>/   сами репозитории
  <папка>/REGISTRY.md, registry.json, registry.csv, registry-branches.csv
  <папка>/%s/                       исходники, сборки, кэш API, логи
  <папка>/%s/                     карантин повреждённых копий

Токен основного GitHub-источника ищется в таком порядке:
  1. флаг -token
  2. переменные окружения GITHUB_TOKEN, GH_TOKEN, GITHUB_PAT, GH_PAT,
     GITHUB_ACCESS_TOKEN, PASTOR_SYNC_TOKEN
  3. файлы .env: путь из -env, затем <папка>/.env, <папка>/%s/.env,
     ./.env, ~/.env, ~/.config/pastor-sync/.env
  4. файлы с «голым» токеном: <папка>/%s/token, ~/.github_token
Токен GitLab-источника: token_env / token_file из конфига, <папка>/%s/tokens/<имя>,
переменная GITLAB_TOKEN_<ИМЯ> в окружении или .env.
Проверить, что нашлось: %[1]s token
Без токена доступны только публичные репозитории GitHub.

Примеры:
  %[1]s                       # полная синхронизация в текущей папке
  %[1]s -jobs 8               # в 8 потоков
  %[1]s -only stm8            # только репозитории с "stm8" в пути
  %[1]s -source work          # только источник work
  %[1]s -forks=false          # пропустить форки
  %[1]s registry              # пересобрать только реестр
  %[1]s scan                  # обновить всё, что лежит в папке, без API
`, toolName, syncDirName, brokenDirName, syncDirName, syncDirName, syncDirName)
}

func main() {
	cfg := &Config{}
	cwd, _ := os.Getwd()

	command := "sync"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	if command == "source" {
		cmdSource(args, cwd)
		return
	}

	fs := flag.NewFlagSet(toolName, flag.ExitOnError)
	fs.Usage = usage
	flag.CommandLine = fs

	fs.StringVar(&cfg.Root, "dir", cwd, "корневая папка (реестр и служебные каталоги)")
	fs.StringVar(&cfg.Root, "scan_dir", cwd, "синоним -dir (совместимость с gitscan)")
	fs.StringVar(&cfg.ReposDir, "repos", defaultRepos, "подпапка с репозиториями относительно -dir")
	fs.StringVar(&cfg.User, "user", envOr("GITHUB_USER", "Pastor"), "владелец репозиториев на GitHub")
	fs.StringVar(&cfg.Token, "token", "", "токен GitHub (иначе берётся из окружения, .env или файла)")
	fs.StringVar(&cfg.EnvFile, "env", "", "путь к .env, откуда взять токен")
	fs.StringVar(&cfg.ConfigFile, "config", "", "файл источников (по умолчанию <папка>/"+configName+")")
	fs.StringVar(&cfg.SourceFilter, "source", "", "работать только с этими источниками (через запятую)")
	fs.IntVar(&cfg.Jobs, "jobs", defaultJobs(), "число параллельных потоков")
	fs.BoolVar(&cfg.Forks, "forks", true, "включать форки")
	fs.BoolVar(&cfg.Member, "member", true, "включать репозитории других владельцев, где пользователь участник")
	fs.BoolVar(&cfg.Archived, "archived", true, "включать архивные репозитории")
	fs.BoolVar(&cfg.Submodules, "submodules", true, "выкачивать подмодули рекурсивно")
	fs.BoolVar(&cfg.ForkSync, "fork-sync", true, "подтягивать в форки свежие ветки из upstream")
	fs.BoolVar(&cfg.ForkPush, "fork-push", true, "обновлять форк и на GitHub (нужен токен)")
	fs.BoolVar(&cfg.ForkTags, "fork-tags", false, "забирать в форк также теги upstream")
	fs.BoolVar(&cfg.SSH, "ssh", false, "клонировать по ssh вместо https")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "ничего не менять, только показать план")
	fs.StringVar(&cfg.Only, "only", "", "обрабатывать только репозитории с этой подстрокой в пути <источник>/<путь>")
	fs.BoolVar(&cfg.NoRegistry, "no-registry", false, "не пересобирать реестр после синхронизации")
	fs.DurationVar(&cfg.Timeout, "timeout", 60*time.Minute, "таймаут на одну git-операцию")
	fs.BoolVar(&cfg.Verbose, "v", false, "подробный вывод git")
	fs.DurationVar(&cfg.Budget, "registry-budget", 0, "ограничить время пересборки описаний в реестре (0 — без ограничения)")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		fatal("не удалось разобрать путь %q: %v", cfg.Root, err)
	}
	cfg.Root = abs
	if cfg.Jobs < 1 {
		cfg.Jobs = 1
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if !explicit["user"] && os.Getenv("GITHUB_USER") == "" {
		if u := FindUser(cfg); u != "" {
			cfg.User = u
		}
	}

	closeLog := setupLog(cfg)
	defer closeLog()

	if err := loadSources(cfg, explicit); err != nil {
		fatal("%v", err)
	}

	switch command {
	case "version":
		fmt.Printf("%s %s\n", toolName, version)
	case "list":
		cmdList(cfg)
	case "sync":
		cmdSync(cfg)
	case "scan":
		cmdScan(cfg)
	case "registry":
		cmdRegistry(cfg)
	case "token":
		cmdToken(cfg)
	case "sources":
		cmdSources(cfg)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", command)
		usage()
		os.Exit(2)
	}
}

func defaultJobs() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 2 {
		n = 2
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// setupLog дублирует вывод в файл <root>/.sync/logs/sync-<ts>.log
func setupLog(cfg *Config) func() {
	log.SetFlags(log.Ldate | log.Ltime)
	dir := filepath.Join(cfg.Root, syncDirName, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}
	name := filepath.Join(dir, "sync-"+time.Now().Format("20060102-150405")+".log")
	f, err := os.Create(name)
	if err != nil {
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	return func() { _ = f.Close() }
}

func fatal(format string, a ...interface{}) {
	log.Printf("ОШИБКА: "+format, a...)
	os.Exit(1)
}
