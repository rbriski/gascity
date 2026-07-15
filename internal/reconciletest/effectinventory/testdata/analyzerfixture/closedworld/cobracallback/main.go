// Package main provides a closed-executable Cobra callback fixture.
package main

import "github.com/spf13/cobra"

func Effect() {}

func namedHandler(*cobra.Command, []string) error {
	Effect()
	return nil
}

func main() {
	root := &cobra.Command{
		Use: "root",
		RunE: func(*cobra.Command, []string) error {
			Effect()
			return nil
		},
	}
	root.AddCommand(&cobra.Command{Use: "named", RunE: namedHandler})
	root.SetArgs([]string{"named"})
	_ = root.Execute()
}
