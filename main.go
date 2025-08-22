//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ----- CLI flags -----

type Flags struct {
	repo       string
	repoDir    string
	statePath  string
	forceFetch bool
	renderOnly bool
	style      string // full | prompt | none
	quick      bool
}

// ----- State model (JSON-compatible with Python tool) -----

type PullRequestState struct {
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	IsDraft   bool       `json:"isDraft"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	IsOverdue bool       `json:"isOverdue"`
}

type AuthoredPullRequestState struct {
	Title        string     `json:"title"`
	URL          string     `json:"url"`
	IsDraft      bool       `json:"isDraft"`
	CreatedAt    *time.Time `json:"createdAt,omitempty"`
	IsWarningAge bool       `json:"isWarningAge"`
	IsOverdueAge bool       `json:"isOverdueAge"`
}

type CritState struct {
	GeneratedAt          time.Time                  `json:"generated_at"`
	Username             string                     `json:"username"`
	Repo                 *string                    `json:"repo"`
	RepoDir              *string                    `json:"repo_dir"`
	PullRequests         []PullRequestState         `json:"pull_requests"`
	AuthoredPullRequests []AuthoredPullRequestState `json:"authored_pull_requests"`
}

// ----- GH API (via CLI) models -----

type ghUser struct {
	Login string `json:"login"`
}

type ghPR struct {
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	IsDraft        bool     `json:"isDraft"`
	UpdatedAt      *string  `json:"updatedAt"`
	CreatedAt      *string  `json:"createdAt"`
	ReviewRequests []ghUser `json:"reviewRequests"`
}

// ----- Helpers -----

func expandUser(path string) string {
	if path == "" {
		return path
	}
	if path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func defaultStatePath() string { return filepath.Join(mustUserHomeDir(), ".crit_state.json") }

func mustUserHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("failed to get home directory: %v", err)
	}
	return h
}

func runGH(dir string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("gh error: %s", strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}

func resolveUsername() (string, error) {
	if u := os.Getenv("USERNAME"); u != "" {
		return u, nil
	}
	out, err := exec.Command("gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve username: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func fetchReviewRequested(repo, repoDir string) ([]ghPR, error) {
	args := []string{"pr", "list", "--search", "review-requested:@me", "--json", "title,url,isDraft,updatedAt,reviewRequests"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := runGH(repoDir, args...)
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

func fetchAuthored(repo, repoDir string) ([]ghPR, error) {
	args := []string{"pr", "list", "--search", "author:@me", "--json", "title,url,isDraft,createdAt"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := runGH(repoDir, args...)
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

// ----- State I/O -----

func readState(path string) (*CritState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	
	var st CritState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, nil
	}
	return &st, nil
}

func writeState(path string, st *CritState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ----- Locking -----

type fileLock struct{ f *os.File }

func tryLock(lockPath string) (*fileLock, bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	// write PID and timestamp
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
	_ = f.Sync()
	return &fileLock{f: f}, true, nil
}

func lockBlocking(lockPath string) (*fileLock, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)))
	_ = f.Sync()
	return &fileLock{f: f}, nil
}

func (l *fileLock) Unlock() {
	if l != nil && l.f != nil {
		_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
		_ = l.f.Close()
	}
}

// ----- Core logic -----

func isFresh(st *CritState) bool {
	age := time.Since(st.GeneratedAt)
	return age >= 0 && age < 15*time.Second
}

func stateMatches(st *CritState, repo, repoDir string) bool {
	if (st.Repo == nil) != (repo == "") || (st.Repo != nil && *st.Repo != repo) {
		return false
	}
	
	expanded := expandUser(repoDir)
	if (st.RepoDir == nil) != (expanded == "") || (st.RepoDir != nil && *st.RepoDir != expanded) {
		return false
	}
	
	return true
}

func makeLink(text, url string) string {
	return "\x1b]8;;" + url + "\a" + text + "\x1b]8;;\a"
}

func formatPRLine(pr PullRequestState) string {
	prefix := ""
	if pr.IsDraft {
		prefix = "[DRAFT] "
	}
	return fmt.Sprintf("%s%s - %s", prefix, pr.Title, makeLink(pr.URL, pr.URL))
}

func formatAuthoredLine(pr AuthoredPullRequestState) string {
	prefix := ""
	if pr.IsDraft {
		prefix = "[DRAFT] "
	}
	return fmt.Sprintf("%s%s - %s", prefix, pr.Title, makeLink(pr.URL, pr.URL))
}

func formatRelativeAge(when time.Time) string {
	d := time.Since(when)
	if d <= 0 {
		return "just now"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", seconds)
}

func render(st *CritState, style string) {
	switch style {
	case "none":
		return
	case "full":
		renderFull(st)
	case "prompt":
		renderPrompt(st)
	}
}

func renderFull(st *CritState) {
	fmt.Println("Reviews requested:")
	for _, pr := range st.PullRequests {
		line := formatPRLine(pr)
		if pr.IsOverdue {
			line = "\x1b[31m" + line + "\x1b[0m"
		}
		fmt.Println(line)
	}
	
	fmt.Println()
	fmt.Println("Your open PRs:")
	for _, apr := range st.AuthoredPullRequests {
		line := formatAuthoredLine(apr)
		if apr.IsOverdueAge {
			line = "\x1b[31m" + line + "\x1b[0m"
		} else if apr.IsWarningAge {
			line = "\x1b[33m" + line + "\x1b[0m"
		}
		fmt.Println(line)
	}
	
	fmt.Printf("(fetched %s ago)\n", formatRelativeAge(st.GeneratedAt))
}

func renderPrompt(st *CritState) {
	reviewCount, reviewStale := countReviews(st.PullRequests)
	authoredCount, authoredRed, authoredOrange := countAuthored(st.AuthoredPullRequests)
	
	if reviewCount == 0 && authoredCount == 0 {
		return
	}
	
	left := formatCount(reviewCount, reviewStale && reviewCount > 0, false)
	right := formatCount(authoredCount, authoredRed && authoredCount > 0, authoredOrange && authoredCount > 0)
	fmt.Print(left + "|" + right)
}

func countReviews(prs []PullRequestState) (count int, hasStale bool) {
	for _, pr := range prs {
		if !pr.IsDraft {
			count++
			if pr.IsOverdue {
				hasStale = true
			}
		}
	}
	return
}

func countAuthored(prs []AuthoredPullRequestState) (count int, hasRed, hasOrange bool) {
	count = len(prs)
	for _, pr := range prs {
		if pr.IsOverdueAge {
			hasRed = true
		} else if pr.IsWarningAge {
			hasOrange = true
		}
	}
	return
}

func formatCount(count int, red, orange bool) string {
	s := fmt.Sprintf("%d", count)
	if red {
		return "\x1b[31m" + s + "\x1b[0m"
	}
	if orange {
		return "\x1b[33m" + s + "\x1b[0m"
	}
	return s
}

func buildState(repo, repoDir string) (*CritState, error) {
	username, err := resolveUsername()
	if err != nil || username == "" {
		return nil, fmt.Errorf("unable to resolve username: %w", err)
	}
	reviewPRs, err := fetchReviewRequested(repo, repoDir)
	if err != nil {
		return nil, err
	}
	var reviewFiltered []PullRequestState
	now := time.Now().UTC()
	for _, pr := range reviewPRs {
		isRequested := false
		for _, rr := range pr.ReviewRequests {
			if rr.Login == username {
				isRequested = true
				break
			}
		}
		if !isRequested {
			continue
		}
		var updated *time.Time
		if pr.UpdatedAt != nil && *pr.UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, *pr.UpdatedAt); err == nil {
				updated = &t
			}
		}
		state := PullRequestState{
			Title:     pr.Title,
			URL:       pr.URL,
			IsDraft:   pr.IsDraft,
			UpdatedAt: updated,
		}
		if !state.IsDraft && state.UpdatedAt != nil && now.Sub(*state.UpdatedAt) > 24*time.Hour {
			state.IsOverdue = true
		}
		reviewFiltered = append(reviewFiltered, state)
	}

	authoredPRs, err := fetchAuthored(repo, repoDir)
	if err != nil {
		return nil, err
	}
	var authoredStates []AuthoredPullRequestState
	for _, pr := range authoredPRs {
		var created *time.Time
		if pr.CreatedAt != nil && *pr.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, *pr.CreatedAt); err == nil {
				created = &t
			}
		}
		state := AuthoredPullRequestState{
			Title:     pr.Title,
			URL:       pr.URL,
			IsDraft:   pr.IsDraft,
			CreatedAt: created,
		}
		if state.CreatedAt != nil {
			age := now.Sub(*state.CreatedAt)
			if age > 7*24*time.Hour {
				state.IsOverdueAge = true
			} else if age > 3*24*time.Hour {
				state.IsWarningAge = true
			}
		}
		authoredStates = append(authoredStates, state)
	}

	var repoPtr, dirPtr *string
	if repo != "" {
		repoPtr = &repo
	}
	if repoDir != "" {
		exp := expandUser(repoDir)
		dirPtr = &exp
	}
	st := &CritState{
		GeneratedAt:          now,
		Username:             username,
		Repo:                 repoPtr,
		RepoDir:              dirPtr,
		PullRequests:         reviewFiltered,
		AuthoredPullRequests: authoredStates,
	}
	return st, nil
}

func spawnBackgroundRefresh(flags Flags) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return
	}
	
	var args []string
	if flags.repo != "" {
		args = append(args, flags.repo)
	}
	if flags.repoDir != "" {
		args = append(args, "--repo-dir", flags.repoDir)
	}
	args = append(args, "--state", flags.statePath, "--force-fetch", "--style", "none")
	
	cmd := exec.Command(exe, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Start()
}

func execute(flags Flags) int {
	// Render-only path
	if flags.renderOnly {
		st, _ := readState(flags.statePath)
		if st != nil {
			render(st, flags.style)
			if flags.quick && stateMatches(st, flags.repo, flags.repoDir) && !isFresh(st) {
				spawnBackgroundRefresh(flags)
			}
		}
		return 0
	}

	// Cache path
	if !flags.forceFetch {
		st, err := readState(flags.statePath)
		if err != nil {
			log.Fatal(err)
		}
		if st != nil {
			if flags.quick && stateMatches(st, flags.repo, flags.repoDir) {
				render(st, flags.style)
				if !isFresh(st) {
					spawnBackgroundRefresh(flags)
				}
				return 0
			}
			if isFresh(st) && stateMatches(st, flags.repo, flags.repoDir) {
				render(st, flags.style)
				return 0
			}
		}
	}

	// Acquire lock
	lockPath := flags.statePath + ".lock"
	l, ok, _ := tryLock(lockPath)
	if !ok {
		// wait for lock
		bl, err := lockBlocking(lockPath)
		if err == nil && bl != nil {
			// re-check freshness under lock
			if st, _ := readState(flags.statePath); st != nil && isFresh(st) && stateMatches(st, flags.repo, flags.repoDir) {
				bl.Unlock()
				render(st, flags.style)
				return 0
			}
			l = bl
		}
	}
	defer l.Unlock()

	// Build and write state
	st, err := buildState(flags.repo, flags.repoDir)
	if err == nil {
		_ = writeState(flags.statePath, st)
		render(st, flags.style)
	}
	return 0
}

func parseFlags() Flags {
	var f Flags
	flag.StringVar(&f.repoDir, "repo-dir", "", "Path to local git repo (sets gh working directory)")
	flag.StringVar(&f.statePath, "state", defaultStatePath(), "Path to state file")
	flag.BoolVar(&f.forceFetch, "force-fetch", false, "Force fetching even if cached state is fresh")
	flag.BoolVar(&f.renderOnly, "render-only", false, "Only render cached state; do not fetch")
	flag.StringVar(&f.style, "style", "full", "Output style: full|prompt|none")
	flag.BoolVar(&f.quick, "quick", false, "Render stale cache immediately and refresh in background")
	flag.Parse()
	
	if args := flag.Args(); len(args) > 0 {
		f.repo = args[0]
	}
	
	f.statePath = expandUser(f.statePath)
	
	// If repo-dir wasn't specified, try to get it from persisted state
	if f.repoDir == "" {
		if st, _ := readState(f.statePath); st != nil && st.RepoDir != nil {
			f.repoDir = *st.RepoDir
		} else {
			// No persisted repo-dir found, require it as a flag
			fmt.Fprintf(os.Stderr, "Error: --repo-dir is required when no previous state exists\n")
			flag.Usage()
			os.Exit(1)
		}
	} else {
		f.repoDir = expandUser(f.repoDir)
	}
	
	return f
}

func main() {
	f := parseFlags()
	os.Exit(execute(f))
}
