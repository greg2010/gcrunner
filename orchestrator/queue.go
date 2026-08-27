package function

import (
	"context"
	"fmt"
	"os"
	"strings"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
)

type cloudTasksClientFactory interface {
	NewClient(ctx context.Context) (*cloudtasks.Client, error)
}

type cloudTasksClientFactoryImpl struct{}

func (cloudTasksClientFactoryImpl) NewClient(ctx context.Context) (*cloudtasks.Client, error) {
	return cloudtasks.NewClient(ctx)
}

type cloudTasksClientInitializer struct {
	client *retryingClientInitializer[*cloudtasks.Client]
}

func newCloudTasksClientInitializer(factory cloudTasksClientFactory) *cloudTasksClientInitializer {
	return &cloudTasksClientInitializer{client: newRetryingClientInitializer(factory.NewClient)}
}

func (i *cloudTasksClientInitializer) get(ctx context.Context) (*cloudtasks.Client, error) {
	client, err := i.client.get(ctx, func(*cloudtasks.Client) {})
	if err != nil {
		return nil, fmt.Errorf("create cloud tasks client: %w", err)
	}
	return client, nil
}

var cloudTasksClient = newCloudTasksClientInitializer(cloudTasksClientFactoryImpl{})

func getCloudTasksClient(ctx context.Context) (*cloudtasks.Client, error) {
	return cloudTasksClient.get(ctx)
}

func enqueueTask(ctx context.Context, path string, payload []byte, jobID int64) error {
	client, err := getCloudTasksClient(ctx)
	if err != nil {
		return err
	}

	queuePath := os.Getenv("CLOUD_TASKS_QUEUE")
	if queuePath == "" {
		return fmt.Errorf("CLOUD_TASKS_QUEUE environment variable not set")
	}

	cloudRunURL := os.Getenv("CLOUD_RUN_URL")
	if cloudRunURL == "" {
		return fmt.Errorf("CLOUD_RUN_URL environment variable not set")
	}

	tasksSAEmail := os.Getenv("CLOUD_TASKS_SA_EMAIL")
	if tasksSAEmail == "" {
		return fmt.Errorf("CLOUD_TASKS_SA_EMAIL environment variable not set")
	}

	pathSuffix := path[strings.LastIndex(path, "/")+1:]
	taskName := fmt.Sprintf("%s/tasks/job-%d-%s", queuePath, jobID, pathSuffix)

	req := &taskspb.CreateTaskRequest{
		Parent: queuePath,
		Task: &taskspb.Task{
			Name: taskName,
			MessageType: &taskspb.Task_HttpRequest{
				HttpRequest: &taskspb.HttpRequest{
					Url:        cloudRunURL + path,
					HttpMethod: taskspb.HttpMethod_POST,
					Body:       payload,
					Headers: map[string]string{
						"Content-Type": "application/json",
					},
					AuthorizationHeader: &taskspb.HttpRequest_OidcToken{
						OidcToken: &taskspb.OidcToken{
							ServiceAccountEmail: tasksSAEmail,
							Audience:            cloudRunURL,
						},
					},
				},
			},
		},
	}

	_, err = client.CreateTask(ctx, req)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}

	return nil
}
