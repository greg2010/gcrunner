package function

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/idtoken"
)

func TestVerifyTaskRequest(t *testing.T) {
	const (
		audience   = "https://gcrunner-webhook-1234.us-central1.run.app"
		expectedSA = "gcrunner-tasks@my-project.iam.gserviceaccount.com"
		attackerSA = "attacker@evil.iam.gserviceaccount.com"
		validToken = "Bearer header.payload.signature"
	)

	validatePayload := func(email string, verified bool) *idtoken.Payload {
		return &idtoken.Payload{
			Audience: audience,
			Claims: map[string]any{
				"email":          email,
				"email_verified": verified,
			},
		}
	}

	tests := []struct {
		name       string
		authHeader string
		stub       func(ctx context.Context, token, aud string) (*idtoken.Payload, error)
		envURL     string
		envEmail   string
		wantErr    string
	}{
		{
			name:       "missing Authorization header",
			authHeader: "",
			envURL:     audience,
			envEmail:   expectedSA,
			wantErr:    "missing or malformed Authorization header",
		},
		{
			name:       "non-Bearer scheme",
			authHeader: "Basic Zm9vOmJhcg==",
			envURL:     audience,
			envEmail:   expectedSA,
			wantErr:    "missing or malformed Authorization header",
		},
		{
			name:       "audience env unset",
			authHeader: validToken,
			envURL:     "",
			envEmail:   expectedSA,
			wantErr:    "CLOUD_RUN_URL not set",
		},
		{
			name:       "expected-email env unset",
			authHeader: validToken,
			envURL:     audience,
			envEmail:   "",
			wantErr:    "CLOUD_TASKS_SA_EMAIL not set",
		},
		{
			name:       "validator rejects token",
			authHeader: validToken,
			envURL:     audience,
			envEmail:   expectedSA,
			stub: func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
				return nil, errors.New("crypto/rsa: verification error")
			},
			wantErr: "validate id token",
		},
		{
			name:       "token issued for different service account",
			authHeader: validToken,
			envURL:     audience,
			envEmail:   expectedSA,
			stub: func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
				return validatePayload(attackerSA, true), nil
			},
			wantErr: "unexpected issuer email",
		},
		{
			name:       "email_verified false",
			authHeader: validToken,
			envURL:     audience,
			envEmail:   expectedSA,
			stub: func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
				return validatePayload(expectedSA, false), nil
			},
			wantErr: "issuer email not verified",
		},
		{
			name:       "valid token accepted",
			authHeader: validToken,
			envURL:     audience,
			envEmail:   expectedSA,
			stub: func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
				if aud != audience {
					t.Errorf("audience = %q, want %q", aud, audience)
				}
				return validatePayload(expectedSA, true), nil
			},
			wantErr: "",
		},
	}

	original := taskTokenValidator
	t.Cleanup(func() { taskTokenValidator = original })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUD_RUN_URL", tc.envURL)
			t.Setenv("CLOUD_TASKS_SA_EMAIL", tc.envEmail)
			if tc.stub != nil {
				taskTokenValidator = tc.stub
			} else {
				taskTokenValidator = func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
					t.Fatal("validator should not be called when header check fails")
					return nil, nil
				}
			}

			r := httptest.NewRequest(http.MethodPost, "/task/queued", nil)
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			err := verifyTaskRequest(context.Background(), r)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestHandleTask_RejectsForgedHeader is the regression test for the public C1
// disclosure: a request with X-CloudTasks-TaskName set but no Authorization
// header must be rejected with 401.
func TestHandleTask_RejectsForgedHeader(t *testing.T) {
	t.Setenv("CLOUD_RUN_URL", "https://gcrunner-webhook-1234.us-central1.run.app")
	t.Setenv("CLOUD_TASKS_SA_EMAIL", "gcrunner-tasks@my-project.iam.gserviceaccount.com")

	original := taskTokenValidator
	t.Cleanup(func() { taskTokenValidator = original })
	taskTokenValidator = func(ctx context.Context, token, aud string) (*idtoken.Payload, error) {
		t.Fatal("validator should not be reached without a Bearer header")
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/task/queued", strings.NewReader(`{"action":"queued"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CloudTasks-TaskName", "forged-by-anyone")

	w := httptest.NewRecorder()
	HandleTask(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleCompleted_DeleteTarget(t *testing.T) {
	tests := []struct {
		name        string
		runnerName  string
		jobID       int64
		runID       int64
		wantName    string
		wantErr     bool
		wantErrText string
	}{
		{
			name:       "runner_name present is used verbatim",
			runnerName: "gcrunner-100-200",
			jobID:      999,
			runID:      888,
			wantName:   "gcrunner-100-200",
		},
		{
			name:       "empty runner_name falls back to reconstructed name",
			runnerName: "",
			jobID:      200,
			runID:      100,
			wantName:   "gcrunner-100-200",
		},
		{
			name:        "non-gcrunner runner_name is rejected",
			runnerName:  "ubuntu-latest",
			jobID:       200,
			runID:       100,
			wantErr:     true,
			wantErrText: "refusing to delete VM with unexpected name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			vmDeleter = func(_ context.Context, name string) error {
				got = name
				return nil
			}
			t.Cleanup(func() { vmDeleter = deleteRunnerVM })

			event := WorkflowJobEvent{
				Action: "completed",
				WorkflowJob: WorkflowJob{
					ID:         tt.jobID,
					RunID:      tt.runID,
					RunnerName: tt.runnerName,
					Labels:     []string{"gcrunner=test"},
				},
			}

			err := handleCompleted(context.Background(), event)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErrText)
				}
				if got != "" {
					t.Errorf("vmDeleter should not have been called; got name %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantName {
				t.Errorf("deleted VM = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestHandleCompleted_NonGcrunnerJob(t *testing.T) {
	called := false
	vmDeleter = func(_ context.Context, _ string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { vmDeleter = deleteRunnerVM })

	event := WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID:         200,
			RunID:      100,
			RunnerName: "gcrunner-100-200",
			Labels:     []string{"self-hosted", "linux"},
		},
	}

	if err := handleCompleted(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("vmDeleter should not be called for non-gcrunner jobs")
	}
}
