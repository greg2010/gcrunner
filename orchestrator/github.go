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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create workflow job request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+credentials.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get workflow job: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN workflow_job_response_close_failed job_id=%d repo=%s error=%v", jobID, owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", workflowJobStatusError(resp.StatusCode, resp.Body, jobID, owner+"/"+repo)
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

func workflowJobStatusError(statusCode int, body io.Reader, jobID int64, repo string) error {
	notFound := statusCode == http.StatusNotFound
	responseBody, err := io.ReadAll(body)
	if err != nil {
		if notFound {
			log.Printf("WARN workflow_job_error_response_read_failed job_id=%d repo=%s error=%v", jobID, repo, err)
			return fmt.Errorf("%w: GitHub returned %d", errWorkflowJobNotFound, statusCode)
		}
		return fmt.Errorf("read workflow job error response: %w", err)
	}
	if notFound {
		return fmt.Errorf("%w: GitHub returned %d: %s", errWorkflowJobNotFound, statusCode, string(responseBody))
	}
	return fmt.Errorf("GitHub returned %d: %s", statusCode, string(responseBody))
}

func (c githubAPI) generateJITConfig(ctx context.Context, owner, repo, runnerName string, labels []string, credentials installationCredentials) (string, error) {
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
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/actions/runners/generate-jitconfig", c.baseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("WARN github_response_close_failed operation=generate_jit_config repo=%s error=%v", owner+"/"+repo, err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("read generate JIT config error response: %w", readErr)
		}
		return "", fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.EncodedJITConfig == "" {
		return "", fmt.Errorf("generate JIT config response missing encoded_jit_config")
	}
	return result.EncodedJITConfig, nil
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
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("read installation token error response: %w", readErr)
		}
		return "", fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
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
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return 0, fmt.Errorf("read installation ID error response: %w", readErr)
		}
		return 0, fmt.Errorf("GitHub returned %d: %s", resp.StatusCode, string(body))
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
