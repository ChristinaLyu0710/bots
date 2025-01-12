// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package labeler

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/go-github/v26/github"

	"istio.io/bots/policybot/handlers/githubwebhook/filters"
	"istio.io/bots/policybot/pkg/config"
	"istio.io/bots/policybot/pkg/gh"
	"istio.io/bots/policybot/pkg/storage"
	"istio.io/bots/policybot/pkg/storage/cache"
	"istio.io/pkg/log"
)

// Generates nagging messages in PRs based on regex matches on the title, body, and affected files
type Labeler struct {
	cache             *cache.Cache
	gc                *gh.ThrottledClient
	orgs              []config.Org
	autoLabels        []config.AutoLabel
	singleLineRegexes map[string]*regexp.Regexp
	multiLineRegexes  map[string]*regexp.Regexp
	repos             map[string][]config.AutoLabel // index is org/repo, value is org-level auto-labels
}

var scope = log.RegisterScope("labeler", "Issue and PR auto-labeler", 0)

func NewLabeler(gc *gh.ThrottledClient, cache *cache.Cache, orgs []config.Org, autoLabels []config.AutoLabel) (filters.Filter, error) {
	l := &Labeler{
		cache:             cache,
		gc:                gc,
		orgs:              orgs,
		autoLabels:        autoLabels,
		singleLineRegexes: make(map[string]*regexp.Regexp),
		multiLineRegexes:  make(map[string]*regexp.Regexp),
		repos:             make(map[string][]config.AutoLabel),
	}

	for _, al := range autoLabels {
		if err := l.processAutoLabelRegexes(al); err != nil {
			return nil, err
		}
	}

	for _, org := range orgs {
		for _, al := range org.AutoLabels {
			if err := l.processAutoLabelRegexes(al); err != nil {
				return nil, err
			}
		}
	}

	for _, org := range orgs {
		for _, repo := range org.Repos {
			l.repos[org.Name+"/"+repo.Name] = org.AutoLabels
		}
	}

	return l, nil
}

// Precompile all the regexes
func (l *Labeler) processAutoLabelRegexes(al config.AutoLabel) error {
	for _, expr := range al.MatchTitle {
		r, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return fmt.Errorf("invalid regular expression %s: %v", expr, err)
		}
		l.singleLineRegexes[expr] = r
	}

	for _, expr := range al.MatchBody {
		r, err := regexp.Compile("(?mi)" + expr)
		if err != nil {
			return fmt.Errorf("invalid regular expression %s: %v", expr, err)
		}
		l.multiLineRegexes[expr] = r
	}

	for _, expr := range al.AbsentLabels {
		r, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return fmt.Errorf("invalid regular expression %s: %v", expr, err)
		}
		l.singleLineRegexes[expr] = r
	}

	return nil
}

// process an event arriving from GitHub
func (l *Labeler) Handle(context context.Context, event interface{}) {
	action := ""
	repo := ""
	number := 0
	var issue *storage.Issue
	var pr *storage.PullRequest

	ip, ok := event.(*github.IssueEvent)
	if ok {
		action = ip.GetEvent()
		repo = ip.GetIssue().GetRepository().GetFullName()
		number = ip.GetIssue().GetNumber()
		issue, _ = gh.ConvertIssue(
			ip.GetIssue().GetRepository().GetOwner().GetLogin(),
			ip.GetIssue().GetRepository().GetName(),
			ip.GetIssue())
	}

	prp, ok := event.(*github.PullRequestEvent)
	if ok {
		action = prp.GetAction()
		repo = prp.GetRepo().GetFullName()
		number = prp.GetPullRequest().GetNumber()
		pr, _ = gh.ConvertPullRequest(
			prp.GetRepo().GetOwner().GetLogin(),
			prp.GetRepo().GetName(),
			prp.GetPullRequest(),
			nil)
	}

	if action != "opened" && action != "review_requested" {
		// not what we care about
		return
	}

	// see if the event is in a repo we're monitoring
	autoLabels, ok := l.repos[repo]
	if !ok {
		scope.Infof("Ignoring event %d from repo %s since it's not in a monitored repo", number, repo)
		return
	}

	scope.Infof("Processing event %d from repo %s", number, repo)

	if issue != nil {
		l.processIssue(context, issue, autoLabels)
	} else {
		l.processPullRequest(context, pr, autoLabels)
	}
}

func (l *Labeler) processIssue(context context.Context, issue *storage.Issue, orgALs []config.AutoLabel) {
	// get all the issue's labels
	var labels []*storage.Label
	for _, labelName := range issue.Labels {
		label, err := l.cache.ReadLabel(context, issue.OrgLogin, issue.RepoName, labelName)
		if err != nil {
			scope.Errorf("Unable to get labels for issue %d in repo %s/%s: %v", issue.IssueNumber, issue.OrgLogin, issue.RepoName, err)
			return
		} else if label != nil {
			labels = append(labels, label)
		}
	}

	// find any matching global auto labels
	var toApply []string
	for _, al := range l.autoLabels {
		if l.matchAutoLabel(al, issue.Title, issue.Body, labels) {
			toApply = append(toApply, al.Labels...)
		}
	}

	// find any matching org-level auto labels
	for _, al := range orgALs {
		if l.matchAutoLabel(al, issue.Title, issue.Body, labels) {
			toApply = append(toApply, al.Labels...)
		}
	}

	if len(toApply) > 0 {
		if _, _, err := l.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
			return client.Issues.AddLabelsToIssue(context, issue.OrgLogin, issue.RepoName, int(issue.IssueNumber), toApply)
		}); err != nil {
			scope.Errorf("Unable to set labels on issue %d in repo %s/%s: %v", issue.IssueNumber, issue.OrgLogin, issue.RepoName, err)
			return
		}
	}

	scope.Infof("Applied %d label(s) to issue %d from repo %s/%s", len(toApply), issue.IssueNumber, issue.OrgLogin, issue.RepoName)
}

func (l *Labeler) processPullRequest(context context.Context, pr *storage.PullRequest, orgALs []config.AutoLabel) {
	// get all the pr's labels
	var labels []*storage.Label
	for _, labelName := range pr.Labels {
		label, err := l.cache.ReadLabel(context, pr.OrgLogin, pr.RepoName, labelName)
		if err != nil {
			scope.Errorf("Unable to get labels for pr %d in repo %s/%s: %v", pr.PullRequestNumber, pr.OrgLogin, pr.RepoName, err)
			return
		} else if label != nil {
			labels = append(labels, label)
		}
	}

	// find any matching global auto labels
	var toApply []string
	for _, al := range l.autoLabels {
		if l.matchAutoLabel(al, pr.Title, pr.Body, labels) {
			toApply = append(toApply, al.Labels...)
		}
	}

	// find any matching org-level auto labels
	for _, al := range orgALs {
		if l.matchAutoLabel(al, pr.Title, pr.Body, labels) {
			toApply = append(toApply, al.Labels...)
		}
	}

	if len(toApply) > 0 {
		if _, _, err := l.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
			return client.Issues.AddLabelsToIssue(context, pr.OrgLogin, pr.RepoName, int(pr.PullRequestNumber), toApply)
		}); err != nil {
			scope.Errorf("Unable to set labels on event %d in repo %s/%s: %v", pr.PullRequestNumber, pr.OrgLogin, pr.RepoName, err)
			return
		}
	}

	scope.Infof("Applied %d label(s) to pr %d from repo %s/%s", len(toApply), pr.PullRequestNumber, pr.OrgLogin, pr.RepoName)
}

func (l *Labeler) matchAutoLabel(al config.AutoLabel, title string, body string, labels []*storage.Label) bool {
	// if the title and body don't match, we're done
	if !l.titleMatch(al, title) && !l.bodyMatch(al, body) {
		return false
	}

	// if any labels match, we're done
	for _, label := range labels {
		if l.labelMatch(al, label.LabelName) {
			return false
		}
	}

	return true
}

func (l *Labeler) titleMatch(al config.AutoLabel, title string) bool {
	for _, expr := range al.MatchTitle {
		r := l.singleLineRegexes[expr]
		if r.MatchString(title) {
			return true
		}
	}

	return false
}

func (l *Labeler) bodyMatch(al config.AutoLabel, body string) bool {
	for _, expr := range al.MatchBody {
		r := l.multiLineRegexes[expr]
		if r.MatchString(body) {
			return true
		}
	}

	return false
}

func (l *Labeler) labelMatch(al config.AutoLabel, label string) bool {
	for _, expr := range al.AbsentLabels {
		r := l.singleLineRegexes[expr]
		if r.MatchString(label) {
			return true
		}
	}

	return false
}
