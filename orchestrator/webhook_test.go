package function

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/stretchr/testify/mock"
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
		return &idtoken.Payload{Claims: map[string]any{
			"email":          email,
			"email_verified": verified,
		}}
	}

	tests := []struct {
		name             string
		authHeader       string
		envURL           string
		envEmail         string
		validatorPayload *idtoken.Payload
		validatorErr     error
		wantErr          string
	}{
		{name: "missing Authorization header", envURL: audience, envEmail: expectedSA, wantErr: "missing or malformed Authorization header"},
		{name: "non-Bearer scheme", authHeader: "Basic Zm9vOmJhcg==", envURL: audience, envEmail: expectedSA, wantErr: "missing or malformed Authorization header"},
		{name: "audience env unset", authHeader: validToken, envEmail: expectedSA, wantErr: "CLOUD_RUN_URL not set"},
		{name: "expected-email env unset", authHeader: validToken, envURL: audience, wantErr: "CLOUD_TASKS_SA_EMAIL not set"},
		{name: "validator rejects token", authHeader: validToken, envURL: audience, envEmail: expectedSA, validatorErr: errors.New("crypto/rsa: verification error"), wantErr: "validate id token"},
		{name: "token issued for different service account", authHeader: validToken, envURL: audience, envEmail: expectedSA, validatorPayload: validatePayload(attackerSA, true), wantErr: "unexpected issuer email"},
		{name: "email_verified false", authHeader: validToken, envURL: audience, envEmail: expectedSA, validatorPayload: validatePayload(expectedSA, false), wantErr: "issuer email not verified"},
		{name: "valid token accepted", authHeader: validToken, envURL: audience, envEmail: expectedSA, validatorPayload: validatePayload(expectedSA, true)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLOUD_RUN_URL", tt.envURL)
			t.Setenv("CLOUD_TASKS_SA_EMAIL", tt.envEmail)
			validator := NewMockTaskTokenValidator(t)
			if tt.envURL != "" && tt.envEmail != "" && strings.HasPrefix(tt.authHeader, "Bearer ") {
				validator.EXPECT().Validate(mock.Anything, "header.payload.signature", audience).Return(tt.validatorPayload, tt.validatorErr)
			}

			r := httptest.NewRequest(http.MethodPost, "/task/queued", nil)
			if tt.authHeader != "" {
				r.Header.Set("Authorization", tt.authHeader)
			}
			err := verifyTaskRequest(context.Background(), r, validator)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestHandleTaskRejectsForgedHeader(t *testing.T) {
	t.Setenv("CLOUD_RUN_URL", "https://gcrunner-webhook-1234.us-central1.run.app")
	t.Setenv("CLOUD_TASKS_SA_EMAIL", "gcrunner-tasks@my-project.iam.gserviceaccount.com")
	validator := NewMockTaskTokenValidator(t)
	handler := newTaskHandler(validator, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/task/queued", strings.NewReader(`{"action":"queued"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CloudTasks-TaskName", "forged-by-anyone")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleCompletedDeleteTarget(t *testing.T) {
	tests := []struct {
		name       string
		runnerName string
		jobID      int64
		runID      int64
		labels     []string
		wantName   string
		wantZone   string
		wantDelete bool
	}{
		{name: "runner_name present is used verbatim", runnerName: "gcrunner-100-200", jobID: 999, runID: 888, wantName: "gcrunner-100-200", wantDelete: true},
		{name: "empty runner_name falls back to reconstructed name", jobID: 200, runID: 100, wantName: "gcrunner-100-200", wantDelete: true},
		{name: "explicit zone is passed to deletion", jobID: 200, runID: 100, labels: []string{"gcrunner=test/zone=europe-west1-b"}, wantName: "gcrunner-100-200", wantZone: "europe-west1-b", wantDelete: true},
		{name: "non-gcrunner runner_name is acknowledged", runnerName: "ubuntu-latest", jobID: 200, runID: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleter := NewMockRunnerVMDeleter(t)
			if tt.wantDelete {
				deleter.EXPECT().DeleteRunnerVM(mock.Anything, tt.wantName, mock.MatchedBy(func(labels *RunnerLabels) bool {
					return labels.Zone == tt.wantZone
				})).Return(nil)
			}
			handler := newTaskHandler(nil, nil, nil, nil, deleter)
			labels := tt.labels
			if labels == nil {
				labels = []string{"gcrunner=test"}
			}
			event := WorkflowJobEvent{
				Action:      "completed",
				WorkflowJob: WorkflowJob{ID: tt.jobID, RunID: tt.runID, RunnerName: tt.runnerName, Labels: labels},
			}

			err := handler.handleCompleted(context.Background(), event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHandleQueuedStaleJobGuard(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		statusErr  error
		wantCreate bool
	}{
		{name: "completed job is skipped", status: "completed"},
		{name: "cancelled job is skipped", status: "cancelled"},
		{name: "in progress job is skipped", status: "in_progress"},
		{name: "queued job is provisioned", status: "queued", wantCreate: true},
		{name: "empty status provisions job", status: "", wantCreate: true},
		{name: "status lookup failure provisions job", statusErr: errors.New("GitHub unavailable"), wantCreate: true},
		{name: "not found job is skipped", statusErr: errWorkflowJobNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credentialsResolver := NewMockInstallationCredentialsResolver(t)
			statusLookup := NewMockWorkflowJobStatusLookup(t)
			vmCreator := NewMockRunnerVMCreator(t)
			credentials := installationCredentials{token: "installation-token"}
			credentialsResolver.EXPECT().GetInstallationCredentials(mock.Anything, "octo-org").Return(credentials, nil)
			statusLookup.EXPECT().GetWorkflowJobStatus(mock.Anything, "octo-org", "octo-repo", int64(200), credentials).Return(tt.status, tt.statusErr)
			if tt.wantCreate {
				vmCreator.EXPECT().CreateRunnerVM(mock.Anything, mock.Anything, mock.Anything, credentials).Return(nil)
			}
			handler := newTaskHandler(nil, credentialsResolver, statusLookup, vmCreator, nil)

			err := handler.handleQueued(context.Background(), WorkflowJobEvent{
				WorkflowJob: WorkflowJob{ID: 200, Labels: []string{"gcrunner=test"}},
				Repository:  Repository{Owner: RepositoryOwner{Login: "octo-org"}, Name: "octo-repo"},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHandleTaskQueuedCreationErrors(t *testing.T) {
	tests := []struct {
		name                string
		createErr           error
		credentialsErr      error
		wantCredentialCalls int
		wantStatus          int
		labels              string
		wantKVM             bool
		skipCreate          bool
	}{
		{name: "fatal error is acknowledged", createErr: &vmCreationError{kind: insertErrorFatal, err: errors.New("permission denied")}, wantStatus: http.StatusOK, labels: "gcrunner=test"},
		{name: "no matching machine type is acknowledged", createErr: &vmCreationError{kind: insertErrorFatal, err: errors.New("no machine type matching constraints")}, wantStatus: http.StatusOK, labels: "gcrunner=test/family=does-not-exist"},
		{name: "kvm impossible job skips failing credential lookup", credentialsErr: errors.New("credentials unavailable"), wantStatus: http.StatusOK, labels: "gcrunner=test/machine=e2-standard-4/kvm=true", skipCreate: true},
		{name: "invalid cpu is acknowledged", wantStatus: http.StatusOK, labels: "gcrunner=test/cpu=bogus", skipCreate: true},
		{name: "exact machine ignores invalid cpu", wantStatus: http.StatusOK, labels: "gcrunner=test/machine=n2d-standard-4/cpu=bogus"},
		{name: "quota error is retried", createErr: &vmCreationError{kind: insertErrorQuota, err: errors.New("quota exceeded")}, wantStatus: http.StatusInternalServerError, labels: "gcrunner=test"},
		{name: "transient error is retried", createErr: errors.New("capacity unavailable"), wantStatus: http.StatusInternalServerError, labels: "gcrunner=test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLOUD_RUN_URL", "https://gcrunner-webhook-1234.us-central1.run.app")
			t.Setenv("CLOUD_TASKS_SA_EMAIL", "gcrunner-tasks@my-project.iam.gserviceaccount.com")
			validator := NewMockTaskTokenValidator(t)
			credentialsResolver := NewMockInstallationCredentialsResolver(t)
			statusLookup := NewMockWorkflowJobStatusLookup(t)
			vmCreator := NewMockRunnerVMCreator(t)
			credentials := installationCredentials{token: "installation-token"}
			validator.EXPECT().Validate(mock.Anything, "header.payload.signature", "https://gcrunner-webhook-1234.us-central1.run.app").Return(&idtoken.Payload{Claims: map[string]any{
				"email":          "gcrunner-tasks@my-project.iam.gserviceaccount.com",
				"email_verified": true,
			}}, nil)
			credentialCalls := 0
			if tt.credentialsErr != nil {
				credentialsResolver.EXPECT().GetInstallationCredentials(mock.Anything, "octo-org").Run(func(context.Context, string) {
					credentialCalls++
				}).Return(credentials, tt.credentialsErr).Maybe()
			} else if !tt.skipCreate {
				credentialsResolver.EXPECT().GetInstallationCredentials(mock.Anything, "octo-org").Return(credentials, nil)
				statusLookup.EXPECT().GetWorkflowJobStatus(mock.Anything, "octo-org", "octo-repo", int64(200), credentials).Return("queued", nil)
			}
			if !tt.skipCreate {
				vmCreator.EXPECT().CreateRunnerVM(mock.Anything, mock.Anything, mock.Anything, credentials).Run(func(_ context.Context, _ WorkflowJobEvent, labels *RunnerLabels, _ installationCredentials) {
					if labels.KVM != tt.wantKVM {
						t.Errorf("KVM label = %t, want %t", labels.KVM, tt.wantKVM)
					}
				}).Return(tt.createErr)
			}
			handler := newTaskHandler(validator, credentialsResolver, statusLookup, vmCreator, nil)
			req := httptest.NewRequest(http.MethodPost, "/task/queued", strings.NewReader(fmt.Sprintf(`{"action":"queued","workflow_job":{"id":200,"labels":[%q]},"repository":{"owner":{"login":"octo-org"},"name":"octo-repo"}}`, tt.labels)))
			req.Header.Set("Authorization", "Bearer header.payload.signature")
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.credentialsErr != nil && credentialCalls != tt.wantCredentialCalls {
				t.Errorf("credential calls = %d, want %d", credentialCalls, tt.wantCredentialCalls)
			}
		})
	}
}

func TestHandleCompletedNonGcrunnerJob(t *testing.T) {
	deleter := NewMockRunnerVMDeleter(t)
	handler := newTaskHandler(nil, nil, nil, nil, deleter)
	event := WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID:         200,
			RunID:      100,
			RunnerName: "gcrunner-100-200",
			Labels:     []string{"self-hosted", "linux"},
		},
	}

	if err := handler.handleCompleted(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSecretManagerClientInitializerCapturesConfiguredProject(t *testing.T) {
	tests := []struct {
		name               string
		gcpProject         string
		googleCloudProject string
		wantProject        string
	}{
		{name: "GCP_PROJECT is used", gcpProject: "gcp-project", wantProject: "gcp-project"},
		{name: "GOOGLE_CLOUD_PROJECT is used as fallback", googleCloudProject: "google-cloud-project", wantProject: "google-cloud-project"},
		{name: "GCP_PROJECT takes precedence", gcpProject: "gcp-project", googleCloudProject: "google-cloud-project", wantProject: "gcp-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GCP_PROJECT", tt.gcpProject)
			t.Setenv("GOOGLE_CLOUD_PROJECT", tt.googleCloudProject)
			factory := NewMockSecretManagerClientFactory(t)
			client := &secretmanager.Client{}
			factory.EXPECT().NewClient(mock.Anything).Return(client, nil).Once()
			initializer := newSecretManagerClientInitializer(factory)

			got, err := initializer.get(context.Background())
			if err != nil {
				t.Fatalf("get client: %v", err)
			}
			if got != client {
				t.Errorf("client = %p, want %p", got, client)
			}
			if initializer.project != tt.wantProject {
				t.Errorf("project = %q, want %q", initializer.project, tt.wantProject)
			}
		})
	}
}

func TestHandleTaskCompletedForeignRunnerName(t *testing.T) {
	tests := []struct {
		name       string
		runnerName string
		wantStatus int
	}{
		{name: "foreign runner name is acknowledged", runnerName: "ubuntu-latest", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLOUD_RUN_URL", "https://gcrunner-webhook-1234.us-central1.run.app")
			t.Setenv("CLOUD_TASKS_SA_EMAIL", "gcrunner-tasks@my-project.iam.gserviceaccount.com")
			validator := NewMockTaskTokenValidator(t)
			validator.EXPECT().Validate(mock.Anything, "header.payload.signature", "https://gcrunner-webhook-1234.us-central1.run.app").Return(&idtoken.Payload{Claims: map[string]any{
				"email":          "gcrunner-tasks@my-project.iam.gserviceaccount.com",
				"email_verified": true,
			}}, nil)
			deleter := NewMockRunnerVMDeleter(t)
			handler := newTaskHandler(validator, nil, nil, nil, deleter)
			req := httptest.NewRequest(http.MethodPost, "/task/completed", strings.NewReader(fmt.Sprintf(`{"action":"completed","workflow_job":{"id":200,"run_id":100,"runner_name":%q,"labels":["gcrunner=test"]}}`, tt.runnerName)))
			req.Header.Set("Authorization", "Bearer header.payload.signature")
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}
