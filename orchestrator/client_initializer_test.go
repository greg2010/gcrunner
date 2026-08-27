package function

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type clientInitializerTestClient struct{}

func TestRetryingClientInitializer(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "retries after failed construction",
			run: func(t *testing.T) {
				client := &clientInitializerTestClient{}
				constructions := 0
				initializer := newRetryingClientInitializer(func(context.Context) (*clientInitializerTestClient, error) {
					constructions++
					if constructions == 1 {
						return nil, errors.New("temporary initialization failure")
					}
					return client, nil
				})

				got, err := initializer.get(context.Background(), func(*clientInitializerTestClient) {})
				if err == nil || err.Error() != "temporary initialization failure" {
					t.Errorf("error = %v, want temporary initialization failure", err)
				}
				if got != nil {
					t.Errorf("client = %v, want nil", got)
				}

				got, err = initializer.get(context.Background(), func(*clientInitializerTestClient) {})
				if err != nil {
					t.Fatalf("get after retry: %v", err)
				}
				if got != client {
					t.Errorf("client = %p, want %p", got, client)
				}
				if constructions != 2 {
					t.Errorf("constructions = %d, want 2", constructions)
				}
			},
		},
		{
			name: "caches successful construction for concurrent callers",
			run: func(t *testing.T) {
				const callers = 8

				client := &clientInitializerTestClient{}
				constructions := 0
				initialized := 0
				initializer := newRetryingClientInitializer(func(context.Context) (*clientInitializerTestClient, error) {
					constructions++
					return client, nil
				})
				start := make(chan struct{})
				errs := make(chan error, callers)
				var ready sync.WaitGroup
				var callersDone sync.WaitGroup
				ready.Add(callers)
				callersDone.Add(callers)
				for range callers {
					go func() {
						defer callersDone.Done()
						ready.Done()
						<-start
						got, err := initializer.get(context.Background(), func(*clientInitializerTestClient) {
							initialized++
						})
						if err != nil {
							errs <- err
							return
						}
						if got != client {
							errs <- errors.New("client does not match constructed client")
						}
					}()
				}
				ready.Wait()
				close(start)
				callersDone.Wait()
				close(errs)
				for err := range errs {
					t.Errorf("concurrent initialization: %v", err)
				}
				if constructions != 1 {
					t.Errorf("constructions = %d, want 1", constructions)
				}
				if initialized != 1 {
					t.Errorf("initialized calls = %d, want 1", initialized)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
