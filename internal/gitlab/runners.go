package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type RunnerPolicy struct {
	ID            int64
	Known         bool
	Paused        bool
	ProtectedOnly bool
	RunUntagged   bool
	Tags          []string
	// A project key exists only after its complete availability list is verified.
	Projects map[int64]bool
	Error    string
}

type apiRunnerPolicy struct {
	ID          string   `json:"id"`
	Paused      *bool    `json:"paused"`
	AccessLevel string   `json:"accessLevel"`
	RunUntagged *bool    `json:"runUntagged"`
	Tags        []string `json:"tagList"`
}

func (c *Client) runnerPolicy(ctx context.Context, id int64) (RunnerPolicy, error) {
	globalID := fmt.Sprintf("gid://gitlab/Ci::Runner/%d", id)
	body, err := json.Marshal(struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{
		Query:     `query RunnerPolicy($id: CiRunnerID!) { runner(id: $id) { id paused accessLevel runUntagged tagList } }`,
		Variables: map[string]string{"id": globalID},
	})
	if err != nil {
		return RunnerPolicy{}, errors.New("could not encode GitLab runner query")
	}
	var response struct {
		Data *struct {
			Runner *apiRunnerPolicy `json:"runner"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if _, err := c.request(ctx, http.MethodPost, c.graphqlURL, bytes.NewReader(body), &response); err != nil {
		return RunnerPolicy{}, err
	}
	if len(response.Errors) != 0 || response.Data == nil || response.Data.Runner == nil {
		return RunnerPolicy{}, errors.New("GitLab runner metadata is unavailable")
	}
	runner := response.Data.Runner
	if runner.ID != globalID || runner.Paused == nil || runner.RunUntagged == nil || runner.Tags == nil ||
		(runner.AccessLevel != "NOT_PROTECTED" && runner.AccessLevel != "REF_PROTECTED") {
		return RunnerPolicy{}, errors.New("GitLab runner metadata is incomplete")
	}
	for _, tag := range runner.Tags {
		if tag == "" {
			return RunnerPolicy{}, errors.New("GitLab runner metadata contains an invalid tag")
		}
	}
	return RunnerPolicy{ID: id, Known: true, Paused: *runner.Paused,
		ProtectedOnly: runner.AccessLevel == "REF_PROTECTED", RunUntagged: *runner.RunUntagged,
		Tags: runner.Tags, Projects: map[int64]bool{}}, nil
}

// Policies reads runner policy and project availability without contacting a runner manager.
// Incomplete metadata stays unknown; a token-wide rate limit rejects the refresh.
func (c *Client) Policies(ctx context.Context, projects []int64, runnerIDs []int64) (map[int64]RunnerPolicy, error) {
	policies := make(map[int64]RunnerPolicy, len(runnerIDs))
	ids := make([]int64, 0, len(runnerIDs))
	for _, id := range runnerIDs {
		if _, found := policies[id]; found {
			continue
		}
		policy := RunnerPolicy{ID: id, Projects: map[int64]bool{}}
		if id <= 0 {
			policy.Error = "GitLab runner ID is invalid"
		} else {
			ids = append(ids, id)
			observed, err := c.runnerPolicy(ctx, id)
			if err != nil {
				var limit *RateLimitError
				if errors.As(err, &limit) {
					return nil, err
				}
				policy.Error = fmt.Sprintf("GitLab runner %d metadata query failed: %s", id, err)
			} else {
				policy = observed
			}
		}
		policies[id] = policy
	}
	if len(ids) == 0 {
		return policies, nil
	}
	checked := map[int64]bool{}
	for _, project := range projects {
		if checked[project] {
			continue
		}
		checked[project] = true
		available, err := c.projectRunners(ctx, project)
		if err != nil {
			var limit *RateLimitError
			if errors.As(err, &limit) {
				return nil, err
			}
			for _, id := range ids {
				policy := policies[id]
				if policy.Error == "" {
					policy.Error = fmt.Sprintf("GitLab project %d runner availability check failed: %s", project, err)
				}
				policies[id] = policy
			}
			continue
		}
		for _, id := range ids {
			policies[id].Projects[project] = available[id]
		}
	}
	return policies, nil
}

func (c *Client) projectRunners(ctx context.Context, project int64) (map[int64]bool, error) {
	if project <= 0 {
		return nil, errors.New("GitLab project ID is invalid")
	}
	batch, err := listPages[struct {
		ID int64 `json:"id"`
	}](ctx, c, fmt.Sprintf("/projects/%d/runners", project), nil)
	if err != nil {
		return nil, err
	}
	available := make(map[int64]bool, len(batch))
	for _, runner := range batch {
		if runner.ID <= 0 || available[runner.ID] {
			return nil, errors.New("GitLab project runner list contains an invalid runner")
		}
		available[runner.ID] = true
	}
	return available, nil
}
