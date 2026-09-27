package function

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type githubAPI struct {
	baseURL string
	client  *http.Client
}

var githubAPIClient = githubAPI{
	baseURL: "https://api.github.com",
	client:  &http.Client{Timeout: 30 * time.Second},
}

var errWorkflowJobNotFound = errors.New("workflow job not found")

type installationCredentials struct {
	token string
}

func (c githubAPI) getWorkflowJobStatus(ctx context.Context, owner, repo string, jobID int64, credentials installationCredentials) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", c.baseURL, owner, repo, jobID)
	req, err := c.newRequest(ctx, http.MethodGet, url, nil, credentials)
	if err != nil {
		return "", fmt.Errorf("create workflow job request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get workflow job: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN workflow_job_response_close_failed job_id=%d repo=%s error=%v", jobID, owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return "", workflowJobNotFoundError(resp.Body, jobID, owner+"/"+repo)
	}
	if resp.StatusCode != http.StatusOK {
		return "", c.statusError(resp)
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode workflow job response: %w", err)
	}
	if result.Status == "" {
		return "", fmt.Errorf("workflow job response missing status")
	}
	return result.Status, nil
}

func workflowJobNotFoundError(body io.Reader, jobID int64, repo string) error {
	responseBody, err := io.ReadAll(body)
	if err != nil {
		log.Printf("WARN workflow_job_error_response_read_failed job_id=%d repo=%s error=%v", jobID, repo, err)
		return fmt.Errorf("%w: GitHub returned %d", errWorkflowJobNotFound, http.StatusNotFound)
	}
	return fmt.Errorf("%w: GitHub returned %d: %s", errWorkflowJobNotFound, http.StatusNotFound, string(responseBody))
}

// githubStatusError is a GitHub REST response with a status the caller did not accept.
type githubStatusError struct {
	Status int
	Body   string
}

func (e *githubStatusError) Error() string {
	return fmt.Sprintf("GitHub returned %d: %s", e.Status, e.Body)
}

// jitRunner is the runner a generate-jitconfig call registered.
type jitRunner struct {
	ID            int64
	EncodedConfig string
}

// registeredRunner is one entry of the repository's self-hosted runner list.
type registeredRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

func (c githubAPI) statusError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read GitHub %d response: %w", resp.StatusCode, err)
	}
	return &githubStatusError{Status: resp.StatusCode, Body: string(body)}
}

func (c githubAPI) newRequest(ctx context.Context, method, url string, body io.Reader, credentials installationCredentials) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	return req, nil
}

func (c githubAPI) generateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string, credentials installationCredentials) (jitRunner, error) {
	body := struct {
		Name          string   `json:"name"`
		RunnerGroupID int      `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}{
		Name:          runnerName,
		RunnerGroupID: 1,
		Labels:        labels,
		WorkFolder:    "_work",
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return jitRunner{}, fmt.Errorf("marshal request body: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners/generate-jitconfig", c.baseURL, owner, repo)
	req, err := c.newRequest(ctx, http.MethodPost, url, strings.NewReader(string(bodyJSON)), credentials)
	if err != nil {
		return jitRunner{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return jitRunner{}, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=generate_jit_config repo=%s error=%v", owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		return jitRunner{}, c.statusError(resp)
	}

	var result struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return jitRunner{}, err
	}
	if result.EncodedJITConfig == "" {
		return jitRunner{}, fmt.Errorf("generate JIT config response missing encoded_jit_config")
	}
	if result.Runner.ID == 0 {
		return jitRunner{}, fmt.Errorf("generate-jitconfig response has no runner id")
	}
	return jitRunner{ID: result.Runner.ID, EncodedConfig: result.EncodedJITConfig}, nil
}

// findRunnerByName returns the repository runner registered under name, or nil when there is none.
func (c githubAPI) findRunnerByName(ctx context.Context, owner, repo, name string, credentials installationCredentials) (*registeredRunner, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners?name=%s", c.baseURL, owner, repo, neturl.QueryEscape(name))
	req, err := c.newRequest(ctx, http.MethodGet, url, nil, credentials)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=find_runner_by_name repo=%s error=%v", owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(resp)
	}

	var result struct {
		Runners []registeredRunner `json:"runners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode list runners response: %w", err)
	}
	for _, runner := range result.Runners {
		if runner.Name == name {
			return &runner, nil
		}
	}
	return nil, nil
}

// deleteRunner removes a runner registration; a runner that is already gone is not an error.
func (c githubAPI) deleteRunner(ctx context.Context, owner, repo string, runnerID int64, credentials installationCredentials) error {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners/%d", c.baseURL, owner, repo, runnerID)
	req, err := c.newRequest(ctx, http.MethodDelete, url, nil, credentials)
	if err != nil {
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=delete_runner repo=%s error=%v", owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return c.statusError(resp)
}

func (c githubAPI) getInstallationCredentials(ctx context.Context, owner string) (installationCredentials, error) {
	installationToken, err := c.getInstallationToken(ctx, owner)
	if err != nil {
		return installationCredentials{}, fmt.Errorf("get installation token: %w", err)
	}
	return installationCredentials{token: installationToken}, nil
}

func (c githubAPI) getInstallationToken(ctx context.Context, owner string) (string, error) {
	appJWT, err := generateAppJWT(ctx)
	if err != nil {
		return "", fmt.Errorf("generate JWT: %w", err)
	}

	installationID, err := c.getInstallationID(ctx, appJWT, owner)
	if err != nil {
		return "", fmt.Errorf("get installation ID: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=get_installation_token owner=%s error=%v", owner, err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		return "", c.statusError(resp)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Token, nil
}

func (c githubAPI) getInstallationID(ctx context.Context, appJWT, owner string) (int64, error) {
	url := fmt.Sprintf("%s/users/%s/installation", c.baseURL, owner)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=get_installation_id owner=%s error=%v", owner, err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, c.statusError(resp)
	}

	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return result.ID, nil
}

func generateAppJWT(ctx context.Context) (string, error) {
	appIDStr, err := getSecret(ctx, "gcrunner-app-id")
	if err != nil {
		return "", fmt.Errorf("get app ID: %w", err)
	}

	keyPEM, err := getSecret(ctx, "gcrunner-private-key")
	if err != nil {
		return "", fmt.Errorf("get private key: %w", err)
	}

	appID, err := strconv.ParseInt(strings.TrimSpace(appIDStr), 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse app ID: %w", err)
	}

	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	return signJWT(appID, key)
}

func signJWT(appID int64, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		Issuer:    strconv.FormatInt(appID, 10),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}
