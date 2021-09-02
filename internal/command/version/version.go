package version

import (
	"fmt"

	"github.com/fischor/kubetnl/internal/version"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/util/templates"
)

type VersionOptions struct {
	genericclioptions.IOStreams
	Short bool
}

var (
	versionExample = templates.Examples(`
                # Print the version number and git commit SHA.
                kubetnl version

		# Print the version number only and omit the git commit SHA.
                kubetnl version --short`)
)

func NewVersionCommand(streams genericclioptions.IOStreams) *cobra.Command {
	o := &VersionOptions{
		IOStreams: streams,
	}

	cmd := &cobra.Command{
		Use:     "version [--short]",
		Short:   "Print the kubetnl version",
		Example: versionExample,
		Run: func(cmd *cobra.Command, args []string) {
			o.Run()
		},
	}

	cmd.Flags().BoolVar(&o.Short, "short", o.Short, "If true, just prints the version number and omits git commit SHA.")

	return cmd
}

func (o *VersionOptions) Run() {
	i := version.Get()
	if o.Short {
		fmt.Fprintf(o.Out, "%s\n", i.Version)
	} else {
		fmt.Fprintf(o.Out, "%s at %s\n", i.Version, i.GitCommit)
	}
}
