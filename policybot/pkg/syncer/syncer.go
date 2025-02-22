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

package syncer

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/google/go-github/v26/github"

	"istio.io/bots/policybot/pkg/config"
	"istio.io/bots/policybot/pkg/gh"
	"istio.io/bots/policybot/pkg/storage"
	"istio.io/bots/policybot/pkg/storage/cache"
	"istio.io/bots/policybot/pkg/zh"
	"istio.io/pkg/log"
)

// Syncer is responsible for synchronizing state from GitHub and ZenHub into our local store
type Syncer struct {
	cache *cache.Cache
	gc    *gh.ThrottledClient
	zc    *zh.ThrottledClient
	store storage.Store
	orgs  []config.Org
}

type FilterFlags int

// the things to sync
const (
	Issues       FilterFlags = 1 << 0
	Prs                      = 1 << 1
	Maintainers              = 1 << 2
	Members                  = 1 << 3
	Labels                   = 1 << 4
	ZenHub                   = 1 << 5
	RepoComments             = 1 << 6
	Events                   = 1 << 7
)

// The state in Syncer is immutable once created. syncState on the other hand represents
// the mutable state used during a single sync operation.
type syncState struct {
	syncer *Syncer
	users  map[string]*storage.User
	flags  FilterFlags
	ctx    context.Context
}

var scope = log.RegisterScope("syncer", "The GitHub data syncer", 0)

func New(gc *gh.ThrottledClient, cache *cache.Cache,
	zc *zh.ThrottledClient, store storage.Store, orgs []config.Org) *Syncer {
	return &Syncer{
		gc:    gc,
		cache: cache,
		zc:    zc,
		store: store,
		orgs:  orgs,
	}
}

func ConvFilterFlags(filter string) (FilterFlags, error) {
	if filter == "" {
		// defaults to everything
		return Issues | Prs | Maintainers | Members | Labels | ZenHub | RepoComments | Events, nil
	}

	var result FilterFlags
	for _, f := range strings.Split(filter, ",") {
		switch f {
		case "issues":
			result |= Issues
		case "prs":
			result |= Prs
		case "maintainers":
			result |= Maintainers
		case "members":
			result |= Members
		case "labels":
			result |= Labels
		case "zenhub":
			result |= ZenHub
		case "repocomments":
			result |= RepoComments
		case "events":
			result |= Events
		default:
			return 0, fmt.Errorf("unknown filter flag %s", f)
		}
	}

	return result, nil
}

func (s *Syncer) Sync(context context.Context, flags FilterFlags) error {
	ss := &syncState{
		syncer: s,
		users:  make(map[string]*storage.User),
		flags:  flags,
		ctx:    context,
	}

	var orgs []*storage.Org
	var repos []*storage.Repo

	// get all the org & repo info
	if err := s.fetchOrgs(ss.ctx, func(org *github.Organization) error {
		orgs = append(orgs, gh.ConvertOrg(org))
		return s.fetchRepos(ss.ctx, func(repo *github.Repository) error {
			repos = append(repos, gh.ConvertRepo(repo))
			return nil
		})
	}); err != nil {
		return err
	}

	if err := s.store.WriteOrgs(ss.ctx, orgs); err != nil {
		return err
	}

	if err := s.store.WriteRepos(ss.ctx, repos); err != nil {
		return err
	}

	for _, org := range orgs {
		var orgRepos []*storage.Repo
		for _, repo := range repos {
			if repo.OrgLogin == org.OrgLogin {
				orgRepos = append(orgRepos, repo)
			}
		}

		if flags&(Members|Labels|Issues|Prs|ZenHub|RepoComments|Events) != 0 {
			if err := ss.handleOrg(org, orgRepos); err != nil {
				return err
			}
		}

		if flags&Maintainers != 0 {
			if err := ss.handleMaintainers(org, orgRepos); err != nil {
				return err
			}
		}
	}

	if err := ss.pushUsers(); err != nil {
		return err
	}

	return nil
}

func (ss *syncState) pushUsers() error {
	users := make([]*storage.User, 0, len(ss.users))
	for _, user := range ss.users {

		if user.Name == "" {
			// Turns out most listing operations return only incomplete users. If we find
			// a user without a name, we try to fetch the full user info from GitHub.

			if u, _, err := ss.syncer.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
				return client.Users.Get(ss.ctx, user.UserLogin)
			}); err == nil {
				user = gh.ConvertUser(u.(*github.User))
			}
		}

		users = append(users, user)
	}

	if err := ss.syncer.store.WriteUsers(ss.ctx, users); err != nil {
		return err
	}

	return nil
}

func (ss *syncState) handleOrg(org *storage.Org, repos []*storage.Repo) error {
	scope.Infof("Syncing org %s", org.OrgLogin)

	for _, repo := range repos {
		if err := ss.handleRepo(repo); err != nil {
			return err
		}
	}

	if ss.flags&Members != 0 {
		if err := ss.handleMembers(org); err != nil {
			return err
		}
	}

	return nil
}

func (ss *syncState) handleRepo(repo *storage.Repo) error {
	scope.Infof("Syncing repo %s/%s", repo.OrgLogin, repo.RepoName)

	if ss.flags&Labels != 0 {
		if err := ss.handleLabels(repo); err != nil {
			return err
		}
	}

	if ss.flags&Issues != 0 {
		if err := ss.handleActivity(repo, ss.handleIssues, func(activity *storage.BotActivity) *time.Time {
			return &activity.LastIssueSyncStart
		}); err != nil {
			return err
		}

		if err := ss.handleActivity(repo, ss.handleIssueComments, func(activity *storage.BotActivity) *time.Time {
			return &activity.LastIssueCommentSyncStart
		}); err != nil {
			return err
		}

	}

	if ss.flags&ZenHub != 0 {
		if err := ss.handleZenHub(repo); err != nil {
			return err
		}
	}

	if ss.flags&Prs != 0 {
		if err := ss.handlePullRequests(repo); err != nil {
			return err
		}

		if err := ss.handleActivity(repo, ss.handlePullRequestReviewComments, func(activity *storage.BotActivity) *time.Time {
			return &activity.LastPullRequestReviewCommentSyncStart
		}); err != nil {
			return err
		}
	}

	if ss.flags&RepoComments != 0 {
		if err := ss.handleRepoComments(repo); err != nil {
			return err
		}
	}

	if ss.flags&Events != 0 {
		if err := ss.handleEvents(repo); err != nil {
			return err
		}
	}

	return nil
}

func (ss *syncState) handleActivity(repo *storage.Repo, cb func(*storage.Repo, time.Time) error,
	getField func(*storage.BotActivity) *time.Time) error {

	start := time.Now().UTC()
	priorStart := time.Time{}

	if activity, _ := ss.syncer.store.ReadBotActivity(ss.ctx, repo.OrgLogin, repo.RepoName); activity != nil {
		priorStart = *getField(activity)
	}

	if err := cb(repo, priorStart); err != nil {
		return err
	}

	if err := ss.syncer.store.UpdateBotActivity(ss.ctx, repo.OrgLogin, repo.RepoName, func(act *storage.BotActivity) error {
		if *getField(act) == priorStart {
			*getField(act) = start
		}
		return nil
	}); err != nil {
		scope.Warnf("unable to update bot activity for repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
	}

	return nil
}

func (ss *syncState) handleMembers(org *storage.Org) error {
	scope.Debugf("Getting members from org %s", org.OrgLogin)

	var storageMembers []*storage.Member
	if err := ss.syncer.fetchMembers(ss.ctx, org, func(members []*github.User) error {
		for _, member := range members {
			ss.addUsers(gh.ConvertUser(member))
			storageMembers = append(storageMembers, &storage.Member{OrgLogin: org.OrgLogin, UserLogin: member.GetLogin()})
		}

		return nil
	}); err != nil {
		return err
	}

	return ss.syncer.store.WriteAllMembers(ss.ctx, storageMembers)
}

func (ss *syncState) handleLabels(repo *storage.Repo) error {
	scope.Debugf("Getting labels from repo %s/%s", repo.OrgLogin, repo.RepoName)

	return ss.syncer.fetchLabels(ss.ctx, repo, func(labels []*github.Label) error {
		storageLabels := make([]*storage.Label, 0, len(labels))
		for _, label := range labels {
			storageLabels = append(storageLabels, gh.ConvertLabel(repo.OrgLogin, repo.RepoName, label))
		}

		return ss.syncer.store.WriteLabels(ss.ctx, storageLabels)
	})
}

func (ss *syncState) handleEvents(repo *storage.Repo) error {
	scope.Debugf("Getting events from repo %s/%s", repo.OrgLogin, repo.RepoName)

	total := 0
	err := ss.syncer.fetchRepoEvents(ss.ctx, repo, func(events []*github.Event) error {
		var issueEvents []*storage.IssueEvent
		var issueCommentEvents []*storage.IssueCommentEvent
		var prEvents []*storage.PullRequestEvent
		var prCommentEvents []*storage.PullRequestReviewCommentEvent
		var prReviewEvents []*storage.PullRequestReviewEvent

		total += len(events)
		scope.Infof("Received %d events", total)

		for _, event := range events {
			switch *event.Type {
			case "IssueEvent":
				payload, err := event.ParsePayload()
				if err != nil {
					scope.Errorf("unable to parse payload for issue event: %v", err)
					continue
				}

				p := payload.(*github.IssueEvent)
				issueEvents = append(issueEvents, &storage.IssueEvent{
					OrgLogin:    repo.OrgLogin,
					RepoName:    repo.RepoName,
					IssueNumber: int64(p.GetIssue().GetNumber()),
					CreatedAt:   event.GetCreatedAt(),
					Actor:       event.GetActor().GetLogin(),
					Action:      p.GetEvent(),
				})

			case "IssueCommentEvent":
				payload, err := event.ParsePayload()
				if err != nil {
					scope.Errorf("unable to parse payload for issue comment event: %v", err)
					continue
				}

				p := payload.(*github.IssueCommentEvent)

				issueCommentEvents = append(issueCommentEvents, &storage.IssueCommentEvent{
					OrgLogin:       repo.OrgLogin,
					RepoName:       repo.RepoName,
					IssueNumber:    int64(p.GetIssue().GetNumber()),
					IssueCommentID: p.GetComment().GetID(),
					CreatedAt:      event.GetCreatedAt(),
					Actor:          event.GetActor().GetLogin(),
					Action:         p.GetAction(),
				})

			case "PullRequestEvent":
				payload, err := event.ParsePayload()
				if err != nil {
					scope.Errorf("unable to parse payload for pull request event: %v", err)
					continue
				}

				p := payload.(*github.PullRequestEvent)
				prEvents = append(prEvents, &storage.PullRequestEvent{
					OrgLogin:          repo.OrgLogin,
					RepoName:          repo.RepoName,
					PullRequestNumber: int64(p.GetPullRequest().GetNumber()),
					CreatedAt:         event.GetCreatedAt(),
					Actor:             event.GetActor().GetLogin(),
					Action:            p.GetAction(),
				})

			case "PullRequestCommentEvent":
				payload, err := event.ParsePayload()
				if err != nil {
					scope.Errorf("unable to parse payload for pull request review comment event: %v", err)
					continue
				}

				p := payload.(*github.PullRequestReviewCommentEvent)
				prCommentEvents = append(prCommentEvents, &storage.PullRequestReviewCommentEvent{
					OrgLogin:                   repo.OrgLogin,
					RepoName:                   repo.RepoName,
					PullRequestNumber:          int64(p.GetPullRequest().GetNumber()),
					PullRequestReviewCommentID: p.GetComment().GetID(),
					CreatedAt:                  event.GetCreatedAt(),
					Actor:                      event.GetActor().GetLogin(),
					Action:                     p.GetAction(),
				})

			case "PullRequestReviewEvent":
				payload, err := event.ParsePayload()
				if err != nil {
					scope.Errorf("unable to parse payload for pull request review event: %v", err)
					continue
				}

				p := payload.(*github.PullRequestReviewEvent)
				prReviewEvents = append(prReviewEvents, &storage.PullRequestReviewEvent{
					OrgLogin:            repo.OrgLogin,
					RepoName:            repo.RepoName,
					PullRequestNumber:   int64(p.GetPullRequest().GetNumber()),
					PullRequestReviewID: p.GetReview().GetID(),
					CreatedAt:           event.GetCreatedAt(),
					Actor:               event.GetActor().GetLogin(),
					Action:              p.GetAction(),
				})
			}
		}

		if len(issueEvents) > 0 {
			if err := ss.syncer.store.WriteIssueEvents(ss.ctx, issueEvents); err != nil {
				return fmt.Errorf("unable to write issue events to storage: %v", err)
			}
		}

		if len(issueCommentEvents) > 0 {
			if err := ss.syncer.store.WriteIssueCommentEvents(ss.ctx, issueCommentEvents); err != nil {
				return fmt.Errorf("unable to write issue comment events to storage: %v", err)
			}
		}

		if len(prEvents) > 0 {
			if err := ss.syncer.store.WritePullRequestEvents(ss.ctx, prEvents); err != nil {
				return fmt.Errorf("unable to write pull request events to storage: %v", err)
			}
		}

		if len(prCommentEvents) > 0 {
			if err := ss.syncer.store.WritePullRequestReviewCommentEvents(ss.ctx, prCommentEvents); err != nil {
				return fmt.Errorf("unable to write pull request review comment events to storage: %v", err)
			}
		}

		if len(prReviewEvents) > 0 {
			if err := ss.syncer.store.WritePullRequestReviewEvents(ss.ctx, prReviewEvents); err != nil {
				return fmt.Errorf("unable to write pull request review events to storage: %v", err)
			}
		}

		return nil
	})

	if err != nil {
		return err
	}

	return ss.syncer.fetchIssueEvents(ss.ctx, repo, func(events []*github.IssueEvent) error {
		var issueEvents []*storage.IssueEvent

		total += len(events)
		scope.Infof("Received %d events", total)

		for _, event := range events {
			issueEvents = append(issueEvents, &storage.IssueEvent{
				OrgLogin:    repo.OrgLogin,
				RepoName:    repo.RepoName,
				IssueNumber: int64(event.GetIssue().GetNumber()),
				CreatedAt:   event.GetCreatedAt(),
				Actor:       event.GetActor().GetLogin(),
				Action:      event.GetEvent(),
			})
		}

		if len(issueEvents) > 0 {
			if err := ss.syncer.store.WriteIssueEvents(ss.ctx, issueEvents); err != nil {
				return fmt.Errorf("unable to write issue events to storage: %v", err)
			}
		}

		return nil
	})
}

func (ss *syncState) handleRepoComments(repo *storage.Repo) error {
	scope.Debugf("Getting comments for repo %s/%s", repo.OrgLogin, repo.RepoName)

	return ss.syncer.fetchRepoComments(ss.ctx, repo, func(comments []*github.RepositoryComment) error {
		storageComments := make([]*storage.RepoComment, 0, len(comments))
		for _, comment := range comments {
			t, users := gh.ConvertRepoComment(repo.OrgLogin, repo.RepoName, comment)
			storageComments = append(storageComments, t)
			ss.addUsers(users...)
		}

		return ss.syncer.store.WriteRepoComments(ss.ctx, storageComments)
	})
}

func (ss *syncState) handleIssues(repo *storage.Repo, startTime time.Time) error {
	scope.Debugf("Getting issues from repo %s/%s", repo.OrgLogin, repo.RepoName)

	total := 0
	return ss.syncer.fetchIssues(ss.ctx, repo, startTime, func(issues []*github.Issue) error {
		var storageIssues []*storage.Issue

		total += len(issues)
		scope.Infof("Received %d issues", total)

		for _, issue := range issues {
			t, users := gh.ConvertIssue(repo.OrgLogin, repo.RepoName, issue)
			storageIssues = append(storageIssues, t)
			ss.addUsers(users...)
		}

		return ss.syncer.store.WriteIssues(ss.ctx, storageIssues)
	})
}

func (ss *syncState) handleIssueComments(repo *storage.Repo, startTime time.Time) error {
	scope.Debugf("Getting issue comments from repo %s/%s", repo.OrgLogin, repo.RepoName)

	total := 0
	return ss.syncer.fetchIssueComments(ss.ctx, repo, startTime, func(comments []*github.IssueComment) error {
		var storageIssueComments []*storage.IssueComment

		total += len(comments)
		scope.Infof("Received %d issue comments", total)

		for _, comment := range comments {
			issueURL := comment.GetIssueURL()
			issueNumber, _ := strconv.Atoi(issueURL[strings.LastIndex(issueURL, "/")+1:])
			t, users := gh.ConvertIssueComment(repo.OrgLogin, repo.RepoName, issueNumber, comment)
			storageIssueComments = append(storageIssueComments, t)
			ss.addUsers(users...)
		}

		return ss.syncer.store.WriteIssueComments(ss.ctx, storageIssueComments)
	})
}

func (ss *syncState) handleZenHub(repo *storage.Repo) error {
	scope.Debugf("Getting ZenHub issue data for repo %s/%s", repo.OrgLogin, repo.RepoName)

	// get all the issues
	var issues []*storage.Issue
	if err := ss.syncer.store.QueryIssuesByRepo(ss.ctx, repo.OrgLogin, repo.RepoName, func(issue *storage.Issue) error {
		issues = append(issues, issue)
		return nil
	}); err != nil {
		return fmt.Errorf("unable to read issues from repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
	}

	// now get the ZenHub data for all issues
	var pipelines []*storage.IssuePipeline
	for _, issue := range issues {
		issueData, err := ss.syncer.zc.ThrottledCall(func(client *zh.Client) (interface{}, error) {
			return client.GetIssueData(int(repo.RepoNumber), int(issue.IssueNumber))
		})

		if err != nil {
			if err == zh.ErrNotFound {
				// not found, so nothing to do...
				return nil
			}

			return fmt.Errorf("unable to get issue data from ZenHub for issue %d in repo %s/%s: %v", issue.IssueNumber, repo.OrgLogin, repo.RepoName, err)
		}

		pipelines = append(pipelines, &storage.IssuePipeline{
			OrgLogin:    repo.OrgLogin,
			RepoName:    repo.RepoName,
			IssueNumber: issue.IssueNumber,
			Pipeline:    issueData.(*zh.IssueData).Pipeline.Name,
		})

		if len(pipelines)%100 == 0 {
			if err = ss.syncer.store.WriteIssuePipelines(ss.ctx, pipelines); err != nil {
				return err
			}
			pipelines = pipelines[:0]
		}
	}

	return ss.syncer.store.WriteIssuePipelines(ss.ctx, pipelines)
}

func (ss *syncState) handlePullRequests(repo *storage.Repo) error {
	scope.Debugf("Getting pull requests from repo %s/%s", repo.OrgLogin, repo.RepoName)

	total := 0
	return ss.syncer.fetchPullRequests(ss.ctx, repo, func(prs []*github.PullRequest) error {
		var storagePRs []*storage.PullRequest
		var storagePRReviews []*storage.PullRequestReview

		total += len(prs)
		scope.Infof("Received %d pull requests", total)

		for _, pr := range prs {
			// if this pr is already known to us and is up to date, skip further processing
			if existing, _ := ss.syncer.cache.ReadPullRequest(ss.ctx, repo.OrgLogin, repo.RepoName, pr.GetNumber()); existing != nil {
				if existing.UpdatedAt == pr.GetUpdatedAt() {
					continue
				}
			}

			if err := ss.syncer.fetchReviews(ss.ctx, repo, pr.GetNumber(), func(reviews []*github.PullRequestReview) error {
				for _, review := range reviews {
					t, users := gh.ConvertPullRequestReview(repo.OrgLogin, repo.RepoName, pr.GetNumber(), review)
					storagePRReviews = append(storagePRReviews, t)
					ss.addUsers(users...)
				}

				return nil
			}); err != nil {
				return err
			}

			var prFiles []string
			if err := ss.syncer.fetchFiles(ss.ctx, repo, pr.GetNumber(), func(files []string) error {
				prFiles = append(prFiles, files...)
				return nil
			}); err != nil {
				return err
			}

			t, users := gh.ConvertPullRequest(repo.OrgLogin, repo.RepoName, pr, prFiles)
			storagePRs = append(storagePRs, t)
			ss.addUsers(users...)
		}

		err := ss.syncer.store.WritePullRequests(ss.ctx, storagePRs)
		if err == nil {
			err = ss.syncer.store.WritePullRequestReviews(ss.ctx, storagePRReviews)
		}

		return err
	})
}

func (ss *syncState) handlePullRequestReviewComments(repo *storage.Repo, start time.Time) error {
	scope.Debugf("Getting pull requests review comments from repo %s/%s", repo.OrgLogin, repo.RepoName)

	total := 0
	return ss.syncer.fetchPullRequestReviewComments(ss.ctx, repo, start, func(comments []*github.PullRequestComment) error {
		var storagePRComments []*storage.PullRequestReviewComment

		total += len(comments)
		scope.Infof("Received %d pull request review comments", total)

		for _, comment := range comments {
			prURL := comment.GetPullRequestURL()
			prNumber, _ := strconv.Atoi(prURL[strings.LastIndex(prURL, "/")+1:])
			t, users := gh.ConvertPullRequestReviewComment(repo.OrgLogin, repo.RepoName, prNumber, comment)
			storagePRComments = append(storagePRComments, t)
			ss.addUsers(users...)
		}

		return ss.syncer.store.WritePullRequestReviewComments(ss.ctx, storagePRComments)
	})
}

func (ss *syncState) handleMaintainers(org *storage.Org, repos []*storage.Repo) error {
	scope.Debugf("Getting maintainers for org %s", org.OrgLogin)

	maintainers := make(map[string]*storage.Maintainer)

	for _, repo := range repos {
		fc, _, _, err := ss.syncer.gc.ThrottledCallTwoResult(func(client *github.Client) (interface{}, interface{}, *github.Response, error) {
			return client.Repositories.GetContents(ss.ctx, repo.OrgLogin, repo.RepoName, "CODEOWNERS", nil)
		})

		if err == nil {
			err = ss.handleCODEOWNERS(org, repo, maintainers, fc.(*github.RepositoryContent))
		} else {
			err = ss.handleOWNERS(org, repo, maintainers)
		}

		if err != nil {
			scope.Warnf("Unable to establish maintainers for repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
		}
	}

	storageMaintainers := make([]*storage.Maintainer, 0, len(maintainers))
	for _, maintainer := range maintainers {
		storageMaintainers = append(storageMaintainers, maintainer)
	}

	return ss.syncer.store.WriteAllMaintainers(ss.ctx, storageMaintainers)
}

func (ss *syncState) handleCODEOWNERS(org *storage.Org, repo *storage.Repo, maintainers map[string]*storage.Maintainer, fc *github.RepositoryContent) error {
	content, err := fc.GetContent()
	if err != nil {
		return fmt.Errorf("unable to read CODEOWNERS body from repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
	}

	lines := strings.Split(content, "\n")

	scope.Debugf("%d lines in CODEOWNERS file for repo %s/%s", len(lines), repo.OrgLogin, repo.RepoName)

	// go through each line of the CODEOWNERS file
	for _, line := range lines {
		l := strings.Trim(line, " \t")
		if strings.HasPrefix(l, "#") || l == "" {
			// skip comment lines or empty lines
			continue
		}

		fields := strings.Fields(l)
		logins := fields[1:]

		for _, login := range logins {
			login = strings.TrimPrefix(login, "@")

			// add the path to this maintainer's list
			path := strings.TrimPrefix(fields[0], "/")
			path = strings.TrimSuffix(path, "/*")
			if path == "*" {
				path = ""
			}

			scope.Debugf("User '%s' can review path '%s/%s/%s'", login, repo.OrgLogin, repo.RepoName, path)

			maintainer, err := ss.getMaintainer(org, maintainers, login)
			if maintainer == nil || err != nil {
				scope.Warnf("Couldn't get info on potential maintainer %s: %v", login, err)
				continue
			}

			maintainer.Paths = append(maintainer.Paths, repo.RepoName+"/"+path)
		}
	}

	return nil
}

type ownersFile struct {
	Approvers []string `json:"approvers"`
	Reviewers []string `json:"reviewers"`
}

func (ss *syncState) handleOWNERS(org *storage.Org, repo *storage.Repo, maintainers map[string]*storage.Maintainer) error {
	opt := &github.CommitsListOptions{
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	}

	// TODO: we need to get the SHA for the latest commit on the master branch, not just any branch
	rc, _, err := ss.syncer.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
		return client.Repositories.ListCommits(ss.ctx, repo.OrgLogin, repo.RepoName, opt)
	})

	if err != nil {
		return fmt.Errorf("unable to get latest commit in repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
	}

	tree, _, err := ss.syncer.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
		return client.Git.GetTree(ss.ctx, repo.OrgLogin, repo.RepoName, rc.([]*github.RepositoryCommit)[0].GetSHA(), true)
	})

	if err != nil {
		return fmt.Errorf("unable to get tree in repo %s/%s: %v", repo.OrgLogin, repo.RepoName, err)
	}

	files := make(map[string]ownersFile)
	for _, entry := range tree.(*github.Tree).Entries {
		components := strings.Split(entry.GetPath(), "/")
		if components[len(components)-1] == "OWNERS" && components[0] != "vendor" { // HACK: skip Go's vendor directory

			url := "https://raw.githubusercontent.com/" + repo.OrgLogin + "/" + repo.RepoName + "/master/" + entry.GetPath()

			resp, err := http.Get(url)
			if err != nil {
				return fmt.Errorf("unable to get %s: %v", url, err)
			}

			body, err := ioutil.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if err != nil {
				return fmt.Errorf("unable to read body for %s: %v", url, err)
			}

			var f ownersFile
			if err := yaml.Unmarshal(body, &f); err != nil {
				return fmt.Errorf("unable to parse body for %s: %v", url, err)
			}

			files[entry.GetPath()] = f
		}
	}

	scope.Debugf("%d OWNERS files found in repo %s/%s", len(files), org.OrgLogin, repo.RepoName)

	for path, file := range files {
		for _, user := range file.Approvers {
			maintainer, err := ss.getMaintainer(org, maintainers, user)
			if maintainer == nil || err != nil {
				scope.Warnf("Couldn't get info on potential maintainer %s: %v", user, err)
				continue
			}

			p := strings.TrimSuffix(path, "OWNERS")

			scope.Debugf("User '%s' can approve path %s/%s/%s", user, org.OrgLogin, repo.RepoName, p)

			maintainer.Paths = append(maintainer.Paths, repo.RepoName+"/"+p)
		}
	}

	return nil
}

func (ss *syncState) addUsers(users ...*storage.User) {
	for _, user := range users {
		ss.users[user.UserLogin] = user
	}
}

func (ss *syncState) getMaintainer(org *storage.Org, maintainers map[string]*storage.Maintainer, login string) (*storage.Maintainer, error) {
	user, ok := ss.users[login]
	if !ok {
		var err error
		user, err = ss.syncer.cache.ReadUser(ss.ctx, login)
		if err != nil {
			return nil, fmt.Errorf("unable to read information from storage for user %s: %v", login, err)
		}
	}

	if user == nil {
		// couldn't find user info, ask GitHub directly
		u, _, err := ss.syncer.gc.ThrottledCall(func(client *github.Client) (interface{}, *github.Response, error) {
			return client.Users.Get(ss.ctx, login)
		})

		if err != nil {
			return nil, fmt.Errorf("unable to read information from GitHub on user %s: %v", login, err)
		}

		user = gh.ConvertUser(u.(*github.User))
		ss.users[user.UserLogin] = user
	}

	maintainer, ok := maintainers[user.UserLogin]
	if !ok {
		// unknown maintainer, so create a record
		maintainer = &storage.Maintainer{
			OrgLogin:  org.OrgLogin,
			UserLogin: user.UserLogin,
		}
		maintainers[user.UserLogin] = maintainer
	}

	return maintainer, nil
}
