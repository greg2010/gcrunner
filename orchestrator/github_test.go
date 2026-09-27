package function

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestWorkflowJobNotFoundError(t *testing.T) {
	tests := []struct {
		name     string
		body     io.Reader
		wantText string
	}{
		{
			name:     "response body",
			body:     strings.NewReader("not found"),
			wantText: "workflow job not found: GitHub returned 404: not found",
		},
		{
			name:     "response body read failure",
			body:     errorReader{err: errors.New("read failed")},
			wantText: "workflow job not found: GitHub returned 404",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := workflowJobNotFoundError(tt.body, 123, "owner/repo")
			if !errors.Is(err, errWorkflowJobNotFound) {
				t.Errorf("workflowJobNotFoundError() = %v, want wrapped %v", err, errWorkflowJobNotFound)
			}
			if err.Error() != tt.wantText {
				t.Errorf("workflowJobNotFoundError() text = %q, want %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestGitHubAPIStatusError(t *testing.T) {
	readErr := errors.New("read failed")
	tests := []struct {
		name        string
		body        io.Reader
		wantStatus  int
		wantText    string
		wantReadErr bool
	}{
		{
			name:       "response body",
			body:       strings.NewReader(`{"message":"boom"}`),
			wantStatus: http.StatusInternalServerError,
			wantText:   `GitHub returned 500: {"message":"boom"}`,
		},
		{
			name:        "response body read failure",
			body:        errorReader{err: readErr},
			wantText:    "read GitHub 500 response: read failed",
			wantReadErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := githubAPI{}.statusError(&http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(tt.body)})
			if tt.wantReadErr {
				if !errors.Is(err, readErr) {
					t.Errorf("statusError() = %v, want wrapped %v", err, readErr)
				}
			} else {
				requireGitHubStatusError(t, err, tt.wantStatus)
			}
			if err.Error() != tt.wantText {
				t.Errorf("statusError() text = %q, want %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestGitHubAPIGetWorkflowJobStatus(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		responseBody   string
		wantStatus     string
		wantNotFound   bool
		wantGenericErr bool
	}{
		{name: "queued job", statusCode: http.StatusOK, responseBody: `{"status":"queued"}`, wantStatus: "queued"},
		{name: "completed job", statusCode: http.StatusOK, responseBody: `{"status":"completed"}`, wantStatus: "completed"},
		{name: "empty response object", statusCode: http.StatusOK, responseBody: `{}`, wantGenericErr: true},
		{name: "null status", statusCode: http.StatusOK, responseBody: `{"status":null}`, wantGenericErr: true},
		{name: "empty status string", statusCode: http.StatusOK, responseBody: `{"status":""}`, wantGenericErr: true},
		{name: "not found job", statusCode: http.StatusNotFound, responseBody: `{"message":"Not Found"}`, wantNotFound: true},
		{name: "server error", statusCode: http.StatusInternalServerError, responseBody: `{"message":"Internal Server Error"}`, wantGenericErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want %s", r.Method, http.MethodGet)
				}
				if r.URL.Path != "/repos/octo-org/octo-repo/actions/jobs/123" {
					t.Errorf("path = %s, want %s", r.URL.Path, "/repos/octo-org/octo-repo/actions/jobs/123")
				}
				if authorization := r.Header.Get("Authorization"); authorization != "Bearer installation-token" {
					t.Errorf("Authorization = %q, want %q", authorization, "Bearer installation-token")
				}
				w.WriteHeader(tt.statusCode)
				if _, err := w.Write([]byte(tt.responseBody)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer server.Close()

			client := githubAPI{baseURL: server.URL, client: server.Client()}
			status, err := client.getWorkflowJobStatus(context.Background(), "octo-org", "octo-repo", 123, installationCredentials{token: "installation-token"})
			if tt.wantNotFound {
				if !errors.Is(err, errWorkflowJobNotFound) {
					t.Errorf("error = %v, want workflow job not found", err)
				}
				return
			}
			if tt.wantGenericErr {
				if err == nil {
					t.Error("error = nil, want generic error")
				}
				if errors.Is(err, errWorkflowJobNotFound) {
					t.Errorf("error = %v, must not be workflow job not found", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("getWorkflowJobStatus() error = %v", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
		})
	}
}

func TestGitHubAPIGenerateJITConfig(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		responseBody string
		want         jitRunner
		wantStatus   int
		wantErrText  string
	}{
		{
			name:         "created runner",
			statusCode:   http.StatusCreated,
			responseBody: `{"runner":{"id":42,"name":"gcrunner-1-2"},"encoded_jit_config":"abc"}`,
			want:         jitRunner{ID: 42, EncodedConfig: "abc"},
		},
		{
			name:         "created runner without id",
			statusCode:   http.StatusCreated,
			responseBody: `{"runner":{"name":"gcrunner-1-2"},"encoded_jit_config":"abc"}`,
			wantErrText:  "no runner id",
		},
		{
			name:         "name already registered",
			statusCode:   http.StatusConflict,
			responseBody: `{"message":"Already exists"}`,
			wantStatus:   http.StatusConflict,
			wantErrText:  `GitHub returned 409: {"message":"Already exists"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Method+" "+r.URL.Path, "POST /repos/o/r/actions/runners/generate-jitconfig"; got != want {
					t.Errorf("request = %q, want %q", got, want)
				}
				if authorization := r.Header.Get("Authorization"); authorization != "Bearer token" {
					t.Errorf("Authorization = %q, want %q", authorization, "Bearer token")
				}
				if accept := r.Header.Get("Accept"); accept != "application/vnd.github+json" {
					t.Errorf("Accept = %q, want %q", accept, "application/vnd.github+json")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
				}
				if want := `{"name":"gcrunner-1-2","runner_group_id":1,"labels":["gcrunner"],"work_folder":"_work"}`; string(body) != want {
					t.Errorf("request body = %s, want %s", body, want)
				}
				w.WriteHeader(tt.statusCode)
				if _, err := w.Write([]byte(tt.responseBody)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer server.Close()

			client := githubAPI{baseURL: server.URL, client: server.Client()}
			got, err := client.generateJITConfig(context.Background(), "o", "r", "gcrunner-1-2", []string{"gcrunner"}, installationCredentials{token: "token"})
			if tt.wantStatus != 0 {
				requireGitHubStatusError(t, err, tt.wantStatus)
				if err.Error() != tt.wantErrText {
					t.Errorf("error = %q, want %q", err.Error(), tt.wantErrText)
				}
				return
			}
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("generateJITConfig() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("generateJITConfig() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("generateJITConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGitHubAPIFindRunnerByName(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		responseBody string
		want         *registeredRunner
		wantStatus   int
	}{
		{
			name:         "matching runner",
			statusCode:   http.StatusOK,
			responseBody: `{"total_count":1,"runners":[{"id":42,"name":"gcrunner-1-2","status":"offline","busy":false}]}`,
			want:         &registeredRunner{ID: 42, Name: "gcrunner-1-2", Status: "offline"},
		},
		{
			name:         "empty list",
			statusCode:   http.StatusOK,
			responseBody: `{"total_count":0,"runners":[]}`,
		},
		{
			name:         "only another runner",
			statusCode:   http.StatusOK,
			responseBody: `{"total_count":1,"runners":[{"id":7,"name":"gcrunner-1-20","status":"online","busy":true}]}`,
		},
		{
			name:         "server error",
			statusCode:   http.StatusInternalServerError,
			responseBody: `{"message":"Internal Server Error"}`,
			wantStatus:   http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery, "GET /repos/o/r/actions/runners?name=gcrunner-1-2"; got != want {
					t.Errorf("request = %q, want %q", got, want)
				}
				if authorization := r.Header.Get("Authorization"); authorization != "Bearer token" {
					t.Errorf("Authorization = %q, want %q", authorization, "Bearer token")
				}
				w.WriteHeader(tt.statusCode)
				if _, err := w.Write([]byte(tt.responseBody)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer server.Close()

			client := githubAPI{baseURL: server.URL, client: server.Client()}
			got, err := client.findRunnerByName(context.Background(), "o", "r", "gcrunner-1-2", installationCredentials{token: "token"})
			if tt.wantStatus != 0 {
				requireGitHubStatusError(t, err, tt.wantStatus)
				return
			}
			if err != nil {
				t.Fatalf("findRunnerByName() error = %v", err)
			}
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("findRunnerByName() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGitHubAPIDeleteRunner(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantStatus int
	}{
		{name: "deleted", statusCode: http.StatusNoContent},
		{name: "already gone", statusCode: http.StatusNotFound},
		{name: "server error", statusCode: http.StatusInternalServerError, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Method+" "+r.URL.Path, "DELETE /repos/o/r/actions/runners/42"; got != want {
					t.Errorf("request = %q, want %q", got, want)
				}
				if authorization := r.Header.Get("Authorization"); authorization != "Bearer token" {
					t.Errorf("Authorization = %q, want %q", authorization, "Bearer token")
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			client := githubAPI{baseURL: server.URL, client: server.Client()}
			err := client.deleteRunner(context.Background(), "o", "r", 42, installationCredentials{token: "token"})
			if tt.wantStatus != 0 {
				requireGitHubStatusError(t, err, tt.wantStatus)
				return
			}
			if err != nil {
				t.Errorf("deleteRunner() error = %v", err)
			}
		})
	}
}

func requireGitHubStatusError(t *testing.T, err error, status int) {
	t.Helper()
	var ghErr *githubStatusError
	if !errors.As(err, &ghErr) {
		t.Fatalf("error = %v, want *githubStatusError", err)
	}
	if ghErr.Status != status {
		t.Errorf("status = %d, want %d", ghErr.Status, status)
	}
}
