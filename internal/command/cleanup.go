package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	cmdwait "k8s.io/kubectl/pkg/cmd/wait"
	"k8s.io/kubectl/pkg/util/templates"
)

type CleanupOptions struct {
	genericclioptions.IOStreams

	Namespace        string
	EnforceNamespace bool
	AllNamespaces    bool
	ForceDeletion    bool
	GracePeriod      int
	WaitForDeletion  bool
	Quiet            bool

	Result *resource.Result

	DynamicClient dynamic.Interface
}

var (
	cleanupLong = templates.LongDesc(`
		Cleanup all errneously leftover resources created by dew.

		Use cleanup in case there are resources left for previous runs.
		Note that if there are active tunnels, this will destroy these.`)

	cleanupExamples = templates.LongDesc(`
		# Cleanup all resources in the current namespace.
		dew cleanup

		# Cleanup all resources in the "hello" namespace.
		dew cleanup -n hello

		# Cleanup all resources in all namespaces.
		dew cleanup --all-namespaces`)
)

func NewCleanupCommand(f cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := &CleanupOptions{
		IOStreams:   streams,
		GracePeriod: -1,
	}

	cmd := &cobra.Command{
		Use:     "cleanup [options]",
		Short:   "Cleanup all errneously leftover resources created by dew",
		Long:    cleanupLong,
		Example: cleanupExamples,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f))
			cmdutil.CheckErr(o.Run(cmd.Context()))
		},
	}

	cmd.Flags().BoolVarP(&o.AllNamespaces, "all-namespaces", "A", false, "If present, list the requested object(s) across all namespaces. Namespace in current context is ignored even if specified with --namespace.")
	cmd.Flags().BoolVar(&o.ForceDeletion, "force", false, "If true, immediately remove resources from API and bypass graceful deletion. Note that immediate deletion of some resources may result in inconsistency or data loss and requires confirmation.")
	cmd.Flags().IntVar(&o.GracePeriod, "grace-period", 0, "Period of time in seconds given to the resource to terminate gracefully. Ignored if negative. Set to 1 for immediate shutdown. Can only be set to 0 when --force is true (force deletion).")
	cmd.Flags().BoolVar(&o.WaitForDeletion, "wait", true, "If true, wait for resources to be gone before returning. This waits for finalizers.")
	// TODO quiet flag

	return cmd
}

func (o *CleanupOptions) Validate() error {
	switch {
	case o.GracePeriod == 0 && o.ForceDeletion:
		fmt.Fprintf(o.ErrOut, "warning: Immediate deletion does not wait for confirmation that the running resource has been terminated. The resource may continue to run on the cluster indefinitely.\n")
	case o.GracePeriod > 0 && o.ForceDeletion:
		return fmt.Errorf("--force and --grace-period greater than 0 cannot be specified together")
	}
	return nil
}

func (o *CleanupOptions) Complete(f cmdutil.Factory) (err error) {
	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	req, _ := labels.NewRequirement("io.github.dew", selection.Exists, []string{})
	selector := labels.NewSelector().Add(*req)

	o.Result = f.NewBuilder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace().
		LabelSelector(selector.String()).
		AllNamespaces(false).
		ResourceTypeOrNameArgs(true, "pod,service,configmap").RequireObject(false).
		Flatten().
		Do()
	err = o.Result.Err()
	if err != nil {
		return err
	}

	o.DynamicClient, err = f.DynamicClient()
	if err != nil {
		return err
	}

	return nil
}
func (o *CleanupOptions) Run(ctx context.Context) error {
	deletedInfos := []*resource.Info{}
	uidMap := cmdwait.UIDMap{}
	err := o.Result.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			// If there was a problem walking the list of resources.
			return err
		}
		deletedInfos = append(deletedInfos, info)
		options := &metav1.DeleteOptions{}
		if o.GracePeriod >= 0 {
			options = metav1.NewDeleteOptions(int64(o.GracePeriod))
		}
		// Delete resource.
		response, err := resource.
			NewHelper(info.Client, info.Mapping).
			DeleteWithOptions(info.Namespace, info.Name, options)
		if err != nil {
			// TODO: returning the error for now, but we should try
			// the other ones and collect the error
			return err
		}
		if !o.Quiet {
			o.PrintObj(info)
		}
		resourceLocation := cmdwait.ResourceLocation{
			GroupResource: info.Mapping.Resource.GroupResource(),
			Namespace:     info.Namespace,
			Name:          info.Name,
		}
		if status, ok := response.(*metav1.Status); ok && status.Details != nil {
			uidMap[resourceLocation] = status.Details.UID
			return nil
		}
		responseMetadata, err := meta.Accessor(response)
		if err != nil {
			// we don't have UID, but we didn't fail the delete, next best thing is just skipping the UID
			klog.V(1).Info(err)
			return nil
		}
		uidMap[resourceLocation] = responseMetadata.GetUID()
		return nil
	})
	if err != nil {
		return err
	}
	if len(deletedInfos) == 0 {
		fmt.Fprintf(o.Out, "No resources found\n")
		return nil
	}
	if !o.WaitForDeletion {
		return nil
	}
	waitOptions := cmdwait.WaitOptions{
		ResourceFinder: genericclioptions.ResourceFinderForResult(resource.InfoListVisitor(deletedInfos)),
		UIDMap:         uidMap,
		DynamicClient:  o.DynamicClient,
		Timeout:        time.Minute,

		Printer:     printers.NewDiscardingPrinter(),
		ConditionFn: cmdwait.IsDeleted,
		IOStreams:   o.IOStreams,
	}
	err = waitOptions.RunWait()
	if errors.IsForbidden(err) || errors.IsMethodNotSupported(err) {
		// if we're forbidden from waiting, we shouldn't fail.
		// if the resource doesn't support a verb we need, we shouldn't fail.
		klog.V(1).Info(err)
		return nil
	}
	return err
}

func (o *CleanupOptions) PrintObj(info *resource.Info) {
	groupKind := info.Mapping.GroupVersionKind
	kindString := fmt.Sprintf("%s.%s", strings.ToLower(groupKind.Kind), groupKind.Group)
	if len(groupKind.Group) == 0 {
		kindString = strings.ToLower(groupKind.Kind)
	}
	operation := "deleted"
	if o.GracePeriod == 0 {
		operation = "force deleted"
	}
	fmt.Fprintf(o.Out, "%s \"%s\" %s\n", kindString, info.Name, operation)
}
