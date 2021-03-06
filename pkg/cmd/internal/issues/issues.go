// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package issues

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	version "github.com/hashicorp/go-version"
	"github.com/pkg/errors"
)

const (
	githubAPITokenEnv    = "GITHUB_API_TOKEN"
	teamcityVCSNumberEnv = "BUILD_VCS_NUMBER"
	teamcityBuildIDEnv   = "TC_BUILD_ID"
	teamcityServerURLEnv = "TC_SERVER_URL"
	tagsEnv              = "TAGS"
	goFlagsEnv           = "GOFLAGS"
	githubUser           = "cockroachdb"
	githubRepo           = "cockroach"
	cockroachPkgPrefix   = "github.com/cockroachdb/cockroach/pkg/"
)

var (
	issueLabels  = []string{"O-robot", "C-test-failure"}
	stacktraceRE = regexp.MustCompile(`(?m:^goroutine\s\d+)`)
)

// Based on the following observed API response:
//
// 422 Validation Failed [{Resource:Issue Field:body Code:custom Message:body
// is too long (maximum is 65536 characters)}]
const githubIssueBodyMaximumLength = 1<<16 - 1

// trimIssueRequestBody trims message such that the total size of an issue body
// is less than githubIssueBodyMaximumLength. usedCharacters specifies the
// number of characters that have already been used for the issue body (see
// newIssueRequest below). message is usually the test failure message and
// possibly includes stacktraces for all of the goroutines (which is what makes
// the message very large).
//
// TODO(peter): Rather than trimming the message like this, perhaps it can be
// added as an attachment or some other expandable comment.
func trimIssueRequestBody(message string, usedCharacters int) string {
	maxLength := githubIssueBodyMaximumLength - usedCharacters

	if m := stacktraceRE.FindStringIndex(message); m != nil {
		// We want the top stack traces plus a few lines before.
		{
			startIdx := m[0]
			for i := 0; i < 100; i++ {
				if idx := strings.LastIndexByte(message[:startIdx], '\n'); idx != -1 {
					startIdx = idx
				}
			}
			message = message[startIdx:]
		}
		for len(message) > maxLength {
			if idx := strings.LastIndexByte(message, '\n'); idx != -1 {
				message = message[:idx]
			} else {
				message = message[:maxLength]
			}
		}
	} else {
		// We want the FAIL line.
		for len(message) > maxLength {
			if idx := strings.IndexByte(message, '\n'); idx != -1 {
				message = message[idx+1:]
			} else {
				message = message[len(message)-maxLength:]
			}
		}
	}

	return message
}

// If the assignee would be the key in this map, assign to the value instead.
// Helpful to avoid pinging former employees.
var oldFriendsMap = map[string]string{
	"tamird": "tschottdorf",
}

func getAssignee(
	ctx context.Context,
	authorEmail string,
	listCommits func(ctx context.Context, owner string, repo string,
		opts *github.CommitsListOptions) ([]*github.RepositoryCommit, *github.Response, error),
) (string, error) {
	commits, _, err := listCommits(ctx, githubUser, githubRepo, &github.CommitsListOptions{
		Author: authorEmail,
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", errors.Errorf("couldn't find GitHub commits for user email %s", authorEmail)
	}

	assignee := *commits[0].Author.Login

	if newAssignee, ok := oldFriendsMap[assignee]; ok {
		assignee = newAssignee
	}
	return assignee, nil
}

func getLatestTag() (string, error) {
	cmd := exec.Command("git", "describe", "--abbrev=0", "--tags")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func getProbableMilestone(
	ctx context.Context,
	getLatestTag func() (string, error),
	listMilestones func(ctx context.Context, owner string, repo string,
		opt *github.MilestoneListOptions) ([]*github.Milestone, *github.Response, error),
) *int {
	tag, err := getLatestTag()
	if err != nil {
		log.Printf("unable to get latest tag: %s", err)
		log.Printf("issues will be posted without milestone")
		return nil
	}

	v, err := version.NewVersion(tag)
	if err != nil {
		log.Printf("unable to parse version from tag: %s", err)
		log.Printf("issues will be posted without milestone")
		return nil
	}
	if len(v.Segments()) < 2 {
		log.Printf("version %s has less than two components; issues will be posted without milestone", tag)
		return nil
	}
	vstring := fmt.Sprintf("%d.%d", v.Segments()[0], v.Segments()[1])

	milestones, _, err := listMilestones(ctx, githubUser, githubRepo, &github.MilestoneListOptions{
		State: "open",
	})
	if err != nil {
		log.Printf("unable to list milestones: %s", err)
		log.Printf("issues will be posted without milestone")
		return nil
	}

	for _, m := range milestones {
		if m.GetTitle() == vstring {
			return m.Number
		}
	}
	return nil
}

type poster struct {
	sha       string
	buildID   string
	serverURL string
	milestone *int

	createIssue func(ctx context.Context, owner string, repo string,
		issue *github.IssueRequest) (*github.Issue, *github.Response, error)
	searchIssues func(ctx context.Context, query string,
		opt *github.SearchOptions) (*github.IssuesSearchResult, *github.Response, error)
	createComment func(ctx context.Context, owner string, repo string, number int,
		comment *github.IssueComment) (*github.IssueComment, *github.Response, error)
	listCommits func(ctx context.Context, owner string, repo string,
		opts *github.CommitsListOptions) ([]*github.RepositoryCommit, *github.Response, error)
	listMilestones func(ctx context.Context, owner string, repo string,
		opt *github.MilestoneListOptions) ([]*github.Milestone, *github.Response, error)
	getLatestTag func() (string, error)
}

func newPoster() *poster {
	token, ok := os.LookupEnv(githubAPITokenEnv)
	if !ok {
		log.Fatalf("GitHub API token environment variable %s is not set", githubAPITokenEnv)
	}

	ctx := context.Background()
	client := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)))

	return &poster{
		createIssue:    client.Issues.Create,
		searchIssues:   client.Search.Issues,
		createComment:  client.Issues.CreateComment,
		listCommits:    client.Repositories.ListCommits,
		listMilestones: client.Issues.ListMilestones,
		getLatestTag:   getLatestTag,
	}
}

func (p *poster) init() {
	var ok bool
	if p.sha, ok = os.LookupEnv(teamcityVCSNumberEnv); !ok {
		log.Fatalf("VCS number environment variable %s is not set", teamcityVCSNumberEnv)
	}
	if p.buildID, ok = os.LookupEnv(teamcityBuildIDEnv); !ok {
		log.Fatalf("teamcity build ID environment variable %s is not set", teamcityBuildIDEnv)
	}
	if p.serverURL, ok = os.LookupEnv(teamcityServerURLEnv); !ok {
		log.Fatalf("teamcity server URL environment variable %s is not set", teamcityServerURLEnv)
	}
	p.milestone = getProbableMilestone(context.Background(), p.getLatestTag, p.listMilestones)
}

func (p *poster) post(
	ctx context.Context, detail, packageName, testName, message, authorEmail string,
) error {
	packageName = cockroachPkgPrefix + packageName

	const bodyTemplate = `SHA: https://github.com/cockroachdb/cockroach/commits/%[1]s

Parameters:%[2]s

Failed test: %[3]s`
	const messageTemplate = "\n\n```\n%s\n```"

	newIssueRequest := func(packageName, testName, message, assignee string) *github.IssueRequest {
		title := fmt.Sprintf("%s: %s failed%s",
			strings.TrimPrefix(packageName, cockroachPkgPrefix), testName, detail)
		body := fmt.Sprintf(bodyTemplate, p.sha, p.parameters(), p.teamcityURL()) + messageTemplate
		// We insert a raw "%s" above so we can figure out the length of the
		// body so far, without the actual error text. We need this length so we
		// can calculate the maximum amount of error text we can include in the
		// issue without exceeding GitHub's limit. We replace that %s in the
		// following Sprintf.
		body = fmt.Sprintf(body, trimIssueRequestBody(message, len(body)))

		return &github.IssueRequest{
			Title:     &title,
			Body:      &body,
			Labels:    &issueLabels,
			Assignee:  &assignee,
			Milestone: p.milestone,
		}
	}

	newIssueComment := func(packageName, testName string) *github.IssueComment {
		body := fmt.Sprintf(bodyTemplate, p.sha, p.parameters(), p.teamcityURL())
		return &github.IssueComment{Body: &body}
	}

	assignee, err := getAssignee(ctx, authorEmail, p.listCommits)
	if err != nil {
		// if we *can't* assign anyone, sigh, feel free to hard-code me.
		// -- tschottdorf, 11/3/2017
		assignee = "tschottdorf"
		message += fmt.Sprintf("\n\nFailed to find issue assignee: \n%s", err)
	}

	issueRequest := newIssueRequest(packageName, testName, message, assignee)
	searchQuery := fmt.Sprintf(`"%s" user:%s repo:%s is:open`,
		*issueRequest.Title, githubUser, githubRepo)
	for _, label := range issueLabels {
		searchQuery = searchQuery + fmt.Sprintf(` label:"%s"`, label)
	}

	var foundIssue *int
	result, _, err := p.searchIssues(ctx, searchQuery, &github.SearchOptions{
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		return errors.Wrapf(err, "failed to search GitHub with query %s",
			github.Stringify(searchQuery))
	}
	if *result.Total > 0 {
		foundIssue = result.Issues[0].Number
	}

	if foundIssue == nil {
		if _, _, err := p.createIssue(ctx, githubUser, githubRepo, issueRequest); err != nil {
			return errors.Wrapf(err, "failed to create GitHub issue %s",
				github.Stringify(issueRequest))
		}
	} else {
		comment := newIssueComment(packageName, testName)
		if _, _, err := p.createComment(
			ctx, githubUser, githubRepo, *foundIssue, comment); err != nil {
			return errors.Wrapf(err, "failed to update issue #%d with %s",
				*foundIssue, github.Stringify(comment))
		}
	}

	return nil
}

func (p *poster) teamcityURL() *url.URL {
	options := url.Values{}
	options.Add("buildId", p.buildID)
	options.Add("tab", "buildLog")

	u, err := url.Parse(p.serverURL)
	if err != nil {
		log.Fatal(err)
	}
	u.Scheme = "https"
	u.Path = "viewLog.html"
	u.RawQuery = options.Encode()
	return u
}

func (p *poster) parameters() string {
	var parameters []string
	for _, parameter := range []string{
		tagsEnv,
		goFlagsEnv,
	} {
		if val, ok := os.LookupEnv(parameter); ok {
			parameters = append(parameters, parameter+"="+val)
		}
	}
	if len(parameters) == 0 {
		return ""
	}
	return "\n```\n" + strings.Join(parameters, "\n") + "\n```"
}

var defaultP struct {
	sync.Once
	*poster
}

// Post either creates a new issue for a failed test, or posts a comment to an
// existing open issue.
func Post(ctx context.Context, detail, packageName, testName, message, authorEmail string) error {
	defaultP.Do(func() {
		defaultP.poster = newPoster()
		defaultP.init()
	})
	return defaultP.post(ctx, detail, packageName, testName, message, authorEmail)
}

// CanPost returns true if the github API token environment variable is set.
func CanPost() bool {
	_, ok := os.LookupEnv(githubAPITokenEnv)
	return ok
}
