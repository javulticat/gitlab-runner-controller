package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/httpclient"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

type RateLimitError struct{ Delay time.Duration }

func (e *RateLimitError) Error() string { return "GitLab rate limit reached" }

type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("GitLab returned HTTP %d", e.StatusCode)
}

type Client struct {
	baseURL    string
	graphqlURL string
	token      string
	http       *http.Client
	now        func() time.Time
}

func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{baseURL: baseURL + "/api/v4", graphqlURL: baseURL + "/api/graphql", token: token,
		http: httpclient.New(12 * time.Second), now: time.Now}
}

type apiJob struct {
	ID     int64    `json:"id"`
	Status string   `json:"status"`
	Tags   []string `json:"tag_list"`
	Runner *struct {
		ID int64 `json:"id"`
	} `json:"runner"`
}

func (j apiJob) stateJob(project int64, now time.Time) state.Job {
	job := state.Job{ProjectID: project, ID: j.ID, Status: j.Status, Tags: j.Tags, UpdatedAt: now}
	if j.Runner != nil {
		job.RunnerID = j.Runner.ID
	}
	return job
}

func validStatus(status string) bool {
	return slices.Contains([]string{"pending", "preparing", "running", "canceling", "canceled", "failed", "success", "skipped", "manual", "scheduled", "created", "waiting_for_resource", "waiting_for_callback"}, status)
}

func (c *Client) get(ctx context.Context, path string, target any) (http.Header, error) {
	return c.request(ctx, http.MethodGet, c.baseURL+path, nil, target)
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, target any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("invalid GitLab request")
	}
	if endpoint == c.graphqlURL {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("GitLab request failed or timed out")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{Delay: httpclient.RetryDelay(response.Header, c.now())}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	data, err := httpclient.ReadBody(response, 4<<20)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return nil, errors.New("invalid GitLab JSON response")
	}
	return response.Header, nil
}

func listPages[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "100")
	result := []T{}
	for page := 1; ; page++ {
		if page > 200 {
			return nil, errors.New("GitLab pagination exceeds 200 pages")
		}
		query.Set("page", strconv.Itoa(page))
		var batch []T
		header, err := c.get(ctx, path+"?"+query.Encode(), &batch)
		if err != nil {
			return nil, err
		}
		if batch == nil {
			return nil, errors.New("GitLab returned a null list")
		}
		result = append(result, batch...)
		if next := header.Get("X-Next-Page"); next != "" {
			number, err := strconv.Atoi(next)
			if err != nil || number != page+1 {
				return nil, errors.New("GitLab returned invalid pagination")
			}
		} else if len(batch) < 100 {
			return result, nil
		}
	}
}

// Sync discovers missed hooks and confirms known jobs absent from the active list.
// A missing confirmation is removed only after complete project discovery.
func (c *Client) Sync(ctx context.Context, projects []int64, check []state.Job) ([]state.Job, error) {
	configured := map[int64]bool{}
	for _, project := range projects {
		configured[project] = true
	}
	for _, previous := range check {
		if !configured[previous.ProjectID] || previous.ID <= 0 {
			return nil, errors.New("GitLab job confirmation is outside configured projects or has an invalid ID")
		}
	}
	jobs := map[string]state.Job{}
	for _, project := range projects {
		batch, err := listPages[apiJob](ctx, c, fmt.Sprintf("/projects/%d/jobs", project),
			url.Values{"scope[]": {"pending", "preparing", "running", "canceling"}})
		if err != nil {
			return nil, err
		}
		for _, item := range batch {
			if item.ID <= 0 || !validStatus(item.Status) {
				return nil, errors.New("GitLab returned an invalid job")
			}
			job := item.stateJob(project, c.now())
			jobs[job.Key()] = job
		}
	}
	check = slices.Clone(check)
	slices.SortFunc(check, func(a, b state.Job) int { return strings.Compare(a.Key(), b.Key()) })
	checked := map[string]bool{}
	for _, previous := range check {
		if _, found := jobs[previous.Key()]; found || checked[previous.Key()] {
			continue
		}
		checked[previous.Key()] = true
		var item apiJob
		if _, err := c.get(ctx, fmt.Sprintf("/projects/%d/jobs/%d", previous.ProjectID, previous.ID), &item); err != nil {
			var status *HTTPStatusError
			if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		if item.ID != previous.ID || !validStatus(item.Status) {
			return nil, errors.New("GitLab returned an invalid job confirmation")
		}
		jobs[previous.Key()] = item.stateJob(previous.ProjectID, c.now())
	}
	result := make([]state.Job, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, job)
	}
	return result, nil
}
