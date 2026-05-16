package function

import (
	"context"
	"strings"
	"testing"
)

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
