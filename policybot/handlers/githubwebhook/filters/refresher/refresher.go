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

package refresher

import (
	"context"
	"time"

	"github.com/google/go-github/v26/github"

	"istio.io/bots/policybot/handlers/githubwebhook/filters"
	"istio.io/bots/policybot/pkg/config"
	"istio.io/bots/policybot/pkg/gh"
	"istio.io/bots/policybot/pkg/storage"
	"istio.io/bots/policybot/pkg/storage/cache"
	"istio.io/pkg/log"
)

// Updates the DB based on incoming GitHub webhook events.
type Refresher struct {
	repos map[string]bool
	cache *cache.Cache
	store storage.Store
	gc    *gh.ThrottledClient
}

var scope = log.RegisterScope("refresher", "Dynamic database refresher", 0)

func NewRefresher(cache *cache.Cache, store storage.Store, gc *gh.ThrottledClient, orgs []config.Org) filters.Filter {
	r := &Refresher{
		repos: make(map[string]bool),
		cache: cache,
		store: store,
		gc:    gc,
	}

	for _, org := range orgs {
		for _, repo := range org.Repos {
			r.repos[org.Name+"/"+repo.Name] = true
		}
	}

	return r
}

// accept an event arriving from GitHub
func (r *Refresher) Handle(context context.Context, event interface{}) {
	switch p := event.(type) {
	case *github.IssueEvent:
		scope.Infof("Received IssueEvent: %s, %d, %s", p.GetIssue().GetRepository().GetFullName(), p.GetIssue().GetNumber(), p.GetEvent())

		if !r.repos[p.GetIssue().GetRepository().GetFullName()] {
			scope.Infof("Ignoring issue %d from repo %s since it's not in a monitored repo", p.GetIssue().GetNumber(), p.GetIssue().GetRepository().GetFullName())
			return
		}

		issue, discoveredUsers := gh.ConvertIssue(
			p.GetIssue().GetRepository().GetOwner().GetLogin(),
			p.GetIssue().GetRepository().GetName(),
			p.GetIssue())
		issues := []*storage.Issue{issue}
		if err := r.cache.WriteIssues(context, issues); err != nil {
			scope.Errorf(err.Error())
			return
		}

		event := &storage.IssueEvent{
			OrgLogin:    issue.OrgLogin,
			RepoName:    issue.RepoName,
			IssueNumber: issue.IssueNumber,
			CreatedAt:   p.GetCreatedAt(),
			Actor:       p.GetActor().GetLogin(),
			Action:      p.GetEvent(),
		}

		events := []*storage.IssueEvent{event}
		if err := r.store.WriteIssueEvents(context, events); err != nil {
			scope.Error(err.Error())
			return
		}

		r.syncUsers(context, discoveredUsers)

	case *github.IssueCommentEvent:
		scope.Infof("Received IssueCommentEvent: %s, %d, %s", p.GetRepo().GetFullName(), p.GetIssue().GetNumber(), p.GetAction())

		if !r.repos[p.GetRepo().GetFullName()] {
			scope.Infof("Ignoring issue comment for issue %d from repo %s since it's not in a monitored repo", p.GetIssue().GetNumber(), p.GetRepo().GetFullName())
			return
		}

		issueComment, discoveredUsers := gh.ConvertIssueComment(
			p.GetRepo().GetOwner().GetLogin(),
			p.GetRepo().GetName(),
			p.GetIssue().GetNumber(),
			p.GetComment())
		issueComments := []*storage.IssueComment{issueComment}
		if err := r.cache.WriteIssueComments(context, issueComments); err == nil {
			event := &storage.IssueCommentEvent{
				OrgLogin:       issueComment.OrgLogin,
				RepoName:       issueComment.RepoName,
				IssueNumber:    issueComment.IssueNumber,
				IssueCommentID: p.GetComment().GetID(),
				CreatedAt:      time.Now(),
				Actor:          p.GetSender().GetLogin(),
				Action:         p.GetAction(),
			}

			events := []*storage.IssueCommentEvent{event}
			if err := r.store.WriteIssueCommentEvents(context, events); err != nil {
				scope.Error(err.Error())
				return
			}
		}

		r.syncUsers(context, discoveredUsers)

	case *github.PullRequestEvent:
		scope.Infof("Received PullRequestEvent: %s, %d, %s", p.GetRepo().GetFullName(), p.GetNumber(), p.GetAction())

		if !r.repos[p.GetRepo().GetFullName()] {
			scope.Infof("Ignoring PR %d from repo %s since it's not in a monitored repo", p.PullRequest.Number, p.GetRepo().GetFullName())
			return
		}

		opt := &github.ListOptions{
			PerPage: 100,
		}

		// get the set of files comprising this PR since the payload didn't supply them
		var allFiles []string
		for {
			files, resp, err := r.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
				return client.PullRequests.ListFiles(context, p.GetRepo().GetOwner().GetLogin(), p.GetRepo().GetName(), p.GetNumber(), opt)
			})

			if err != nil {
				scope.Errorf("Unable to list all files for pull request %d in repo %s: %v\n", p.Number, p.GetRepo().GetFullName(), err)
				return
			}

			for _, f := range files.([]*github.CommitFile) {
				allFiles = append(allFiles, f.GetFilename())
			}

			if resp.NextPage == 0 {
				break
			}

			opt.Page = resp.NextPage
		}

		pr, discoveredUsers := gh.ConvertPullRequest(
			p.GetOrganization().GetLogin(),
			p.GetRepo().GetName(),
			p.GetPullRequest(),
			allFiles)
		prs := []*storage.PullRequest{pr}
		if err := r.cache.WritePullRequests(context, prs); err != nil {
			scope.Errorf(err.Error())
		}

		event := &storage.PullRequestEvent{
			OrgLogin:          pr.OrgLogin,
			RepoName:          pr.RepoName,
			PullRequestNumber: pr.PullRequestNumber,
			CreatedAt:         time.Now(),
			Actor:             p.GetSender().GetLogin(),
			Action:            p.GetAction(),
		}

		events := []*storage.PullRequestEvent{event}
		if err := r.store.WritePullRequestEvents(context, events); err != nil {
			scope.Error(err.Error())
			return
		}

		r.syncUsers(context, discoveredUsers)

	case *github.PullRequestReviewEvent:
		scope.Infof("Received PullRequestReviewEvent: %s, %d, %s", p.GetRepo().GetFullName(), p.GetPullRequest().GetNumber(), p.GetAction())

		if !r.repos[p.GetRepo().GetFullName()] {
			scope.Infof("Ignoring PR review for PR %d from repo %s since it's not in a monitored repo", p.PullRequest.Number, p.GetRepo().GetFullName())
			return
		}

		review, discoveredUsers := gh.ConvertPullRequestReview(
			p.GetOrganization().GetLogin(),
			p.GetRepo().GetName(),
			p.GetPullRequest().GetNumber(),
			p.GetReview())
		reviews := []*storage.PullRequestReview{review}
		if err := r.cache.WritePullRequestReviews(context, reviews); err != nil {
			scope.Errorf(err.Error())
		}

		event := &storage.PullRequestReviewEvent{
			OrgLogin:            review.OrgLogin,
			RepoName:            review.RepoName,
			PullRequestNumber:   review.PullRequestNumber,
			PullRequestReviewID: p.GetReview().GetID(),
			CreatedAt:           time.Now(),
			Actor:               p.GetSender().GetLogin(),
			Action:              p.GetAction(),
		}

		events := []*storage.PullRequestReviewEvent{event}
		if err := r.store.WritePullRequestReviewEvents(context, events); err != nil {
			scope.Error(err.Error())
			return
		}

		r.syncUsers(context, discoveredUsers)

	case github.PullRequestReviewCommentEvent:
		scope.Infof("Received PullRequestReviewCommentEvent: %s, %d, %s", p.GetRepo().GetFullName(), p.GetPullRequest().GetNumber(), p.GetAction())

		if !r.repos[p.GetRepo().GetFullName()] {
			scope.Infof("Ignoring PR review comment for PR %d from repo %s since it's not in a monitored repo", p.PullRequest.Number, p.GetRepo().GetFullName())
			return
		}

		comment, discoveredUsers := gh.ConvertPullRequestReviewComment(
			p.GetRepo().GetOwner().GetLogin(),
			p.GetRepo().GetName(),
			p.GetPullRequest().GetNumber(),
			p.GetComment())
		comments := []*storage.PullRequestReviewComment{comment}
		if err := r.cache.WritePullRequestReviewComments(context, comments); err != nil {
			scope.Errorf(err.Error())
		}

		event := &storage.PullRequestReviewCommentEvent{
			OrgLogin:                   comment.OrgLogin,
			RepoName:                   comment.RepoName,
			PullRequestNumber:          comment.PullRequestNumber,
			PullRequestReviewCommentID: p.GetComment().GetID(),
			CreatedAt:                  time.Now(),
			Actor:                      p.GetSender().GetLogin(),
			Action:                     p.GetAction(),
		}

		events := []*storage.PullRequestReviewCommentEvent{event}
		if err := r.store.WritePullRequestReviewCommentEvents(context, events); err != nil {
			scope.Error(err.Error())
			return
		}

		r.syncUsers(context, discoveredUsers)

	case *github.CommitCommentEvent:
		scope.Infof("Received CommitCommentEvent: %s, %s", p.GetRepo().GetFullName(), p.GetAction())

		if !r.repos[p.GetRepo().GetFullName()] {
			scope.Infof("Ignoring repo comment from repo %s since it's not in a monitored repo", p.GetRepo().GetFullName())
			return
		}

		comment, discoveredUsers := gh.ConvertRepoComment(
			p.GetRepo().GetOwner().GetLogin(),
			p.GetRepo().GetName(),
			p.GetComment())
		comments := []*storage.RepoComment{comment}
		if err := r.cache.WriteRepoComments(context, comments); err != nil {
			scope.Errorf(err.Error())
		}

		event := &storage.RepoCommentEvent{
			OrgLogin:      comment.OrgLogin,
			RepoName:      comment.RepoName,
			RepoCommentID: p.GetComment().GetID(),
			CreatedAt:     time.Now(),
			Actor:         p.GetSender().GetLogin(),
			Action:        p.GetAction(),
		}

		events := []*storage.RepoCommentEvent{event}
		if err := r.store.WriteRepoCommentEvents(context, events); err != nil {
			scope.Error(err.Error())
			return
		}

		r.syncUsers(context, discoveredUsers)

	default:
		// not what we're looking for
		scope.Debugf("Unknown event received: %T %+v", p, p)
		return
	}
}

func (r *Refresher) syncUsers(context context.Context, users []*storage.User) {
	if err := r.cache.WriteUsers(context, users); err != nil {
		scope.Errorf("Unable to write users: %v", err)
	}
}
