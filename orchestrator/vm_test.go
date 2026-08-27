package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/googleapi"
)

func TestParseDiskSize(t *testing.T) {
	tests := []struct {
		name    string
		disk    string
		want    int64
		wantErr bool
	}{
		{name: "gigabytes suffix", disk: "75gb", want: 75},
		{name: "no suffix", disk: "50", want: 50},
		{name: "small disk uses default", disk: "5gb", want: 50},
		{name: "zero disk is invalid", disk: "0gb", wantErr: true},
		{name: "negative disk is invalid", disk: "-1gb", wantErr: true},
		{name: "non numeric", disk: "bogus", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDiskSize(tt.disk)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDiskSize(%q) error = %v, wantErr = %t", tt.disk, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseDiskSize(%q) = %d, want %d", tt.disk, got, tt.want)
			}
		})
	}
}

func TestClassifyVMInsertError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want insertErrorKind
	}{
		{name: "nil error", err: nil, want: insertErrorRetryable},
		{name: "Google API not found", err: &googleapi.Error{Code: 404, Errors: []googleapi.ErrorItem{{Reason: "notFound"}}}, want: insertErrorFatal},
		{name: "Google API quota exceeded", err: &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded"}}}, want: insertErrorQuota},
		{name: "Google API permission denied", err: &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}, want: insertErrorFatal},
		{name: "Google API permission denied without details", err: &googleapi.Error{Code: 403, Message: "Permission denied"}, want: insertErrorRetryable},
		{name: "Google API empty forbidden without details", err: &googleapi.Error{Code: 403}, want: insertErrorRetryable},
		{name: "Google API bad request without details", err: &googleapi.Error{Code: 400, Message: "Bad Request"}, want: insertErrorFatal},
		{name: "Google API unauthorized without details", err: &googleapi.Error{Code: 401, Message: "Unauthorized"}, want: insertErrorFatal},
		{name: "Google API conflict without details", err: &googleapi.Error{Code: 409}, want: insertErrorAlreadyExists},
		{name: "Google API conflict with quota reason", err: &googleapi.Error{Code: 409, Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded"}}}, want: insertErrorQuota},
		{name: "operation quota exceeded", err: errors.New("QUOTA_EXCEEDED: insufficient regional quota"), want: insertErrorQuota},
		{name: "operation resource not found", err: errors.New("RESOURCE_NOT_FOUND: machine type not available"), want: insertErrorFatal},
		{name: "already exists", err: errors.New("The resource already exists"), want: insertErrorAlreadyExists},
		{name: "already exists camel", err: errors.New("alreadyExists"), want: insertErrorAlreadyExists},
		{name: "permission denied", err: errors.New("Permission denied"), want: insertErrorFatal},
		{name: "invalid configuration", err: errors.New("INVALID_ARGUMENT: invalid machine type"), want: insertErrorFatal},
		{name: "zone exhausted", err: errors.New("ZONE_RESOURCE_POOL_EXHAUSTED"), want: insertErrorRetryable},
		{name: "generic transient error", err: errors.New("some transient error"), want: insertErrorRetryable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyInsertError(tt.err); got != tt.want {
				t.Errorf("classifyInsertError() = %d, want %d", got, tt.want)
			}
		})
	}
}

func seedMachineTypeCache(t *testing.T, zone string, types []*MachineTypeInfo) {
	t.Helper()
	machineTypeCache.mu.Lock()
	previous, hadPrevious := machineTypeCache.types[zone]
	machineTypeCache.types[zone] = machineTypeCacheEntry{types: types, fetchedAt: machineTypeCache.nowFunc()}
	machineTypeCache.mu.Unlock()
	t.Cleanup(func() {
		machineTypeCache.mu.Lock()
		defer machineTypeCache.mu.Unlock()
		if hadPrevious {
			machineTypeCache.types[zone] = previous
			return
		}
		delete(machineTypeCache.types, zone)
	})
}

func TestValidateRunnerLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  *RunnerLabels
		wantErr bool
	}{
		{name: "valid multi family", labels: &RunnerLabels{MachineMode: "family", Family: "n2d+c3", CPU: "4", RAM: "16", Disk: "75gb"}},
		{name: "empty middle family component", labels: &RunnerLabels{MachineMode: "family", Family: "e2++t2d", CPU: "4", RAM: "16", Disk: "75gb"}, wantErr: true},
		{name: "leading empty family component", labels: &RunnerLabels{MachineMode: "family", Family: "+n2d", CPU: "4", RAM: "16", Disk: "75gb"}, wantErr: true},
		{name: "trailing empty family component", labels: &RunnerLabels{MachineMode: "family", Family: "n2d+", CPU: "4", RAM: "16", Disk: "75gb"}, wantErr: true},
		{name: "family of only separators", labels: &RunnerLabels{MachineMode: "family", Family: "+", CPU: "4", RAM: "16", Disk: "75gb"}, wantErr: true},
		{name: "valid multi zone", labels: &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-2", Disk: "75gb", Zone: "us-central1-a+us-central1-b"}},
		{name: "trailing empty zone component", labels: &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-2", Disk: "75gb", Zone: "us-central1-a+"}, wantErr: true},
		{name: "leading empty zone component", labels: &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-2", Disk: "75gb", Zone: "+us-central1-a"}, wantErr: true},
		{name: "invalid cpu range", labels: &RunnerLabels{MachineMode: "auto", CPU: "bogus", Disk: "75gb"}, wantErr: true},
		{name: "exact mode skips cpu validation", labels: &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-2", CPU: "bogus", Disk: "75gb"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRunnerLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRunnerLabels() error = %v, wantErr = %t", err, tt.wantErr)
			}
			if tt.wantErr && !isFatalVMCreationError(err) {
				t.Errorf("validateRunnerLabels() error = %v, want fatal vmCreationError", err)
			}
		})
	}
}

func TestCreateRunnerVMGeneratesJITAfterPreflight(t *testing.T) {
	tests := []struct {
		name            string
		labels          *RunnerLabels
		jitResponseBody string
		seedZone        string
		seedTypes       []*MachineTypeInfo
		wantErr         bool
		wantJITCalls    int
		wantVMCalls     int
	}{
		{
			name:    "invalid labels do not generate JIT config",
			labels:  &RunnerLabels{MachineMode: "family", Family: "n2d", CPU: "bogus", RAM: "4", Disk: "75gb"},
			wantErr: true,
		},
		{
			name:    "KVM unsupported exact machine does not generate JIT config",
			labels:  &RunnerLabels{MachineMode: "exact", Machine: "e2-standard-4", Disk: "75gb", KVM: true},
			wantErr: true,
		},
		{
			name:    "KVM unsupported family does not generate JIT config",
			labels:  &RunnerLabels{MachineMode: "family", Family: "e2", CPU: "4", RAM: "16", Disk: "75gb", KVM: true},
			wantErr: true,
		},
		{
			name:    "malformed family list does not generate JIT config",
			labels:  &RunnerLabels{MachineMode: "family", Family: "e2++t2d", CPU: "4", RAM: "16", Disk: "75gb", KVM: true},
			wantErr: true,
		},
		{
			name:    "malformed zone list does not generate JIT config",
			labels:  &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-4", Disk: "75gb", Zone: "+us-central1-a"},
			wantErr: true,
		},
		{
			name:         "VM creation generates one JIT config",
			labels:       &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-4", Disk: "75gb", Zone: "us-central1-a"},
			wantJITCalls: 1,
			wantVMCalls:  1,
		},
		{
			name:     "valid multi-family creates VM",
			labels:   &RunnerLabels{MachineMode: "family", Family: "e2+n2d", CPU: "4", RAM: "16", Disk: "75gb", Zone: "jit-test-zone"},
			seedZone: "jit-test-zone",
			seedTypes: []*MachineTypeInfo{
				{Name: "n2d-standard-4", Family: "n2d", Category: "standard", VCPUs: 4, MemoryMB: 16384},
			},
			wantJITCalls: 1,
			wantVMCalls:  1,
		},
		{
			name:            "empty JIT config response fails before VM creation",
			labels:          &RunnerLabels{MachineMode: "exact", Machine: "n2d-standard-4", Disk: "75gb", Zone: "us-central1-a"},
			jitResponseBody: `{}`,
			wantErr:         true,
			wantJITCalls:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.seedZone != "" {
				seedMachineTypeCache(t, tt.seedZone, tt.seedTypes)
			}
			jitCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jitCalls++
				if r.Method != http.MethodPost {
					t.Errorf("JIT request method = %s, want %s", r.Method, http.MethodPost)
				}
				if want := "/repos/octo-org/octo-repo/actions/runners/generate-jitconfig"; r.URL.Path != want {
					t.Errorf("JIT request path = %s, want %s", r.URL.Path, want)
				}
				if authorization := r.Header.Get("Authorization"); authorization != "Bearer token" {
					t.Errorf("Authorization = %q, want %q", authorization, "Bearer token")
				}
				var jitRequest struct {
					Name          string   `json:"name"`
					RunnerGroupID int      `json:"runner_group_id"`
					Labels        []string `json:"labels"`
					WorkFolder    string   `json:"work_folder"`
				}
				if err := json.NewDecoder(r.Body).Decode(&jitRequest); err != nil {
					t.Errorf("decode JIT request: %v", err)
				}
				if jitRequest.Name != "gcrunner-100-200" {
					t.Errorf("JIT runner name = %q, want %q", jitRequest.Name, "gcrunner-100-200")
				}
				if jitRequest.RunnerGroupID != 1 {
					t.Errorf("JIT runner group = %d, want 1", jitRequest.RunnerGroupID)
				}
				if fmt.Sprint(jitRequest.Labels) != fmt.Sprint([]string{"gcrunner=test"}) {
					t.Errorf("JIT labels = %v, want %v", jitRequest.Labels, []string{"gcrunner=test"})
				}
				if jitRequest.WorkFolder != "_work" {
					t.Errorf("JIT work folder = %q, want %q", jitRequest.WorkFolder, "_work")
				}
				w.WriteHeader(http.StatusCreated)
				responseBody := tt.jitResponseBody
				if responseBody == "" {
					responseBody = `{"encoded_jit_config":"jit-config"}`
				}
				if _, err := w.Write([]byte(responseBody)); err != nil {
					t.Errorf("write JIT response: %v", err)
				}
			}))
			defer server.Close()

			originalGitHubAPIClient := githubAPIClient
			githubAPIClient = githubAPI{baseURL: server.URL, client: server.Client()}
			t.Cleanup(func() {
				githubAPIClient = originalGitHubAPIClient
			})

			vmCalls := 0
			originalCreateInstance := createInstanceForRunner
			createInstanceForRunner = func(_ context.Context, _, _, _ string, _ *RunnerLabels, _, _ string) error {
				vmCalls++
				return nil
			}
			t.Cleanup(func() {
				createInstanceForRunner = originalCreateInstance
			})

			err := createRunnerVM(context.Background(), WorkflowJobEvent{
				WorkflowJob: WorkflowJob{ID: 200, RunID: 100, Labels: []string{"gcrunner=test"}},
				Repository:  Repository{Owner: RepositoryOwner{Login: "octo-org"}, Name: "octo-repo"},
			}, tt.labels, installationCredentials{token: "token"})

			if (err != nil) != tt.wantErr {
				t.Fatalf("createRunnerVM() error = %v, wantErr = %t", err, tt.wantErr)
			}
			if jitCalls != tt.wantJITCalls {
				t.Errorf("JIT config calls = %d, want %d", jitCalls, tt.wantJITCalls)
			}
			if vmCalls != tt.wantVMCalls {
				t.Errorf("VM creation calls = %d, want %d", vmCalls, tt.wantVMCalls)
			}
		})
	}
}

func TestCreateVMInZones(t *testing.T) {
	tests := []struct {
		name         string
		errorsByZone map[string]error
		wantCalls    []string
		wantFatal    bool
		wantErr      bool
	}{
		{
			name: "not found in first zone succeeds in second",
			errorsByZone: map[string]error{
				"zone-a": errors.New("RESOURCE_NOT_FOUND: machine type not available"),
			},
			wantCalls: []string{"zone-a", "zone-b"},
		},
		{
			name: "not found in every zone is fatal",
			errorsByZone: map[string]error{
				"zone-a": errors.New("RESOURCE_NOT_FOUND: machine type not available"),
				"zone-b": errors.New("RESOURCE_NOT_FOUND: machine type not available"),
			},
			wantCalls: []string{"zone-a", "zone-b"},
			wantFatal: true,
			wantErr:   true,
		},
		{
			name: "not found and pool exhausted is retryable",
			errorsByZone: map[string]error{
				"zone-a": errors.New("RESOURCE_NOT_FOUND: machine type not available"),
				"zone-b": errors.New("resource pool exhausted"),
			},
			wantCalls: []string{"zone-a", "zone-b"},
			wantErr:   true,
		},
		{
			name: "typed fatal error stops zone retries",
			errorsByZone: map[string]error{
				"zone-a": &vmCreationError{kind: insertErrorFatal, err: errors.New("nested virtualization unsupported")},
			},
			wantCalls: []string{"zone-a"},
			wantFatal: true,
			wantErr:   true,
		},
		{
			name: "no match in first zone succeeds in second",
			errorsByZone: map[string]error{
				"zone-a": &vmCreationError{kind: insertErrorNoMachineType, err: errors.New("no machine type matching constraints")},
			},
			wantCalls: []string{"zone-a", "zone-b"},
		},
		{
			name: "no match in every zone is fatal",
			errorsByZone: map[string]error{
				"zone-a": &vmCreationError{kind: insertErrorNoMachineType, err: errors.New("no machine type matching constraints")},
				"zone-b": &vmCreationError{kind: insertErrorNoMachineType, err: errors.New("no machine type matching constraints")},
			},
			wantCalls: []string{"zone-a", "zone-b"},
			wantFatal: true,
			wantErr:   true,
		},
		{
			name: "no match and pool exhausted is retryable",
			errorsByZone: map[string]error{
				"zone-a": &vmCreationError{kind: insertErrorNoMachineType, err: errors.New("no machine type matching constraints")},
				"zone-b": errors.New("resource pool exhausted"),
			},
			wantCalls: []string{"zone-a", "zone-b"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := createVMInZones([]string{"zone-a", "zone-b"}, func(zone string) error {
				calls = append(calls, zone)
				return tt.errorsByZone[zone]
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("createVMInZones() error = %v, wantErr = %t", err, tt.wantErr)
			}
			if isFatalVMCreationError(err) != tt.wantFatal {
				t.Errorf("isFatalVMCreationError() = %t, want %t", isFatalVMCreationError(err), tt.wantFatal)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tt.wantCalls) {
				t.Errorf("zones called = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestRunnerVMZones(t *testing.T) {
	tests := []struct {
		name          string
		labels        *RunnerLabels
		listedZones   []string
		listErr       error
		wantZones     []string
		wantListCalls int
		wantErr       bool
	}{
		{
			name:      "explicit zone is honored without regional discovery",
			labels:    &RunnerLabels{Zone: "europe-west1-b"},
			wantZones: []string{"europe-west1-b"},
		},
		{
			name:          "no zone label uses regional discovery",
			labels:        &RunnerLabels{},
			listedZones:   []string{"us-central1-a", "us-central1-b"},
			wantZones:     []string{"us-central1-a", "us-central1-b"},
			wantListCalls: 1,
		},
		{
			name:          "regional discovery failure uses default zones",
			labels:        &RunnerLabels{},
			listErr:       errors.New("unavailable"),
			wantZones:     []string{"us-central1-a", "us-central1-b", "us-central1-c"},
			wantListCalls: 1,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listCalls := 0
			zones, err := runnerVMZones(context.Background(), "project", "us-central1", tt.labels, func(_ context.Context, _, _ string) ([]string, error) {
				listCalls++
				return tt.listedZones, tt.listErr
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("runnerVMZones() error = %v, wantErr = %t", err, tt.wantErr)
			}
			if fmt.Sprint(zones) != fmt.Sprint(tt.wantZones) {
				t.Errorf("runnerVMZones() = %v, want %v", zones, tt.wantZones)
			}
			if listCalls != tt.wantListCalls {
				t.Errorf("regional discovery calls = %d, want %d", listCalls, tt.wantListCalls)
			}
		})
	}
}

func TestDeleteVMInZones(t *testing.T) {
	tests := []struct {
		name         string
		zones        []string
		errorsByZone map[string]error
		wantCalls    []string
		wantErr      bool
	}{
		{
			name: "deletes in second zone after not found in first",
			errorsByZone: map[string]error{
				"zone-a": &googleapi.Error{Code: 404},
			},
			wantCalls: []string{"zone-a", "zone-b"},
		},
		{
			name:  "returns error for unavailable delete",
			zones: []string{"zone-a"},
			errorsByZone: map[string]error{
				"zone-a": &googleapi.Error{Code: 503},
			},
			wantCalls: []string{"zone-a"},
			wantErr:   true,
		},
		{
			name: "returns nil when not found in every zone",
			errorsByZone: map[string]error{
				"zone-a": &googleapi.Error{Code: 404},
				"zone-b": &googleapi.Error{Errors: []googleapi.ErrorItem{{Reason: "notFound"}}},
			},
			wantCalls: []string{"zone-a", "zone-b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			zones := tt.zones
			if zones == nil {
				zones = []string{"zone-a", "zone-b"}
			}
			err := deleteVMInZones("runner", zones, func(zone string) error {
				calls = append(calls, zone)
				return tt.errorsByZone[zone]
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("deleteVMInZones() error = %v, wantErr = %t", err, tt.wantErr)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tt.wantCalls) {
				t.Errorf("zones called = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestIsResourceNotFoundError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "Google API status not found", err: &googleapi.Error{Code: 404}, want: true},
		{name: "Google API reason not found", err: &googleapi.Error{Code: 400, Errors: []googleapi.ErrorItem{{Reason: "notFound"}}}, want: true},
		{name: "Google API message resource not found", err: &googleapi.Error{Code: 400, Message: "RESOURCE_NOT_FOUND machine type"}},
		{name: "untyped resource not found", err: errors.New("RESOURCE_NOT_FOUND machine type"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isResourceNotFoundError(tt.err); got != tt.want {
				t.Errorf("isResourceNotFoundError() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestIsFatalVMCreationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "fatal creation error", err: &vmCreationError{kind: insertErrorFatal, err: errors.New("permission denied")}, want: true},
		{name: "wrapped fatal creation error", err: fmt.Errorf("create runner: %w", &vmCreationError{kind: insertErrorFatal, err: errors.New("permission denied")}), want: true},
		{name: "quota creation error", err: &vmCreationError{kind: insertErrorQuota, err: errors.New("quota exceeded")}},
		{name: "ordinary error", err: errors.New("capacity unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFatalVMCreationError(tt.err); got != tt.want {
				t.Errorf("isFatalVMCreationError() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestValidateKVMSupported(t *testing.T) {
	tests := []struct {
		name        string
		machineType string
		wantErr     bool
	}{
		{"n2d allowed", "n2d-standard-4", false},
		{"n2 allowed", "n2-standard-2", false},
		{"n1 allowed", "n1-standard-4", false},
		{"c3 allowed", "c3-standard-8", false},
		{"c3d allowed", "c3d-standard-8", false},
		{"c2 allowed", "c2-standard-4", false},
		{"e2 rejected", "e2-standard-4", true},
		{"e2-micro rejected", "e2-micro", true},
		{"t2d rejected", "t2d-standard-2", true},
		{"t2a rejected", "t2a-standard-2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKVMSupported(tt.machineType)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateKVMSupported(%q) err = %v, wantErr = %v", tt.machineType, err, tt.wantErr)
			}
		})
	}
}
