package image

type Config struct {
	Default    DefaultOption    `yaml:"default"`
	Kubernetes KubernetesOption `yaml:"kubernetes"`
	Register   Repository       `yaml:"register"`
	Images     []string         `yaml:"images"`
}

type DefaultOption struct {
	PushKubernetes bool `yaml:"push_kubernetes"`
	PushImages     bool `yaml:"push_images"`
}

type KubernetesOption struct {
	Version string `yaml:"version"`
}

type Repository struct {
	Target RepositoryOption `yaml:"target"`
	Source RepositoryOption `yaml:"source"`
}

type RepositoryOption struct {
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Registry  string `yaml:"registry"`
	Namespace string `yaml:"namespace"`
}
