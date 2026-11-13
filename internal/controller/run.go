package controller

import (
	"context"
	"errors"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

var ErrStopping = errors.New("controller is stopping")

const (
	reconciliationTimeout = 5 * time.Minute
	operationCleanupGrace = 5 * time.Second
)

type admissionContextKey struct{}

func (c *Controller) admissionStopped(ctx context.Context) bool {
	if c.stopping.Load() {
		return true
	}
	admission, ok := ctx.Value(admissionContextKey{}).(context.Context)
	if ok && admission.Err() != nil {
		c.stopping.Store(true)
		return true
	}
	return false
}

func (c *Controller) ShutdownGrace() time.Duration {
	return time.Duration(c.cfg.RequestTimeout) + 2*operationCleanupGrace
}

func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Duration(c.cfg.PollInterval))
	defer ticker.Stop()
	defer c.stopping.Store(true)
	for {
		if ctx.Err() != nil {
			return c.shutdownResult(nil)
		}
		// Termination stops admission while an active request keeps its deadline.
		workParent := context.WithValue(context.WithoutCancel(ctx), admissionContextKey{}, ctx)
		workCtx, cancel := context.WithTimeout(workParent, reconciliationTimeout+time.Duration(c.cfg.RequestTimeout))
		done := make(chan error, 1)
		go func() { done <- c.Step(workCtx) }()
		var err error
		select {
		case err = <-done:
			cancel()
		case <-ctx.Done():
			c.stopping.Store(true)
			grace := time.NewTimer(time.Duration(c.cfg.RequestTimeout) + operationCleanupGrace)
			select {
			case err = <-done:
				grace.Stop()
			case <-grace.C:
				cancel()
				// Join before the caller can release the state lock.
				err = <-done
			}
			cancel()
			return c.shutdownResult(err)
		}
		if !c.store.Healthy() {
			return c.shutdownResult(err)
		}
		if err != nil && !errors.Is(err, state.ErrStale) && !errors.Is(err, ErrStopping) && ctx.Err() == nil {
			c.log.Error("reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return c.shutdownResult(nil)
		case <-ticker.C:
		case <-c.wake:
		}
	}
}

func (c *Controller) shutdownResult(err error) error {
	if !c.store.Healthy() {
		if errors.Is(err, state.ErrPoisoned) {
			return err
		}
		return errors.Join(state.ErrPoisoned, err)
	}
	return nil
}
