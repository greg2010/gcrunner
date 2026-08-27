package function

import (
	"context"
	"sync"
)

type retryingClientInitializer[T any] struct {
	newClient func(context.Context) (T, error)
	mu        sync.Mutex
	client    T
	ready     bool
}

func newRetryingClientInitializer[T any](newClient func(context.Context) (T, error)) *retryingClientInitializer[T] {
	return &retryingClientInitializer[T]{newClient: newClient}
}

func (i *retryingClientInitializer[T]) get(ctx context.Context, initialized func(T)) (T, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.ready {
		return i.client, nil
	}

	client, err := i.newClient(ctx)
	if err != nil {
		var zero T
		return zero, err
	}

	initialized(client)
	i.client = client
	i.ready = true
	return i.client, nil
}
