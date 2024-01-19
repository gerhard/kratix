package main

import (
	"os"
)

type Kratix struct {
	// +private
	Golang *Go
}

func New(
	// Golang version to use when building
	// +optional
	golang string,
) *Kratix {
	if golang == "" {
		golang = "1.21.4"
	}

	return &Kratix{
		Golang: dag.Go(GoOpts{
			Version: golang,
		}),
	}
}

// example usage: "dagger call test"
func (m *Kratix) Test() (*Container, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	return m.Golang.
		WithSource(dag.Host().Directory(cwd+"/..", HostDirectoryOpts{
			Exclude: []string{".git", "bin"},
		})).
		Exec([]string{"make", "test"}), nil
}
