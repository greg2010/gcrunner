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

func TestWorkflowJobStatusError(t *testing.T) {
	readErr := errors.New("read failed")
	tests := []struct {
		name         string
		statusCode   int
		body         io.Reader
		wantNotFound bool
	}{
		{
			name:         "not found response",
			statusCode:   404,
			body:         strings.NewReader("not found"),
			wantNotFound: true,
		},
		{
			name:         "not found response body read failure",
			statusCode:   404,
			body:         errorReader{err: readErr},
			wantNotFound: true,
		},
		{
			name:       "server error response body read failure",
			statusCode: 500,
			body:       errorReader{err: readErr},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := workflowJobStatusError(tt.statusCode, tt.body, 123, "owner/repo")
			if errors.Is(err, errWorkflowJobNotFound) != tt.wantNotFound {
				t.Errorf("workflowJobStatusError() not found = %t, want %t (error = %v)", errors.Is(err, errWorkflowJobNotFound), tt.wantNotFound, err)
			}
			if tt.statusCode != 404 && !errors.Is(err, readErr) {
				t.Errorf("workflowJobStatusError() error = %v, want wrapped %v", err, readErr)
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
