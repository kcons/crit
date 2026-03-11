//go:build darwin

package main

import (
	"testing"
	"time"
)

func TestFormatPromptString(t *testing.T) {
	now := time.Now()
	
	tests := []struct {
		name     string
		state    *CritState
		expected string
	}{
		{
			name: "no reviews or authored PRs",
			state: &CritState{
				GeneratedAt:          now,
				Username:             "testuser",
				PullRequests:         []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "👍",
		},
		{
			name: "only reviews",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{Title: "Test PR 1", URL: "https://github.com/test/repo/pull/1", IsDraft: false},
					{Title: "Test PR 2", URL: "https://github.com/test/repo/pull/2", IsDraft: false},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "🔍2",
		},
		{
			name: "only authored PRs",
			state: &CritState{
				GeneratedAt:  now,
				Username:     "testuser",
				PullRequests: []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{Title: "My PR 1", URL: "https://github.com/test/repo/pull/3", IsDraft: false},
				},
			},
			expected: "🚢1",
		},
		{
			name: "both reviews and authored PRs",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{Title: "Test PR 1", URL: "https://github.com/test/repo/pull/1", IsDraft: false},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{Title: "My PR 1", URL: "https://github.com/test/repo/pull/3", IsDraft: false},
					{Title: "My PR 2", URL: "https://github.com/test/repo/pull/4", IsDraft: false},
				},
			},
			expected: "🔍1 🚢2",
		},
		{
			name: "draft PRs are excluded from review count",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{Title: "Draft PR", URL: "https://github.com/test/repo/pull/1", IsDraft: true},
					{Title: "Real PR", URL: "https://github.com/test/repo/pull/2", IsDraft: false},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "🔍1",
		},
		{
			name: "authored PRs include drafts in count",
			state: &CritState{
				GeneratedAt:  now,
				Username:     "testuser",
				PullRequests: []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{Title: "My Draft", URL: "https://github.com/test/repo/pull/3", IsDraft: true},
					{Title: "My PR", URL: "https://github.com/test/repo/pull/4", IsDraft: false},
				},
			},
			expected: "🚢2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPromptString(tt.state, true)
			if result != tt.expected {
				t.Errorf("formatPromptString() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFormatPromptStringWithColors(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-25 * time.Hour) // Overdue
	
	tests := []struct {
		name     string
		state    *CritState
		expected string
	}{
		{
			name: "overdue review",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{
						Title:     "Old PR",
						URL:       "https://github.com/test/repo/pull/1",
						IsDraft:   false,
						UpdatedAt: &oldTime,
						IsOverdue: true,
					},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "🔍\x1b[31m1\x1b[0m", // Red color codes
		},
		{
			name: "overdue authored PR",
			state: &CritState{
				GeneratedAt:  now,
				Username:     "testuser",
				PullRequests: []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{
						Title:        "Old authored PR",
						URL:          "https://github.com/test/repo/pull/3",
						IsDraft:      false,
						CreatedAt:    &oldTime,
						IsOverdueAge: true,
					},
				},
			},
			expected: "🚢\x1b[31m1\x1b[0m", // Red color codes
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPromptString(tt.state, true)
			if result != tt.expected {
				t.Errorf("formatPromptString() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFormatPromptStringWithoutColors(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-25 * time.Hour) // Overdue
	warningTime := now.Add(-4 * 24 * time.Hour) // Warning age
	
	tests := []struct {
		name     string
		state    *CritState
		expected string
	}{
		{
			name: "no reviews or authored PRs",
			state: &CritState{
				GeneratedAt:          now,
				Username:             "testuser",
				PullRequests:         []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "👍",
		},
		{
			name: "overdue review without colors",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{
						Title:     "Old PR",
						URL:       "https://github.com/test/repo/pull/1",
						IsDraft:   false,
						UpdatedAt: &oldTime,
						IsOverdue: true,
					},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{},
			},
			expected: "🔍1", // No color codes
		},
		{
			name: "overdue authored PR without colors",
			state: &CritState{
				GeneratedAt:  now,
				Username:     "testuser",
				PullRequests: []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{
						Title:        "Old authored PR",
						URL:          "https://github.com/test/repo/pull/3",
						IsDraft:      false,
						CreatedAt:    &oldTime,
						IsOverdueAge: true,
					},
				},
			},
			expected: "🚢1", // No color codes
		},
		{
			name: "warning age authored PR without colors",
			state: &CritState{
				GeneratedAt:  now,
				Username:     "testuser",
				PullRequests: []PullRequestState{},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{
						Title:        "Warning age PR",
						URL:          "https://github.com/test/repo/pull/4",
						IsDraft:      false,
						CreatedAt:    &warningTime,
						IsWarningAge: true,
					},
				},
			},
			expected: "🚢1", // No color codes
		},
		{
			name: "both overdue review and authored PR without colors",
			state: &CritState{
				GeneratedAt: now,
				Username:    "testuser",
				PullRequests: []PullRequestState{
					{
						Title:     "Old review PR",
						URL:       "https://github.com/test/repo/pull/1",
						IsDraft:   false,
						UpdatedAt: &oldTime,
						IsOverdue: true,
					},
				},
				AuthoredPullRequests: []AuthoredPullRequestState{
					{
						Title:        "Old authored PR 1",
						URL:          "https://github.com/test/repo/pull/3",
						IsDraft:      false,
						CreatedAt:    &oldTime,
						IsOverdueAge: true,
					},
					{
						Title:        "Warning authored PR 2",
						URL:          "https://github.com/test/repo/pull/4",
						IsDraft:      false,
						CreatedAt:    &warningTime,
						IsWarningAge: true,
					},
				},
			},
			expected: "🔍1 🚢2", // No color codes
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPromptString(tt.state, false)
			if result != tt.expected {
				t.Errorf("formatPromptString() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func makeThread(resolved bool, lastAuthor string) reviewThread {
	var t reviewThread
	t.IsResolved = resolved
	t.Comments.Nodes = []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	}{
		{Author: struct {
			Login string `json:"login"`
		}{Login: lastAuthor}},
	}
	return t
}

func TestHasUnresolvedThreadFromOthers(t *testing.T) {
	tests := []struct {
		name     string
		threads  []reviewThread
		username string
		want     bool
	}{
		{
			name:     "no threads",
			threads:  nil,
			username: "me",
			want:     false,
		},
		{
			name:     "resolved thread from other",
			threads:  []reviewThread{makeThread(true, "reviewer")},
			username: "me",
			want:     false,
		},
		{
			name:     "unresolved thread last comment by self",
			threads:  []reviewThread{makeThread(false, "me")},
			username: "me",
			want:     false,
		},
		{
			name:     "unresolved thread last comment by other",
			threads:  []reviewThread{makeThread(false, "reviewer")},
			username: "me",
			want:     true,
		},
		{
			name: "mix of resolved and unresolved, only resolved from others",
			threads: []reviewThread{
				makeThread(true, "reviewer"),
				makeThread(false, "me"),
			},
			username: "me",
			want:     false,
		},
		{
			name: "mix of resolved and unresolved, unresolved from other",
			threads: []reviewThread{
				makeThread(true, "me"),
				makeThread(false, "reviewer"),
			},
			username: "me",
			want:     true,
		},
		{
			name: "multiple unresolved, one from other",
			threads: []reviewThread{
				makeThread(false, "me"),
				makeThread(false, "me"),
				makeThread(false, "reviewer"),
			},
			username: "me",
			want:     true,
		},
		{
			name: "multiple reviewers, all responded to",
			threads: []reviewThread{
				makeThread(false, "me"),
				makeThread(false, "me"),
			},
			username: "me",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasUnresolvedThreadFromOthers(tt.threads, tt.username)
			if got != tt.want {
				t.Errorf("hasUnresolvedThreadFromOthers() = %v, want %v", got, tt.want)
			}
		})
	}
}