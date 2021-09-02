package command

import (
	"flag"
	"io"

	"github.com/fischor/kubetnl/internal/command/cleanup"
	"github.com/fischor/kubetnl/internal/command/options"
	"github.com/fischor/kubetnl/internal/command/tunnel"
	"github.com/fischor/kubetnl/internal/command/version"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	kubetnlLong = templates.LongDesc("")
)

func NewKubetnlCommand(in io.Reader, out, err io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubetnl",
		Short: "Tunnel traffic received on pod to your local machine",
		Long:  kubetnlLong,
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
				tunnel.NewTunnelCommand(f, streams),
				cleanup.NewCleanupCommand(f, streams),
			},
		},
	}
	groups.Add(cmd)

	templates.ActsAsRootCommand(cmd, []string{}, groups...)

	// Add subcommands not within any group.
	cmd.AddCommand(version.NewVersionCommand(streams))
	cmd.AddCommand(options.NewOptionsCommand(streams.Out))

	return cmd
}
