// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/models/webhook"
	"code.gitea.io/gitea/modules/commitstatus"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/json"
	api "code.gitea.io/gitea/modules/structs"
	webhook_module "code.gitea.io/gitea/modules/webhook"
	"code.gitea.io/gitea/tests"

	runnerv1 "code.gitea.io/actions-proto-go/runner/v1"
	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWebHookLink(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	session := loginUser(t, "user2")

	baseurl := "/user2/repo1/settings/hooks"
	tests := []string{
		// webhook list page
		baseurl,
		// new webhook page
		baseurl + "/gitea/new",
		// edit webhook page
		baseurl + "/1",
	}

	for _, url := range tests {
		resp := session.MakeRequest(t, NewRequest(t, "GET", url), http.StatusOK)
		htmlDoc := NewHTMLParser(t, resp.Body)
		menus := htmlDoc.doc.Find(".ui.top.attached.header .ui.dropdown .menu a")
		menus.Each(func(i int, menu *goquery.Selection) {
			url, exist := menu.Attr("href")
			assert.True(t, exist)
			assert.True(t, strings.HasPrefix(url, baseurl))
		})
	}
}

func testAPICreateWebhookForRepo(t *testing.T, session *TestSession, userName, repoName, url, event string, branchFilter ...string) {
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeAll)
	var branchFilterString string
	if len(branchFilter) > 0 {
		branchFilterString = branchFilter[0]
	}
	req := NewRequestWithJSON(t, "POST", "/api/v1/repos/"+userName+"/"+repoName+"/hooks", api.CreateHookOption{
		Type: "gitea",
		Config: api.CreateHookOptionConfig{
			"content_type": "json",
			"url":          url,
		},
		Events:       []string{event},
		Active:       true,
		BranchFilter: branchFilterString,
	}).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusCreated)
}

func testCreateWebhookForRepo(t *testing.T, session *TestSession, webhookType, userName, repoName, url, eventKind string) {
	csrf := GetUserCSRFToken(t, session)
	req := NewRequestWithValues(t, "POST", "/"+userName+"/"+repoName+"/settings/hooks/"+webhookType+"/new", map[string]string{
		"_csrf":        csrf,
		"payload_url":  url,
		"events":       eventKind,
		"active":       "true",
		"content_type": fmt.Sprintf("%d", webhook.ContentTypeJSON),
		"http_method":  "POST",
	})
	session.MakeRequest(t, req, http.StatusSeeOther)
}

func testAPICreateWebhookForOrg(t *testing.T, session *TestSession, userName, url, event string) {
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeAll)
	req := NewRequestWithJSON(t, "POST", "/api/v1/orgs/"+userName+"/hooks", api.CreateHookOption{
		Type: "gitea",
		Config: api.CreateHookOptionConfig{
			"content_type": "json",
			"url":          url,
		},
		Events: []string{event},
		Active: true,
	}).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusCreated)
}

type mockWebhookProvider struct {
	server *httptest.Server
}

func newMockWebhookProvider(callback func(r *http.Request), status int) *mockWebhookProvider {
	m := &mockWebhookProvider{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callback(r)
		w.WriteHeader(status)
	}))
	return m
}

func (m *mockWebhookProvider) URL() string {
	if m.server == nil {
		return ""
	}
	return m.server.URL
}

// Close closes the mock webhook http server
func (m *mockWebhookProvider) Close() {
	if m.server != nil {
		m.server.Close()
		m.server = nil
	}
}

func Test_WebhookCreate(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.CreatePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.CreatePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = string(webhook_module.HookEventCreate)
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "create")

		// 2. trigger the webhook
		testAPICreateBranch(t, session, "user2", "repo1", "master", "master2", http.StatusCreated)

		// 3. validate the webhook is triggered
		assert.Len(t, payloads, 1)
		assert.Equal(t, string(webhook_module.HookEventCreate), triggeredEvent)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)
		assert.Equal(t, "master2", payloads[0].Ref)
		assert.Equal(t, "branch", payloads[0].RefType)
	})
}

func Test_WebhookDelete(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.DeletePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.DeletePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "delete"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "delete")

		// 2. trigger the webhook
		testAPICreateBranch(t, session, "user2", "repo1", "master", "master2", http.StatusCreated)
		testAPIDeleteBranch(t, "master2", http.StatusNoContent)

		// 3. validate the webhook is triggered
		assert.Equal(t, "delete", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)
		assert.Equal(t, "master2", payloads[0].Ref)
		assert.Equal(t, "branch", payloads[0].RefType)
	})
}

func Test_WebhookFork(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.ForkPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.ForkPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "fork"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user1")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "fork")

		// 2. trigger the webhook
		testRepoFork(t, session, "user2", "repo1", "user1", "repo1-fork", "master")

		// 3. validate the webhook is triggered
		assert.Equal(t, "fork", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, "repo1-fork", payloads[0].Repo.Name)
		assert.Equal(t, "user1/repo1-fork", payloads[0].Repo.FullName)
		assert.Equal(t, "repo1", payloads[0].Forkee.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Forkee.FullName)
	})
}

func Test_WebhookIssueComment(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.IssueCommentPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.IssueCommentPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "issue_comment"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "issue_comment")

		t.Run("create comment", func(t *testing.T) {
			// 2. trigger the webhook
			issueURL := testNewIssue(t, session, "user2", "repo1", "Title2", "Description2")
			testIssueAddComment(t, session, issueURL, "issue title2 comment1", "")

			// 3. validate the webhook is triggered
			assert.Equal(t, "issue_comment", triggeredEvent)
			assert.Len(t, payloads, 1)
			assert.EqualValues(t, "created", payloads[0].Action)
			assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
			assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
			assert.Equal(t, "Title2", payloads[0].Issue.Title)
			assert.Equal(t, "Description2", payloads[0].Issue.Body)
			assert.Equal(t, "issue title2 comment1", payloads[0].Comment.Body)
		})

		t.Run("update comment", func(t *testing.T) {
			payloads = make([]api.IssueCommentPayload, 0, 2)
			triggeredEvent = ""

			// 2. trigger the webhook
			issueURL := testNewIssue(t, session, "user2", "repo1", "Title3", "Description3")
			commentID := testIssueAddComment(t, session, issueURL, "issue title3 comment1", "")
			modifiedContent := "issue title2 comment1 - modified"
			req := NewRequestWithValues(t, "POST", fmt.Sprintf("/%s/%s/comments/%d", "user2", "repo1", commentID), map[string]string{
				"_csrf":   GetUserCSRFToken(t, session),
				"content": modifiedContent,
			})
			session.MakeRequest(t, req, http.StatusOK)

			// 3. validate the webhook is triggered
			assert.Equal(t, "issue_comment", triggeredEvent)
			assert.Len(t, payloads, 2)
			assert.EqualValues(t, "edited", payloads[1].Action)
			assert.Equal(t, "repo1", payloads[1].Issue.Repo.Name)
			assert.Equal(t, "user2/repo1", payloads[1].Issue.Repo.FullName)
			assert.Equal(t, "Title3", payloads[1].Issue.Title)
			assert.Equal(t, "Description3", payloads[1].Issue.Body)
			assert.Equal(t, modifiedContent, payloads[1].Comment.Body)
		})

		t.Run("Update comment with no content change", func(t *testing.T) {
			payloads = make([]api.IssueCommentPayload, 0, 2)
			triggeredEvent = ""
			commentContent := "issue title3 comment1"

			// 2. trigger the webhook
			issueURL := testNewIssue(t, session, "user2", "repo1", "Title3", "Description3")
			commentID := testIssueAddComment(t, session, issueURL, commentContent, "")

			payloads = make([]api.IssueCommentPayload, 0, 2)
			triggeredEvent = ""
			req := NewRequestWithValues(t, "POST", fmt.Sprintf("/%s/%s/comments/%d", "user2", "repo1", commentID), map[string]string{
				"_csrf":   GetUserCSRFToken(t, session),
				"content": commentContent,
			})
			session.MakeRequest(t, req, http.StatusOK)

			// 3. validate the webhook is not triggered because no content change
			assert.Empty(t, triggeredEvent)
			assert.Empty(t, payloads)
		})
	})
}

func Test_WebhookRelease(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.ReleasePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.ReleasePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "release"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "release")

		// 2. trigger the webhook
		createNewRelease(t, session, "/user2/repo1", "v0.0.99", "v0.0.99", false, false)

		// 3. validate the webhook is triggered
		assert.Equal(t, "release", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, "repo1", payloads[0].Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repository.FullName)
		assert.Equal(t, "v0.0.99", payloads[0].Release.TagName)
		assert.False(t, payloads[0].Release.IsDraft)
		assert.False(t, payloads[0].Release.IsPrerelease)
	})
}

func Test_WebhookPush(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.PushPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.PushPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "push"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "push")

		// 2. trigger the webhook
		testCreateFile(t, session, "user2", "repo1", "master", "test_webhook_push.md", "# a test file for webhook push")

		// 3. validate the webhook is triggered
		assert.Equal(t, "push", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)
		assert.Len(t, payloads[0].Commits, 1)
		assert.Equal(t, []string{"test_webhook_push.md"}, payloads[0].Commits[0].Added)
	})
}

func Test_WebhookPushDevBranch(t *testing.T) {
	var payloads []api.PushPayload
	var triggeredEvent string
	provider := newMockWebhookProvider(func(r *http.Request) {
		content, _ := io.ReadAll(r.Body)
		var payload api.PushPayload
		err := json.Unmarshal(content, &payload)
		assert.NoError(t, err)
		payloads = append(payloads, payload)
		triggeredEvent = "push"
	}, http.StatusOK)
	defer provider.Close()

	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		// only for dev branch
		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "push", "develop")

		// 2. this should not trigger the webhook
		testCreateFile(t, session, "user2", "repo1", "master", "test_webhook_push.md", "# a test file for webhook push")
		assert.Empty(t, triggeredEvent)
		assert.Empty(t, payloads)

		// 3. trigger the webhook
		testCreateFile(t, session, "user2", "repo1", "develop", "test_webhook_push.md", "# a test file for webhook push")

		// 4. validate the webhook is triggered
		assert.Equal(t, "push", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "develop", payloads[0].Branch())
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)
		assert.Len(t, payloads[0].Commits, 1)
		assert.Equal(t, []string{"test_webhook_push.md"}, payloads[0].Commits[0].Added)
	})
}

func Test_WebhookIssue(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.IssuePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.IssuePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "issues"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "issues")

		// 2. trigger the webhook
		testNewIssue(t, session, "user2", "repo1", "Title1", "Description1")

		// 3. validate the webhook is triggered
		assert.Equal(t, "issues", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "opened", payloads[0].Action)
		assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
		assert.Equal(t, "Title1", payloads[0].Issue.Title)
		assert.Equal(t, "Description1", payloads[0].Issue.Body)
		assert.Positive(t, payloads[0].Issue.Created.Unix())
		assert.Positive(t, payloads[0].Issue.Updated.Unix())
	})
}

func Test_WebhookIssueDelete(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.IssuePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.IssuePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "issue"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")
		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "issues")
		issueURL := testNewIssue(t, session, "user2", "repo1", "Title1", "Description1")

		// 2. trigger the webhook
		testIssueDelete(t, session, issueURL)

		// 3. validate the webhook is triggered
		assert.Equal(t, "issue", triggeredEvent)
		require.Len(t, payloads, 2)
		assert.EqualValues(t, "deleted", payloads[1].Action)
		assert.Equal(t, "repo1", payloads[1].Issue.Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[1].Issue.Repo.FullName)
		assert.Equal(t, "Title1", payloads[1].Issue.Title)
		assert.Equal(t, "Description1", payloads[1].Issue.Body)
	})
}

func Test_WebhookIssueAssign(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.PullRequestPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.PullRequestPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "pull_request_assign"
		}, http.StatusOK)
		defer provider.Close()

		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "pull_request_assign")

		// 2. trigger the webhook, issue 2 is a pull request
		testIssueAssign(t, session, repo1.Link(), 2, user2.ID)

		// 3. validate the webhook is triggered
		assert.Equal(t, "pull_request_assign", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "assigned", payloads[0].Action)
		assert.Equal(t, "repo1", payloads[0].PullRequest.Base.Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[0].PullRequest.Base.Repository.FullName)
		assert.Equal(t, "issue2", payloads[0].PullRequest.Title)
		assert.Equal(t, "content for the second issue", payloads[0].PullRequest.Body)
		assert.Equal(t, user2.ID, payloads[0].PullRequest.Assignee.ID)
	})
}

func Test_WebhookIssueMilestone(t *testing.T) {
	var payloads []api.IssuePayload
	var triggeredEvent string
	provider := newMockWebhookProvider(func(r *http.Request) {
		content, _ := io.ReadAll(r.Body)
		var payload api.IssuePayload
		err := json.Unmarshal(content, &payload)
		assert.NoError(t, err)
		payloads = append(payloads, payload)
		triggeredEvent = "issues"
	}, http.StatusOK)
	defer provider.Close()

	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		// create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})
		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "issue_milestone")

		t.Run("assign a milestone", func(t *testing.T) {
			// trigger the webhook
			testIssueChangeMilestone(t, session, repo1.Link(), 1, 1)

			// validate the webhook is triggered
			assert.Equal(t, "issues", triggeredEvent)
			assert.Len(t, payloads, 1)
			assert.Equal(t, "milestoned", string(payloads[0].Action))
			assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
			assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
			assert.Equal(t, "issue1", payloads[0].Issue.Title)
			assert.Equal(t, "content for the first issue", payloads[0].Issue.Body)
			assert.EqualValues(t, 1, payloads[0].Issue.Milestone.ID)
		})

		t.Run("change a milestong", func(t *testing.T) {
			// trigger the webhook again
			triggeredEvent = ""
			payloads = make([]api.IssuePayload, 0, 1)
			// change milestone to 2
			testIssueChangeMilestone(t, session, repo1.Link(), 1, 2)

			// validate the webhook is triggered
			assert.Equal(t, "issues", triggeredEvent)
			assert.Len(t, payloads, 1)
			assert.Equal(t, "milestoned", string(payloads[0].Action))
			assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
			assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
			assert.Equal(t, "issue1", payloads[0].Issue.Title)
			assert.Equal(t, "content for the first issue", payloads[0].Issue.Body)
			assert.EqualValues(t, 2, payloads[0].Issue.Milestone.ID)
		})

		t.Run("remove a milestone", func(t *testing.T) {
			// trigger the webhook again
			triggeredEvent = ""
			payloads = make([]api.IssuePayload, 0, 1)
			// change milestone to 0
			testIssueChangeMilestone(t, session, repo1.Link(), 1, 0)

			// validate the webhook is triggered
			assert.Equal(t, "issues", triggeredEvent)
			assert.Len(t, payloads, 1)
			assert.Equal(t, "demilestoned", string(payloads[0].Action))
			assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
			assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
			assert.Equal(t, "issue1", payloads[0].Issue.Title)
			assert.Equal(t, "content for the first issue", payloads[0].Issue.Body)
			assert.Nil(t, payloads[0].Issue.Milestone)
		})
	})
}

func Test_WebhookPullRequest(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.PullRequestPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.PullRequestPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "pull_request"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "pull_request")

		testAPICreateBranch(t, session, "user2", "repo1", "master", "master2", http.StatusCreated)
		// 2. trigger the webhook
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})
		testCreatePullToDefaultBranch(t, session, repo1, repo1, "master2", "first pull request")

		// 3. validate the webhook is triggered
		assert.Equal(t, "pull_request", triggeredEvent)
		require.Len(t, payloads, 1)
		assert.Equal(t, "repo1", payloads[0].PullRequest.Base.Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[0].PullRequest.Base.Repository.FullName)
		assert.Equal(t, "repo1", payloads[0].PullRequest.Head.Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[0].PullRequest.Head.Repository.FullName)
		assert.Equal(t, 0, *payloads[0].PullRequest.Additions)
		assert.Equal(t, 0, *payloads[0].PullRequest.ChangedFiles)
		assert.Equal(t, 0, *payloads[0].PullRequest.Deletions)
	})
}

func Test_WebhookPullRequestDelete(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.PullRequestPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.PullRequestPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "pull_request"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")
		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "pull_request")

		testAPICreateBranch(t, session, "user2", "repo1", "master", "master2", http.StatusCreated)

		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})
		issueURL := testCreatePullToDefaultBranch(t, session, repo1, repo1, "master2", "first pull request")

		// 2. trigger the webhook
		testIssueDelete(t, session, path.Join(repo1.Link(), "pulls", issueURL))

		// 3. validate the webhook is triggered
		assert.Equal(t, "pull_request", triggeredEvent)
		require.Len(t, payloads, 2)
		assert.EqualValues(t, "deleted", payloads[1].Action)
		assert.Equal(t, "repo1", payloads[1].PullRequest.Base.Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[1].PullRequest.Base.Repository.FullName)
		assert.Equal(t, 0, *payloads[1].PullRequest.Additions)
		assert.Equal(t, 0, *payloads[1].PullRequest.ChangedFiles)
		assert.Equal(t, 0, *payloads[1].PullRequest.Deletions)
	})
}

func Test_WebhookPullRequestComment(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.IssueCommentPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.IssueCommentPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "pull_request_comment"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "pull_request_comment")

		// 2. trigger the webhook
		testAPICreateBranch(t, session, "user2", "repo1", "master", "master2", http.StatusCreated)
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})
		prID := testCreatePullToDefaultBranch(t, session, repo1, repo1, "master2", "first pull request")

		testIssueAddComment(t, session, "/user2/repo1/pulls/"+prID, "pull title2 comment1", "")

		// 3. validate the webhook is triggered
		assert.Equal(t, "pull_request_comment", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "created", payloads[0].Action)
		assert.Equal(t, "repo1", payloads[0].Issue.Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Issue.Repo.FullName)
		assert.Equal(t, "first pull request", payloads[0].Issue.Title)
		assert.Empty(t, payloads[0].Issue.Body)
		assert.Equal(t, "pull title2 comment1", payloads[0].Comment.Body)
	})
}

func Test_WebhookWiki(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.WikiPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.WikiPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "wiki"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "wiki")

		// 2. trigger the webhook
		testAPICreateWikiPage(t, session, "user2", "repo1", "Test Wiki Page", http.StatusCreated)

		// 3. validate the webhook is triggered
		assert.Equal(t, "wiki", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "created", payloads[0].Action)
		assert.Equal(t, "repo1", payloads[0].Repository.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repository.FullName)
		assert.Equal(t, "Test-Wiki-Page", payloads[0].Page)
	})
}

func Test_WebhookRepository(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.RepositoryPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.RepositoryPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "repository"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user1")

		testAPICreateWebhookForOrg(t, session, "org3", provider.URL(), "repository")

		// 2. trigger the webhook
		testAPIOrgCreateRepo(t, session, "org3", "repo_new", http.StatusCreated)

		// 3. validate the webhook is triggered
		assert.Equal(t, "repository", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "created", payloads[0].Action)
		assert.Equal(t, "org3", payloads[0].Organization.UserName)
		assert.Equal(t, "repo_new", payloads[0].Repository.Name)
		assert.Equal(t, "org3/repo_new", payloads[0].Repository.FullName)
	})
}

func Test_WebhookPackage(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.PackagePayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			content, _ := io.ReadAll(r.Body)
			var payload api.PackagePayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "package"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user1")

		testAPICreateWebhookForOrg(t, session, "org3", provider.URL(), "package")

		// 2. trigger the webhook
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeAll)
		url := fmt.Sprintf("/api/packages/%s/generic/%s/%s", "org3", "gitea", "v1.24.0")
		req := NewRequestWithBody(t, "PUT", url+"/gitea", strings.NewReader("This is a dummy file")).
			AddTokenAuth(token)
		MakeRequest(t, req, http.StatusCreated)

		// 3. validate the webhook is triggered
		assert.Equal(t, "package", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.EqualValues(t, "created", payloads[0].Action)
		assert.Equal(t, "gitea", payloads[0].Package.Name)
		assert.Equal(t, "generic", payloads[0].Package.Type)
		assert.Equal(t, "org3", payloads[0].Organization.UserName)
		assert.Equal(t, "v1.24.0", payloads[0].Package.Version)
	})
}

func Test_WebhookStatus(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.CommitStatusPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			assert.Contains(t, r.Header["X-Github-Event-Type"], "status", "X-GitHub-Event-Type should contain status")
			assert.Contains(t, r.Header["X-Github-Hook-Installation-Target-Type"], "repository", "X-GitHub-Hook-Installation-Target-Type should contain repository")
			assert.Contains(t, r.Header["X-Gitea-Event-Type"], "status", "X-Gitea-Event-Type should contain status")
			assert.Contains(t, r.Header["X-Gitea-Hook-Installation-Target-Type"], "repository", "X-Gitea-Hook-Installation-Target-Type should contain repository")
			assert.Contains(t, r.Header["X-Gogs-Event-Type"], "status", "X-Gogs-Event-Type should contain status")
			content, _ := io.ReadAll(r.Body)
			var payload api.CommitStatusPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "status"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "status")

		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})

		gitRepo1, err := gitrepo.OpenRepository(t.Context(), repo1)
		assert.NoError(t, err)
		commitID, err := gitRepo1.GetBranchCommitID(repo1.DefaultBranch)
		assert.NoError(t, err)

		// 2. trigger the webhook
		testCtx := NewAPITestContext(t, "user2", "repo1", auth_model.AccessTokenScopeAll)

		// update a status for a commit via API
		doAPICreateCommitStatus(testCtx, commitID, api.CreateStatusOption{
			State:       commitstatus.CommitStatusSuccess,
			TargetURL:   "http://test.ci/",
			Description: "",
			Context:     "testci",
		})(t)

		// 3. validate the webhook is triggered
		assert.Equal(t, "status", triggeredEvent)
		assert.Len(t, payloads, 1)
		assert.Equal(t, commitID, payloads[0].Commit.ID)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)
		assert.Equal(t, "testci", payloads[0].Context)
		assert.Equal(t, commitID, payloads[0].SHA)
	})
}

func Test_WebhookStatus_NoWrongTrigger(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var trigger string
		provider := newMockWebhookProvider(func(r *http.Request) {
			assert.NotContains(t, r.Header["X-Github-Event-Type"], "status", "X-GitHub-Event-Type should not contain status")
			assert.NotContains(t, r.Header["X-Gitea-Event-Type"], "status", "X-Gitea-Event-Type should not contain status")
			assert.NotContains(t, r.Header["X-Gogs-Event-Type"], "status", "X-Gogs-Event-Type should not contain status")
			trigger = "push"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		session := loginUser(t, "user2")

		// create a push_only webhook from web UI
		testCreateWebhookForRepo(t, session, "gitea", "user2", "repo1", provider.URL(), "push_only")

		// 2. trigger the webhook with a push action
		testCreateFile(t, session, "user2", "repo1", "master", "test_webhook_push.md", "# a test file for webhook push")

		// 3. validate the webhook is triggered with right event
		assert.Equal(t, "push", trigger)
	})
}

func Test_WebhookWorkflowJob(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
		var payloads []api.WorkflowJobPayload
		var triggeredEvent string
		provider := newMockWebhookProvider(func(r *http.Request) {
			assert.Contains(t, r.Header["X-Github-Event-Type"], "workflow_job", "X-GitHub-Event-Type should contain workflow_job")
			assert.Contains(t, r.Header["X-Gitea-Event-Type"], "workflow_job", "X-Gitea-Event-Type should contain workflow_job")
			assert.Contains(t, r.Header["X-Gogs-Event-Type"], "workflow_job", "X-Gogs-Event-Type should contain workflow_job")
			content, _ := io.ReadAll(r.Body)
			var payload api.WorkflowJobPayload
			err := json.Unmarshal(content, &payload)
			assert.NoError(t, err)
			payloads = append(payloads, payload)
			triggeredEvent = "workflow_job"
		}, http.StatusOK)
		defer provider.Close()

		// 1. create a new webhook with special webhook for repo1
		user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		session := loginUser(t, "user2")
		token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

		testAPICreateWebhookForRepo(t, session, "user2", "repo1", provider.URL(), "workflow_job")

		repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})

		gitRepo1, err := gitrepo.OpenRepository(t.Context(), repo1)
		assert.NoError(t, err)

		runner := newMockRunner()
		runner.registerAsRepoRunner(t, "user2", "repo1", "mock-runner", []string{"ubuntu-latest"}, false)

		// 2. trigger the webhooks

		// add workflow file to the repo
		// init the workflow
		wfTreePath := ".gitea/workflows/push.yml"
		wfFileContent := `name: Push
on: push
jobs:
  wf1-job:
    runs-on: ubuntu-latest
    steps:
      - run: echo 'test the webhook'
  wf2-job:
    runs-on: ubuntu-latest
    needs: wf1-job
    steps:
      - run: echo 'cmd 1'
      - run: echo 'cmd 2'
`
		opts := getWorkflowCreateFileOptions(user2, repo1.DefaultBranch, "create "+wfTreePath, wfFileContent)
		createWorkflowFile(t, token, "user2", "repo1", wfTreePath, opts)

		commitID, err := gitRepo1.GetBranchCommitID(repo1.DefaultBranch)
		assert.NoError(t, err)

		// 3. validate the webhook is triggered
		assert.Equal(t, "workflow_job", triggeredEvent)
		assert.Len(t, payloads, 2)
		assert.Equal(t, "queued", payloads[0].Action)
		assert.Equal(t, "queued", payloads[0].WorkflowJob.Status)
		assert.Equal(t, []string{"ubuntu-latest"}, payloads[0].WorkflowJob.Labels)
		assert.Equal(t, commitID, payloads[0].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[0].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[0].Repo.FullName)

		assert.Equal(t, "waiting", payloads[1].Action)
		assert.Equal(t, "waiting", payloads[1].WorkflowJob.Status)
		assert.Equal(t, commitID, payloads[1].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[1].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[1].Repo.FullName)

		// 4. Execute a single Job
		task := runner.fetchTask(t)
		outcome := &mockTaskOutcome{
			result: runnerv1.Result_RESULT_SUCCESS,
		}
		runner.execTask(t, task, outcome)

		// 5. validate the webhook is triggered
		assert.Equal(t, "workflow_job", triggeredEvent)
		assert.Len(t, payloads, 5)
		assert.Equal(t, "in_progress", payloads[2].Action)
		assert.Equal(t, "in_progress", payloads[2].WorkflowJob.Status)
		assert.Equal(t, "mock-runner", payloads[2].WorkflowJob.RunnerName)
		assert.Equal(t, commitID, payloads[2].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[2].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[2].Repo.FullName)

		assert.Equal(t, "completed", payloads[3].Action)
		assert.Equal(t, "completed", payloads[3].WorkflowJob.Status)
		assert.Equal(t, "mock-runner", payloads[3].WorkflowJob.RunnerName)
		assert.Equal(t, "success", payloads[3].WorkflowJob.Conclusion)
		assert.Equal(t, commitID, payloads[3].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[3].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[3].Repo.FullName)
		assert.Contains(t, payloads[3].WorkflowJob.URL, fmt.Sprintf("/actions/jobs/%d", payloads[3].WorkflowJob.ID))
		assert.Contains(t, payloads[3].WorkflowJob.HTMLURL, fmt.Sprintf("/jobs/%d", 0))
		assert.Len(t, payloads[3].WorkflowJob.Steps, 1)

		assert.Equal(t, "queued", payloads[4].Action)
		assert.Equal(t, "queued", payloads[4].WorkflowJob.Status)
		assert.Equal(t, []string{"ubuntu-latest"}, payloads[4].WorkflowJob.Labels)
		assert.Equal(t, commitID, payloads[4].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[4].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[4].Repo.FullName)

		// 6. Execute a single Job
		task = runner.fetchTask(t)
		outcome = &mockTaskOutcome{
			result: runnerv1.Result_RESULT_FAILURE,
		}
		runner.execTask(t, task, outcome)

		// 7. validate the webhook is triggered
		assert.Equal(t, "workflow_job", triggeredEvent)
		assert.Len(t, payloads, 7)
		assert.Equal(t, "in_progress", payloads[5].Action)
		assert.Equal(t, "in_progress", payloads[5].WorkflowJob.Status)
		assert.Equal(t, "mock-runner", payloads[5].WorkflowJob.RunnerName)

		assert.Equal(t, commitID, payloads[5].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[5].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[5].Repo.FullName)

		assert.Equal(t, "completed", payloads[6].Action)
		assert.Equal(t, "completed", payloads[6].WorkflowJob.Status)
		assert.Equal(t, "failure", payloads[6].WorkflowJob.Conclusion)
		assert.Equal(t, "mock-runner", payloads[6].WorkflowJob.RunnerName)
		assert.Equal(t, commitID, payloads[6].WorkflowJob.HeadSha)
		assert.Equal(t, "repo1", payloads[6].Repo.Name)
		assert.Equal(t, "user2/repo1", payloads[6].Repo.FullName)
		assert.Contains(t, payloads[6].WorkflowJob.URL, fmt.Sprintf("/actions/jobs/%d", payloads[6].WorkflowJob.ID))
		assert.Contains(t, payloads[6].WorkflowJob.HTMLURL, fmt.Sprintf("/jobs/%d", 1))
		assert.Len(t, payloads[6].WorkflowJob.Steps, 2)
	})
}

type workflowRunWebhook struct {
	URL            string
	payloads       []api.WorkflowRunPayload
	triggeredEvent string
}

func Test_WebhookWorkflowRun(t *testing.T) {
	webhookData := &workflowRunWebhook{}
	provider := newMockWebhookProvider(func(r *http.Request) {
		assert.Contains(t, r.Header["X-Github-Event-Type"], "workflow_run", "X-GitHub-Event-Type should contain workflow_run")
		assert.Contains(t, r.Header["X-Gitea-Event-Type"], "workflow_run", "X-Gitea-Event-Type should contain workflow_run")
		assert.Contains(t, r.Header["X-Gogs-Event-Type"], "workflow_run", "X-Gogs-Event-Type should contain workflow_run")
		content, _ := io.ReadAll(r.Body)
		var payload api.WorkflowRunPayload
		err := json.Unmarshal(content, &payload)
		assert.NoError(t, err)
		webhookData.payloads = append(webhookData.payloads, payload)
		webhookData.triggeredEvent = "workflow_run"
	}, http.StatusOK)
	defer provider.Close()
	webhookData.URL = provider.URL()

	tests := []struct {
		name     string
		callback func(t *testing.T, webhookData *workflowRunWebhook)
	}{
		{
			name:     "WorkflowRun",
			callback: testWebhookWorkflowRun,
		},
		{
			name:     "WorkflowRunDepthLimit",
			callback: testWebhookWorkflowRunDepthLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			webhookData.payloads = nil
			webhookData.triggeredEvent = ""
			onGiteaRun(t, func(t *testing.T, giteaURL *url.URL) {
				test.callback(t, webhookData)
			})
		})
	}
}

func testWebhookWorkflowRun(t *testing.T, webhookData *workflowRunWebhook) {
	// 1. create a new webhook with special webhook for repo1
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := loginUser(t, "user2")
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

	testAPICreateWebhookForRepo(t, session, "user2", "repo1", webhookData.URL, "workflow_run")

	repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})

	gitRepo1, err := gitrepo.OpenRepository(t.Context(), repo1)
	assert.NoError(t, err)

	runner := newMockRunner()
	runner.registerAsRepoRunner(t, "user2", "repo1", "mock-runner", []string{"ubuntu-latest"}, false)

	// 2.1 add workflow_run workflow file to the repo

	opts := getWorkflowCreateFileOptions(user2, repo1.DefaultBranch, "create "+"dispatch.yml", `
on:
  workflow_run:
    workflows: ["Push"]
    types:
    - completed
jobs:
  dispatch:
    runs-on: ubuntu-latest
    steps:
      - run: echo 'test the webhook'
`)
	createWorkflowFile(t, token, "user2", "repo1", ".gitea/workflows/dispatch.yml", opts)

	// 2.2 trigger the webhooks

	// add workflow file to the repo
	// init the workflow
	wfTreePath := ".gitea/workflows/push.yml"
	wfFileContent := `name: Push
on: push
jobs:
  wf1-job:
    runs-on: ubuntu-latest
    steps:
      - run: echo 'test the webhook'
  wf2-job:
    runs-on: ubuntu-latest
    needs: wf1-job
    steps:
      - run: echo 'cmd 1'
      - run: echo 'cmd 2'
`
	opts = getWorkflowCreateFileOptions(user2, repo1.DefaultBranch, "create "+wfTreePath, wfFileContent)
	createWorkflowFile(t, token, "user2", "repo1", wfTreePath, opts)

	commitID, err := gitRepo1.GetBranchCommitID(repo1.DefaultBranch)
	assert.NoError(t, err)

	// 3. validate the webhook is triggered
	assert.Equal(t, "workflow_run", webhookData.triggeredEvent)
	assert.Len(t, webhookData.payloads, 1)
	assert.Equal(t, "requested", webhookData.payloads[0].Action)
	assert.Equal(t, "queued", webhookData.payloads[0].WorkflowRun.Status)
	assert.Equal(t, repo1.DefaultBranch, webhookData.payloads[0].WorkflowRun.HeadBranch)
	assert.Equal(t, commitID, webhookData.payloads[0].WorkflowRun.HeadSha)
	assert.Equal(t, "repo1", webhookData.payloads[0].Repo.Name)
	assert.Equal(t, "user2/repo1", webhookData.payloads[0].Repo.FullName)

	// 4. Execute two Jobs
	task := runner.fetchTask(t)
	outcome := &mockTaskOutcome{
		result: runnerv1.Result_RESULT_SUCCESS,
	}
	runner.execTask(t, task, outcome)

	task = runner.fetchTask(t)
	outcome = &mockTaskOutcome{
		result: runnerv1.Result_RESULT_FAILURE,
	}
	runner.execTask(t, task, outcome)

	// 7. validate the webhook is triggered
	assert.Equal(t, "workflow_run", webhookData.triggeredEvent)
	assert.Len(t, webhookData.payloads, 3)
	assert.Equal(t, "completed", webhookData.payloads[1].Action)
	assert.Equal(t, "push", webhookData.payloads[1].WorkflowRun.Event)

	// 3. validate the webhook is triggered
	assert.Equal(t, "workflow_run", webhookData.triggeredEvent)
	assert.Len(t, webhookData.payloads, 3)
	assert.Equal(t, "requested", webhookData.payloads[2].Action)
	assert.Equal(t, "queued", webhookData.payloads[2].WorkflowRun.Status)
	assert.Equal(t, "workflow_run", webhookData.payloads[2].WorkflowRun.Event)
	assert.Equal(t, repo1.DefaultBranch, webhookData.payloads[2].WorkflowRun.HeadBranch)
	assert.Equal(t, commitID, webhookData.payloads[2].WorkflowRun.HeadSha)
	assert.Equal(t, "repo1", webhookData.payloads[2].Repo.Name)
	assert.Equal(t, "user2/repo1", webhookData.payloads[2].Repo.FullName)
}

func testWebhookWorkflowRunDepthLimit(t *testing.T, webhookData *workflowRunWebhook) {
	// 1. create a new webhook with special webhook for repo1
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := loginUser(t, "user2")
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)

	testAPICreateWebhookForRepo(t, session, "user2", "repo1", webhookData.URL, "workflow_run")

	repo1 := unittest.AssertExistsAndLoadBean(t, &repo.Repository{ID: 1})

	gitRepo1, err := gitrepo.OpenRepository(t.Context(), repo1)
	assert.NoError(t, err)

	// 2. trigger the webhooks

	// add workflow file to the repo
	// init the workflow
	wfTreePath := ".gitea/workflows/push.yml"
	wfFileContent := `name: Endless Loop
on:
  push:
  workflow_run:
    types:
    - requested
jobs:
  dispatch:
    runs-on: ubuntu-latest
    steps:
      - run: echo 'test the webhook'
`
	opts := getWorkflowCreateFileOptions(user2, repo1.DefaultBranch, "create "+wfTreePath, wfFileContent)
	createWorkflowFile(t, token, "user2", "repo1", wfTreePath, opts)

	commitID, err := gitRepo1.GetBranchCommitID(repo1.DefaultBranch)
	assert.NoError(t, err)

	// 3. validate the webhook is triggered
	assert.Equal(t, "workflow_run", webhookData.triggeredEvent)
	// 1x push + 5x workflow_run requested chain
	assert.Len(t, webhookData.payloads, 6)
	for i := range 6 {
		assert.Equal(t, "requested", webhookData.payloads[i].Action)
		assert.Equal(t, "queued", webhookData.payloads[i].WorkflowRun.Status)
		assert.Equal(t, repo1.DefaultBranch, webhookData.payloads[i].WorkflowRun.HeadBranch)
		assert.Equal(t, commitID, webhookData.payloads[i].WorkflowRun.HeadSha)
		if i == 0 {
			assert.Equal(t, "push", webhookData.payloads[i].WorkflowRun.Event)
		} else {
			assert.Equal(t, "workflow_run", webhookData.payloads[i].WorkflowRun.Event)
		}
		assert.Equal(t, "repo1", webhookData.payloads[i].Repo.Name)
		assert.Equal(t, "user2/repo1", webhookData.payloads[i].Repo.FullName)
	}
}
