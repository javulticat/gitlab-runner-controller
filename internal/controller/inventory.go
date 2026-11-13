package controller

import (
	"errors"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func indexInventory(resources []provider.Resource) (map[string]provider.Resource, error) {
	if resources == nil {
		return nil, errors.New("provider omitted its current resource inventory")
	}
	result := make(map[string]provider.Resource, len(resources))
	for _, resource := range resources {
		if resource.ID == "" || (resource.State != provider.Running && resource.State != provider.Stopped && resource.State != provider.Unknown) {
			return nil, errors.New("provider returned invalid resource inventory")
		}
		if _, exists := result[resource.ID]; exists {
			return nil, errors.New("provider returned duplicate resource IDs")
		}
		result[resource.ID] = resource
	}
	return result, nil
}
