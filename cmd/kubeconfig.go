package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// kubeconfig is a minimal representation of a kubectl config file sufficient
// for merging cluster, user, and context entries. Fields not listed here are
// preserved via the yaml package's node-based round-trip (see mergeKubeconfig).
type kubeconfig struct {
	APIVersion     string       `yaml:"apiVersion"`
	Kind           string       `yaml:"kind"`
	Clusters       []kcCluster  `yaml:"clusters"`
	Users          []kcUser     `yaml:"users"`
	Contexts       []kcContext  `yaml:"contexts"`
	CurrentContext string       `yaml:"current-context"`
	Preferences    yaml.Node    `yaml:"preferences,omitempty"`
}

type kcCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		CertificateAuthorityData string `yaml:"certificate-authority-data,omitempty"`
		Server                   string `yaml:"server"`
	} `yaml:"cluster"`
}

type kcUser struct {
	Name string `yaml:"name"`
	User struct {
		ClientCertificateData string `yaml:"client-certificate-data,omitempty"`
		ClientKeyData         string `yaml:"client-key-data,omitempty"`
	} `yaml:"user"`
}

type kcContext struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster   string `yaml:"cluster"`
		User      string `yaml:"user"`
		Namespace string `yaml:"namespace,omitempty"`
	} `yaml:"context"`
}

// mergeKubeconfig reads the kubeconfig at src (e.g. ~/.kube/local.yaml written
// by the provider), renames all entries named "default" to contextName, and
// upserts the cluster, user, and context entries into dst (~/.kube/config).
// It also sets current-context = contextName in dst.
//
// dst is created with mode 0600 if it does not exist. src must exist.
// Callers should treat a non-nil error as advisory: the cluster is up, only
// the convenience merge failed.
func mergeKubeconfig(dst, src, contextName string) error {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("kubeconfig: read %s: %w", src, err)
	}
	var srcKC kubeconfig
	if err := yaml.Unmarshal(srcData, &srcKC); err != nil {
		return fmt.Errorf("kubeconfig: parse %s: %w", src, err)
	}

	// Rename the "default" entries k3s always writes to contextName so they
	// do not collide with other clusters' "default" entries in dst.
	for i := range srcKC.Clusters {
		if srcKC.Clusters[i].Name == "default" {
			srcKC.Clusters[i].Name = contextName
		}
	}
	for i := range srcKC.Users {
		if srcKC.Users[i].Name == "default" {
			srcKC.Users[i].Name = contextName
		}
	}
	for i := range srcKC.Contexts {
		if srcKC.Contexts[i].Name == "default" {
			srcKC.Contexts[i].Name = contextName
		}
		if srcKC.Contexts[i].Context.Cluster == "default" {
			srcKC.Contexts[i].Context.Cluster = contextName
		}
		if srcKC.Contexts[i].Context.User == "default" {
			srcKC.Contexts[i].Context.User = contextName
		}
	}

	// Load or initialise the destination kubeconfig.
	var dstKC kubeconfig
	dstData, err := os.ReadFile(dst)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(dstData, &dstKC); err != nil {
			return fmt.Errorf("kubeconfig: parse %s: %w", dst, err)
		}
	case os.IsNotExist(err):
		dstKC = kubeconfig{APIVersion: "v1", Kind: "Config"}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("kubeconfig: mkdir %s: %w", filepath.Dir(dst), err)
		}
	default:
		return fmt.Errorf("kubeconfig: read %s: %w", dst, err)
	}

	// Upsert cluster.
	for _, sc := range srcKC.Clusters {
		replaced := false
		for i, dc := range dstKC.Clusters {
			if dc.Name == sc.Name {
				dstKC.Clusters[i] = sc
				replaced = true
				break
			}
		}
		if !replaced {
			dstKC.Clusters = append(dstKC.Clusters, sc)
		}
	}

	// Upsert user.
	for _, su := range srcKC.Users {
		replaced := false
		for i, du := range dstKC.Users {
			if du.Name == su.Name {
				dstKC.Users[i] = su
				replaced = true
				break
			}
		}
		if !replaced {
			dstKC.Users = append(dstKC.Users, su)
		}
	}

	// Upsert context.
	for _, sc := range srcKC.Contexts {
		replaced := false
		for i, dc := range dstKC.Contexts {
			if dc.Name == sc.Name {
				dstKC.Contexts[i] = sc
				replaced = true
				break
			}
		}
		if !replaced {
			dstKC.Contexts = append(dstKC.Contexts, sc)
		}
	}

	dstKC.CurrentContext = contextName

	out, err := yaml.Marshal(&dstKC)
	if err != nil {
		return fmt.Errorf("kubeconfig: marshal: %w", err)
	}
	if err := os.WriteFile(dst, out, 0o600); err != nil {
		return fmt.Errorf("kubeconfig: write %s: %w", dst, err)
	}
	return nil
}

// defaultKubeconfigPath returns the path kubectl reads by default:
// $KUBECONFIG if set, otherwise ~/.kube/config.
func defaultKubeconfigPath() (string, error) {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("kubeconfig: resolve home: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}
