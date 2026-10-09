package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ForkStatus — результат синхронизации форка с upstream.
type ForkStatus struct {
	Upstream   string   `json:"upstream,omitempty"`
	Checked    bool     `json:"checked"`
	InSync     []string `json:"in_sync,omitempty"`       // ветки совпадают с upstream
	Updated    []string `json:"updated,omitempty"`       // ветки перемотаны вперёд
	PushedTo   []string `json:"pushed_remote,omitempty"` // ветки обновлены и на GitHub
	Diverged   []string `json:"diverged,omitempty"`      // ветки разошлись, автоматом не трогаем
	Behind     []string `json:"behind,omitempty"`        // отстают, но обновить не удалось
	OnlyInFork []string `json:"only_in_fork,omitempty"`  // веток нет в upstream
	Note       string   `json:"note,omitempty"`
}

// ---------- GitHub API: серверная перемотка форка ----------

// mergeUpstream просит GitHub перемотать ветку форка на upstream
// (то же, что кнопка «Sync fork» в интерфейсе). Возвращает true,
// если ветка была обновлена.
func mergeUpstream(s *Source, fullName, branch string) (bool, error) {
	if s.Token == "" {
		return false, fmt.Errorf("нужен токен")
	}
	body, _ := json.Marshal(map[string]string{"branch": branch})
	req, err := http.NewRequest("POST", githubProvider{s}.api()+"/repos/"+fullName+"/merge-upstream", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gitscan-sync/"+version)
	req.Header.Set("Authorization", "Bearer "+s.Token)

	resp, raw, err := httpDo(s.client, req)
	if err != nil {
		return false, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var out struct {
			MergeType string `json:"merge_type"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.MergeType != "none", nil
	case resp.StatusCode == 409:
		return false, fmt.Errorf("конфликт с upstream")
	case resp.StatusCode == 422:
		return false, fmt.Errorf("GitHub отказался перематывать ветку")
	default:
		msg := string(raw)
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
}

// ---------- собственно синхронизация ----------

// syncFork подтягивает в форк свежие ветки из upstream: локально всегда,
// на GitHub — если разрешено и есть токен. Только перемотка вперёд,
// разошедшиеся ветки не трогаются. Форки GitLab пока не синхронизируются.
func syncFork(cfg *Config, r Repo, dir string) *ForkStatus {
	s := r.src
	if !cfg.ForkSync || !r.Fork || r.Parent == nil || r.Parent.Path == "" || s == nil || s.Type != "github" {
		return nil
	}
	st := &ForkStatus{Upstream: r.Parent.Path, Checked: true}
	upURL := r.Parent.CloneURL
	if upURL == "" {
		upURL = s.webBase() + "/" + r.Parent.Path + ".git"
	}

	if cur, err := git(cfg, dir, "remote", "get-url", "upstream"); err != nil {
		if _, err := git(cfg, dir, "remote", "add", "upstream", upURL); err != nil {
			st.Note = "не удалось добавить remote upstream"
			return st
		}
	} else if cur != upURL && !strings.Contains(cur, "@") {
		_, _ = git(cfg, dir, "remote", "set-url", "upstream", upURL)
	}

	fetchArgs := []string{"fetch", "upstream", "--prune", "--quiet",
		"+refs/heads/*:refs/remotes/upstream/*"}
	if cfg.ForkTags {
		fetchArgs = append(fetchArgs, "--tags")
	} else {
		fetchArgs = append(fetchArgs, "--no-tags")
	}
	if _, err := gitNet(cfg, s, dir, fetchArgs...); err != nil {
		st.Note = "upstream недоступен: " + shortErr(err)
		return st
	}

	upBranches, err := gitLines(cfg, dir, "for-each-ref", "--format=%(refname:strip=3)", "refs/remotes/upstream")
	if err != nil {
		st.Note = "не удалось прочитать ветки upstream"
		return st
	}
	upSet := map[string]bool{}
	for _, b := range upBranches {
		if b != "" && b != "HEAD" {
			upSet[b] = true
		}
	}

	originBranches := remoteBranches(cfg, dir)
	pushedAny := false

	for _, b := range originBranches {
		if !upSet[b] {
			st.OnlyInFork = append(st.OnlyInFork, b)
			continue
		}
		originSHA, err1 := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+b)
		upSHA, err2 := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/remotes/upstream/"+b)
		if err1 != nil || err2 != nil {
			continue
		}
		if originSHA == upSHA {
			st.InSync = append(st.InSync, b)
			continue
		}
		// перематываем только если форк — предок upstream
		if _, err := git(cfg, dir, "merge-base", "--is-ancestor", originSHA, upSHA); err != nil {
			st.Diverged = append(st.Diverged, b)
			continue
		}

		remoteOK := false
		if cfg.ForkPush {
			if s.Token != "" {
				if _, err := mergeUpstream(s, r.Path, b); err == nil {
					remoteOK = true
				} else if _, perr := gitNet(cfg, s, dir, "push", "origin",
					"refs/remotes/upstream/"+b+":refs/heads/"+b); perr == nil {
					remoteOK = true
				} else {
					st.Behind = append(st.Behind, b+" (GitHub: "+shortErr(err)+")")
				}
			} else if st.Note == "" {
				st.Note = "форк на GitHub не обновлён — нужен токен"
			}
		}
		if remoteOK {
			st.PushedTo = append(st.PushedTo, b)
			pushedAny = true
		}

		// локально двигаем ветку на upstream
		if moved := fastForwardLocal(cfg, dir, b, upSHA); moved {
			st.Updated = append(st.Updated, b)
		} else if !remoteOK {
			st.Behind = append(st.Behind, b)
		}
	}

	if pushedAny {
		// подтягиваем обновлённые ссылки origin, чтобы реестр не врал
		_, _ = gitNet(cfg, s, dir, "fetch", "origin", "--prune", "--quiet",
			"+refs/heads/*:refs/remotes/origin/*")
	}

	sort.Strings(st.InSync)
	sort.Strings(st.Updated)
	sort.Strings(st.PushedTo)
	sort.Strings(st.Diverged)
	sort.Strings(st.OnlyInFork)
	return st
}

// fastForwardLocal перематывает локальную ветку на указанный коммит,
// если это возможно без потери коммитов.
func fastForwardLocal(cfg *Config, dir, branch, target string) bool {
	cur := currentBranch(cfg, dir)
	localSHA, err := git(cfg, dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil || localSHA == "" {
		_, err := git(cfg, dir, "branch", "--quiet", branch, target)
		return err == nil
	}
	if localSHA == target {
		return false
	}
	if _, err := git(cfg, dir, "merge-base", "--is-ancestor", localSHA, target); err != nil {
		return false
	}
	if branch == cur {
		if !isClean(cfg, dir) {
			return false
		}
		_, err := git(cfg, dir, "merge", "--ff-only", target)
		return err == nil
	}
	_, err = git(cfg, dir, "update-ref", "refs/heads/"+branch, target, localSHA)
	return err == nil
}

// describeFork — короткая строка для лога.
func describeFork(st *ForkStatus) string {
	if st == nil || !st.Checked {
		return ""
	}
	var parts []string
	if n := len(st.Updated); n > 0 {
		parts = append(parts, fmt.Sprintf("из upstream перемотано веток: %d", n))
	}
	if n := len(st.PushedTo); n > 0 {
		parts = append(parts, fmt.Sprintf("на GitHub обновлено: %d", n))
	}
	if n := len(st.Diverged); n > 0 {
		parts = append(parts, fmt.Sprintf("разошлись с upstream: %d", n))
	}
	if st.Note != "" {
		parts = append(parts, st.Note)
	}
	return strings.Join(parts, ", ")
}
