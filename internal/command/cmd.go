package command

import (
	"flag"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"
)

type RootOptions struct{}

var (
	rootLong = templates.LongDesc("")
)

// TODO: a global signal for graceful shutdown thats triggered on the first
// CTRL+C would be nice.
// context cancel shut be for KILL, since that also works for operations that
// we do not control (API calls etc).

func NewRootCommand(in io.Reader, out, err io.Writer) *cobra.Command {
	_ = &RootOptions{}

	cmd := &cobra.Command{
		Use:   "kubetnl",
		Short: "Tunnel traffic received on pod to your local machine",
		Long:  rootLong,
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}

	// Adds the following flags:
	//
	// 	"kubeconfig" "cluster" "user" "context" "namespace" "server"
	// 	"tls-server-name" "insecure-skip-tls-verify"
	// 	"client-certificate" "client-key" "certificate-authority"
	// 	"token" "as" "as-group" "username" "password" "request-timeout"
	// 	"cache-dir"
	//
	// These flags are used by the cmdutil.Factory.
	kubeConfigFlags := genericclioptions.NewConfigFlags(true)
	kubeConfigFlags.AddFlags(cmd.PersistentFlags())

	// Why?
	cmd.PersistentFlags().AddGoFlagSet(flag.CommandLine)

	f := cmdutil.NewFactory(kubeConfigFlags)
	streams := genericclioptions.IOStreams{In: in, Out: out, ErrOut: err}

	// Wrapping the command within groups and using the
	// templates.ActsAsRootCommand function will cmd to have a similiar
	// look and feel like kubectl: Examples will be rendered correctly,
	// global options not shown on every subcommand etc.
	groups := templates.CommandGroups{
		{
			Message: "Basic commands",
			Commands: []*cobra.Command{
				NewTunnelCommand(f, streams),
				NewCleanupCommand(f, streams),
			},
		},
	}
	groups.Add(cmd)

	templates.ActsAsRootCommand(cmd, []string{}, groups...)

	// Add subcommands not within any group.
	cmd.AddCommand(NewVersionCommand(streams))
	cmd.AddCommand(NewOptionsCommand(streams.Out))

	return cmd
}
