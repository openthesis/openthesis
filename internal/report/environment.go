package report

import "github.com/openthesis/openthesis/internal/testconfig"

// EnvironmentInfo describes the containers/nodes in the test environment.
type EnvironmentInfo struct {
	Containers []ContainerInfo `json:"containers"`
}

// ContainerInfo describes a single container or node in the environment.
type ContainerInfo struct {
	Name  string `json:"name"`
	Image string `json:"image,omitempty"` // image reference if from compose/docker
	Tag   string `json:"tag,omitempty"`
}

// BuildEnvironment creates an EnvironmentInfo from the test config.
func BuildEnvironment(cfg *testconfig.Config) EnvironmentInfo {
	var containers []ContainerInfo
	for _, n := range cfg.Nodes {
		containers = append(containers, ContainerInfo{
			Name:  n.Name,
			Image: n.Binary,
		})
	}
	return EnvironmentInfo{Containers: containers}
}
