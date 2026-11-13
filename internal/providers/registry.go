package providers

import (
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/commandprovider"
	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func Build(cfg config.Config) (map[string]provider.Provider, error) {
	result := make(map[string]provider.Provider, len(cfg.Providers))
	for name, spec := range cfg.Providers {
		instance, err := commandprovider.New(spec.Command, spec.Env, provider.Description{Identity: spec.Identity, Lifecycle: spec.Lifecycle}, time.Duration(cfg.RequestTimeout))
		if err != nil {
			return nil, err
		}
		result[name] = instance
	}
	return result, nil
}
