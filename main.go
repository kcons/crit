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
	"sort"
	"text/template"
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
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	IsDraft       bool       `json:"isDraft"`
	CreatedAt     *time.Time `json:"createdAt,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
	IsWarningAge  bool       `json:"isWarningAge"`
	IsOverdueAge  bool       `json:"isOverdueAge"`
	NeedsResponse bool       `json:"needsResponse"`
	IsApproved    bool       `json:"isApproved"`
	ChecksPassed  bool       `json:"checksPassed"`
	HasConflict   bool       `json:"hasConflict"`
}

type CritState struct {
	GeneratedAt          time.Time                  `json:"generated_at"`
	FetchDuration        time.Duration              `json:"fetch_duration,omitempty"`
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

type ghCheckRun struct {
	TypeName   string `json:"__typename"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"` // StatusContext uses state instead of status/conclusion
}

type ghPR struct {
	Title              string       `json:"title"`
	URL                string       `json:"url"`
	IsDraft            bool         `json:"isDraft"`
	UpdatedAt          *string      `json:"updatedAt"`
	CreatedAt          *string      `json:"createdAt"`
	ReviewRequests     []ghUser     `json:"reviewRequests"`
	ReviewDecision     string       `json:"reviewDecision"`
	StatusCheckRollup  []ghCheckRun `json:"statusCheckRollup"`
	Mergeable          string       `json:"mergeable"`
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
	args := []string{"pr", "list", "--search", "author:@me", "--json", "title,url,isDraft,createdAt,updatedAt,reviewDecision,statusCheckRollup,mergeable"}
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

func parseTimePtr(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, *s); err == nil {
		return &t
	}
	return nil
}

func makeLink(text, url string) string {
	return "\x1b]8;;" + url + "\a" + text + "\x1b]8;;\a"
}

func formatPRLine(pr PullRequestState) string {
	prefix := ""
	if pr.IsDraft {
		prefix = "[DRAFT] "
	}
	return fmt.Sprintf("%s%s\n    %s", prefix, pr.Title, makeLink(pr.URL, pr.URL))
}

func formatAuthoredLine(pr AuthoredPullRequestState) string {
	prefix := ""
	if pr.HasConflict {
		prefix = "\u274C "
	} else if pr.NeedsResponse {
		prefix = "\u2757"
	} else if !pr.IsDraft && pr.IsApproved && pr.ChecksPassed {
		prefix = "\U0001F6A2 "
	}
	if pr.IsDraft {
		prefix += "[DRAFT] "
	}
	return fmt.Sprintf("%s%s\n    %s", prefix, pr.Title, makeLink(pr.URL, pr.URL))
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
	case "log":
		renderLog(st)
	}
}

func reviewPROrder(pr PullRequestState) int {
	// overdue non-draft=0, non-draft=1, overdue draft=2, draft=3
	base := 0
	if !pr.IsOverdue {
		base = 1
	}
	if pr.IsDraft {
		base += 2
	}
	return base
}

func authoredPROrder(pr AuthoredPullRequestState) int {
	// color group: overdue=0, warning=1, normal=2; draft adds 3
	base := 2
	if pr.IsOverdueAge {
		base = 0
	} else if pr.IsWarningAge {
		base = 1
	}
	if pr.IsDraft {
		base += 3
	}
	return base
}

func updatedAtDesc(a, b *time.Time) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

func renderFull(st *CritState) {
	sort.Slice(st.PullRequests, func(i, j int) bool {
		oi, oj := reviewPROrder(st.PullRequests[i]), reviewPROrder(st.PullRequests[j])
		if oi != oj {
			return oi < oj
		}
		return updatedAtDesc(st.PullRequests[i].UpdatedAt, st.PullRequests[j].UpdatedAt)
	})
	sort.Slice(st.AuthoredPullRequests, func(i, j int) bool {
		oi, oj := authoredPROrder(st.AuthoredPullRequests[i]), authoredPROrder(st.AuthoredPullRequests[j])
		if oi != oj {
			return oi < oj
		}
		return updatedAtDesc(st.AuthoredPullRequests[i].UpdatedAt, st.AuthoredPullRequests[j].UpdatedAt)
	})

	if len(st.PullRequests) > 0 {
		fmt.Println("Reviews requested:")
		for _, pr := range st.PullRequests {
			line := formatPRLine(pr)
			if pr.IsOverdue {
				line = "\x1b[31m" + line + "\x1b[0m"
			}
			fmt.Println(line)
		}
		fmt.Println()
	}

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

func formatPromptString(st *CritState, useColors bool) string {
	reviewCount, reviewStale := countReviews(st.PullRequests)
	authoredCount, authoredRed, authoredOrange, authoredNeedsResponse := countAuthored(st.AuthoredPullRequests)
	
	if reviewCount == 0 && authoredCount == 0 {
		return "👍"
	}
	
	parts := []string{}
	
	if reviewCount > 0 {
		var countStr string
		if useColors {
			countStr = formatCount(reviewCount, reviewStale, false)
		} else {
			countStr = fmt.Sprintf("%d", reviewCount)
		}
		parts = append(parts, "🔍"+countStr)
	}
	
	if authoredCount > 0 {
		var countStr string
		if useColors {
			countStr = formatCount(authoredCount, authoredRed, authoredOrange)
		} else {
			countStr = fmt.Sprintf("%d", authoredCount)
		}
		if authoredNeedsResponse {
			countStr += "\u2757"
		}
		parts = append(parts, "🚢"+countStr)
	}

	if len(parts) == 1 {
		return parts[0]
	}
	if len(parts) == 2 {
		return parts[0] + " " + parts[1]
	}
	
	return ""
}

func renderPrompt(st *CritState) {
	fmt.Print(formatPromptString(st, true))
}

func renderLog(st *CritState) {
	reviewCount, reviewStale := countReviews(st.PullRequests)
	authoredCount, _, _, _ := countAuthored(st.AuthoredPullRequests)
	staleTag := ""
	if reviewStale {
		staleTag = " STALE"
	}
	fmt.Printf("%s fetch=%s reviews=%d authored=%d drafts=%d%s\n",
		st.GeneratedAt.Format(time.RFC3339),
		st.FetchDuration.Round(time.Millisecond),
		reviewCount,
		authoredCount,
		countDrafts(st),
		staleTag,
	)
}

func countDrafts(st *CritState) int {
	n := 0
	for _, pr := range st.PullRequests {
		if pr.IsDraft {
			n++
		}
	}
	for _, pr := range st.AuthoredPullRequests {
		if pr.IsDraft {
			n++
		}
	}
	return n
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

func countAuthored(prs []AuthoredPullRequestState) (count int, hasRed, hasOrange, hasNeedsResponse bool) {
	count = len(prs)
	for _, pr := range prs {
		if pr.IsOverdueAge {
			hasRed = true
		} else if pr.IsWarningAge {
			hasOrange = true
		}
		if pr.NeedsResponse {
			hasNeedsResponse = true
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

// resolveRepoName returns the "owner/name" for the repo, either from the flag or by querying gh.
func resolveRepoName(repo, repoDir string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	out, err := runGH(repoDir, "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// fetchNeedsResponse uses the GraphQL API to check review threads for authored PRs.
// Returns a set of PR URLs that have unresolved threads where the last comment is not from username.
func fetchNeedsResponse(repo, repoDir, username string) (map[string]bool, error) {
	repoName, err := resolveRepoName(repo, repoDir)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf("repo:%s is:pr is:open author:%s", repoName, username)
	gqlQuery := `query($q: String!) {
		search(query: $q, type: ISSUE, first: 30) {
			nodes {
				... on PullRequest {
					url
					reviewThreads(first: 100) {
						nodes {
							isResolved
							comments(last: 1) {
								nodes {
									author { login }
								}
							}
						}
					}
				}
			}
		}
	}`

	out, err := runGH(repoDir, "api", "graphql", "-f", "query="+gqlQuery, "-f", "q="+query)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data struct {
			Search struct {
				Nodes []struct {
					URL           string `json:"url"`
					ReviewThreads struct {
						Nodes []reviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, err
	}

	needs := make(map[string]bool)
	for _, pr := range result.Data.Search.Nodes {
		if hasUnresolvedThreadFromOthers(pr.ReviewThreads.Nodes, username) {
			needs[pr.URL] = true
		}
	}
	return needs, nil
}

type reviewThread struct {
	IsResolved bool `json:"isResolved"`
	Comments   struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

// hasUnresolvedThreadFromOthers returns true if any unresolved review thread
// has a last comment from someone other than username.
func hasUnresolvedThreadFromOthers(threads []reviewThread, username string) bool {
	for _, thread := range threads {
		if thread.IsResolved {
			continue
		}
		comments := thread.Comments.Nodes
		if len(comments) > 0 && comments[0].Author.Login != username {
			return true
		}
	}
	return false
}

func allChecksPassed(checks []ghCheckRun) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if c.TypeName == "StatusContext" {
			if c.State != "SUCCESS" {
				return false
			}
		} else {
			if c.Status != "COMPLETED" {
				return false
			}
			if c.Conclusion != "SUCCESS" && c.Conclusion != "SKIPPED" && c.Conclusion != "NEUTRAL" {
				return false
			}
		}
	}
	return true
}

func buildState(repo, repoDir string) (*CritState, error) {
	username, err := resolveUsername()
	if err != nil || username == "" {
		return nil, fmt.Errorf("unable to resolve username: %w", err)
	}
	type reviewFetchResult struct {
		prs []ghPR
		err error
	}
	type authoredFetchResult struct {
		prs []ghPR
		err error
	}
	type needsResponseResult struct {
		m map[string]bool
	}

	reviewCh := make(chan reviewFetchResult, 1)
	authoredCh := make(chan authoredFetchResult, 1)
	needsCh := make(chan needsResponseResult, 1)

	go func() {
		prs, err := fetchReviewRequested(repo, repoDir)
		reviewCh <- reviewFetchResult{prs, err}
	}()
	go func() {
		prs, err := fetchAuthored(repo, repoDir)
		authoredCh <- authoredFetchResult{prs, err}
	}()
	go func() {
		m, _ := fetchNeedsResponse(repo, repoDir, username)
		if m == nil {
			m = make(map[string]bool)
		}
		needsCh <- needsResponseResult{m}
	}()

	reviewRes := <-reviewCh
	if reviewRes.err != nil {
		return nil, reviewRes.err
	}
	authoredRes := <-authoredCh
	if authoredRes.err != nil {
		return nil, authoredRes.err
	}
	needsResponse := (<-needsCh).m

	reviewPRs := reviewRes.prs
	authoredPRs := authoredRes.prs

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
		updated := parseTimePtr(pr.UpdatedAt)
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
	var authoredStates []AuthoredPullRequestState
	for _, pr := range authoredPRs {
		created := parseTimePtr(pr.CreatedAt)
		updated := parseTimePtr(pr.UpdatedAt)
		state := AuthoredPullRequestState{
			Title:         pr.Title,
			URL:           pr.URL,
			IsDraft:       pr.IsDraft,
			CreatedAt:     created,
			UpdatedAt:     updated,
			NeedsResponse: needsResponse[pr.URL],
			IsApproved:    pr.ReviewDecision == "APPROVED",
			ChecksPassed:  allChecksPassed(pr.StatusCheckRollup),
			HasConflict:   pr.Mergeable == "CONFLICTING",
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

// ----- Launchd service installation -----

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.user.crit</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Executable}}</string>
		{{- if .Repo}}
		<string>{{.Repo}}</string>
		{{- end}}
		{{- if .RepoDir}}
		<string>--repo-dir</string>
		<string>{{.RepoDir}}</string>
		{{- end}}
		<string>--state</string>
		<string>{{.StatePath}}</string>
		<string>--force-fetch</string>
		<string>--style</string>
		<string>log</string>
	</array>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
	<key>StartInterval</key>
	<integer>300</integer>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>`

type PlistData struct {
	Executable string
	Repo       string
	RepoDir    string
	StatePath  string
	LogPath    string
}

func getPlistPath() string {
	return filepath.Join(mustUserHomeDir(), "Library/LaunchAgents/com.user.crit.plist")
}

func getLogPath() string {
	return filepath.Join(mustUserHomeDir(), "Library/Logs/crit.log")
}

func installLaunchdService(flags Flags) error {
	plistPath := getPlistPath()
	
	// Check if service is already installed
	if _, err := os.Stat(plistPath); err == nil {
		// Check if it's also loaded
		cmd := exec.Command("launchctl", "list", "com.user.crit")
		if cmd.Run() == nil {
			fmt.Println("Service is already installed and loaded.")
			return nil
		} else {
			fmt.Println("Service is installed but not loaded. Loading...")
			loadCmd := exec.Command("launchctl", "load", plistPath)
			if err := loadCmd.Run(); err != nil {
				return fmt.Errorf("failed to load existing service: %w", err)
			}
			fmt.Println("Successfully loaded existing service.")
			return nil
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	launchAgentsDir := filepath.Dir(plistPath)

	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	tmpl, err := template.New("plist").Parse(launchdPlistTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse plist template: %w", err)
	}

	data := PlistData{
		Executable: exe,
		Repo:       flags.repo,
		RepoDir:    flags.repoDir,
		StatePath:  flags.statePath,
		LogPath:    getLogPath(),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute plist template: %w", err)
	}

	if err := os.WriteFile(plistPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	cmd := exec.Command("launchctl", "load", plistPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to load launchd service: %w", err)
	}

	fmt.Printf("Successfully installed and loaded launchd service at %s\n", plistPath)
	fmt.Println("The service will run every 5 minutes to update crit state.")
	return nil
}

func removeLaunchdService() error {
	plistPath := getPlistPath()
	
	// Try to unload the service first
	cmd := exec.Command("launchctl", "unload", plistPath)
	_ = cmd.Run() // Ignore error if service wasn't loaded
	
	// Remove the plist file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}
	
	fmt.Println("Successfully removed launchd service.")
	return nil
}

func statusLaunchdService() error {
	plistPath := getPlistPath()
	
	// Check if plist file exists
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Println("Service is not installed.")
		return nil
	}
	
	// Check if service is loaded
	cmd := exec.Command("launchctl", "list", "com.user.crit")
	output, err := cmd.Output()
	if err != nil {
		fmt.Println("Service is installed but not loaded.")
		return nil
	}
	
	fmt.Printf("Service is installed and loaded.\nPlist path: %s\n", plistPath)
	
	// Parse launchctl list output to show basic status
	lines := strings.Split(string(output), "\n")
	if len(lines) > 0 && lines[0] != "" {
		parts := strings.Fields(lines[0])
		if len(parts) >= 3 {
			fmt.Printf("Status: PID=%s, Exit Code=%s, Label=%s\n", parts[0], parts[1], parts[2])
		}
	}
	
	return nil
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
	fetchStart := time.Now()
	st, err := buildState(flags.repo, flags.repoDir)
	if err == nil {
		st.FetchDuration = time.Since(fetchStart)
		_ = writeState(flags.statePath, st)
		render(st, flags.style)
	}
	return 0
}

func handleServiceCommand(subcommand string) int {
	switch subcommand {
	case "install":
		// Parse flags for install subcommand
		installCmd := flag.NewFlagSet("install", flag.ExitOnError)
		var flags Flags
		installCmd.StringVar(&flags.repoDir, "repo-dir", "", "Path to local git repo (sets gh working directory)")
		installCmd.StringVar(&flags.statePath, "state", defaultStatePath(), "Path to state file")
		
		// Manually parse to handle repo argument before flags
		args := os.Args[3:]
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			flags.repo = args[0]
			args = args[1:]
		}
		
		installCmd.Parse(args)
		
		flags.statePath = expandUser(flags.statePath)
		if flags.repoDir != "" {
			flags.repoDir = expandUser(flags.repoDir)
		}
		
		if err := installLaunchdService(flags); err != nil {
			fmt.Fprintf(os.Stderr, "Error installing service: %v\n", err)
			return 1
		}
		return 0
		
	case "remove":
		if err := removeLaunchdService(); err != nil {
			fmt.Fprintf(os.Stderr, "Error removing service: %v\n", err)
			return 1
		}
		return 0
		
	case "status":
		if err := statusLaunchdService(); err != nil {
			fmt.Fprintf(os.Stderr, "Error checking service status: %v\n", err)
			return 1
		}
		return 0
		
	case "log":
		if err := showServiceLog(); err != nil {
			fmt.Fprintf(os.Stderr, "Error showing service log: %v\n", err)
			return 1
		}
		return 0

	default:
		fmt.Fprintf(os.Stderr, "Unknown service subcommand: %s\n", subcommand)
		fmt.Fprintf(os.Stderr, "Usage: crit service <install|remove|status|log>\n")
		return 1
	}
}

func showServiceLog() error {
	logPath := getLogPath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Println("No log file found. Is the service installed?")
		return nil
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}

	cmd := exec.Command(pager, "+G", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func parseFlags() Flags {
	var f Flags
	flag.StringVar(&f.repoDir, "repo-dir", "", "Path to local git repo (sets gh working directory)")
	flag.StringVar(&f.statePath, "state", defaultStatePath(), "Path to state file")
	flag.BoolVar(&f.forceFetch, "force-fetch", false, "Force fetching even if cached state is fresh")
	flag.BoolVar(&f.renderOnly, "render-only", false, "Only render cached state; do not fetch")
	flag.StringVar(&f.style, "style", "full", "Output style: full|prompt|log|none")
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

func printUsage() {
	cmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `Usage: %[1]s [command] [options]

Commands:
  (default)    Fetch and display PR review status
  service      Manage the background launchd service
  help         Show this help message

Options (for default command):
  [repo]              GitHub repo (e.g. owner/repo) as positional argument
  --repo-dir <path>   Path to local git repo (sets gh working directory)
  --state <path>      Path to state file (default: ~/.crit_state.json)
  --force-fetch       Force fetching even if cached state is fresh
  --render-only       Only render cached state; do not fetch
  --style <style>     Output style: full|prompt|log|none (default: full)
  --quick             Render stale cache immediately and refresh in background

Service subcommands:
  %[1]s service install [repo] [--repo-dir <path>] [--state <path>]
  %[1]s service remove
  %[1]s service status
  %[1]s service log
`, cmd)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "service":
			if len(os.Args) < 3 {
				fmt.Fprintf(os.Stderr, "Usage: gcrit service <install|remove|status|log>\n")
				os.Exit(1)
			}
			os.Exit(handleServiceCommand(os.Args[2]))
		case "help", "--help", "-h":
			printUsage()
			os.Exit(0)
		default:
			// If the first arg doesn't start with "-", it could be either
			// a repo name or an unknown command. Repo names contain "/".
			arg := os.Args[1]
			if !strings.HasPrefix(arg, "-") && !strings.Contains(arg, "/") {
				fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", arg)
				printUsage()
				os.Exit(1)
			}
		}
	}

	f := parseFlags()
	os.Exit(execute(f))
}
