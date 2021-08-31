package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"path/filepath"

	"github.com/fischor/dew/internal/port"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/homedir"
)

type ClientOptions struct {
	Service         string
	RawPortMappings []string

	PortMappings []port.Mapping
	RESTConfig   *rest.Config
	ClientSet    *kubernetes.Clientset
}

var (
	dewImage         = "cr.mycom.com:5000/dew-server"
	dewContainer     = "main"
	dewServerCommand = "dew-server"

	clientLong    = ""
	clientExample = ""
)

// dew <service/pod-name> -p 8080:8080 -p 7070:7070
//
// If there is already a service with that name and a dew annotation,
// we can still try to connect to it.

func main() {
	cmd := NewClientCommand()
	if err := cmd.Execute(); err != nil {
		log.Fatalf("error: %v", err)
	}
}

func NewClientCommand() *cobra.Command {
	o := NewClientOptions()

	cmd := &cobra.Command{
		Use:   "dew",
		Short: "Tunnel traffic received on pod to your local machine",
		Long:  clientLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Setup stop channel. Notifies the server when to stop
			// serving. Is closed when a os.Interrupt is received.
			stopCh := make(chan struct{})
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			go func() {
				<-sig
				close(stopCh)
			}()

			err := o.Complete(args)
			if err != nil {
				return err
			}
			return Run(o, stopCh)
		},
	}

	cmd.Flags().StringSliceVarP(&o.RawPortMappings, "publish", "p", nil, "The TCP ports to pipe")

	return cmd
}
func NewClientOptions() *ClientOptions {
	return &ClientOptions{}
}

func (o *ClientOptions) Complete(args []string) error {
	// TODO: collect multiple errors
	if len(args) != 1 {
		return fmt.Errorf("requires exactly one agument, got %d", len(args))
	}
	o.Service = args[0]

	if len(o.RawPortMappings) == 0 {
		return fmt.Errorf("specify one or more ports to tunnel. Use --tcp 8080:8080 or --udp 8080:9090 for example.")
	}
	var err error
	o.PortMappings, err = port.ParseMappings(o.RawPortMappings)
	if err != nil {
		return err
	}

	// Set up Kubernetes Client set.
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	// flag.Parse()
	o.RESTConfig, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	o.ClientSet, err = kubernetes.NewForConfig(o.RESTConfig)
	if err != nil {
		return err
	}
	return err
}

// dew -p <local>:<container>/<protocol> <service>

// TODO: test if the BuildConfigFromFlags actually picks up and -n --namespace
// arguments.

func Run(completedOptions *ClientOptions, stopCh <-chan struct{}) error {
	ctx := context.Background()

	// dewID is attached to the dew pod as a "dew-id" keyed label.
	dewID := "dew-" + uuid.New().String()

	// Create the service.
	serviceClient := completedOptions.ClientSet.CoreV1().Services(corev1.NamespaceDefault)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: completedOptions.Service,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"dew-id": dewID,
			},
			Ports: servicePorts(completedOptions.PortMappings),
		},
	}
	log.Printf("Creating service \"%s\"...\n", completedOptions.Service)
	service, err := serviceClient.Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service: %v", err)
	}
	log.Printf("Created service \"%s\".", service.GetObjectMeta().GetName())
	defer func() {
		log.Printf("Deleting service %s ...", service.Name)
		serviceClient.Delete(ctx, service.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting service: %v", err)
		}
	}()

	// Create the pod.
	podClient := completedOptions.ClientSet.CoreV1().Pods(corev1.NamespaceDefault)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: completedOptions.Service,
			Labels: map[string]string{
				"dew-id": dewID,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  dewContainer,
				Image: dewImage,
				Ports: containerPorts(completedOptions.PortMappings),
			}},
		},
	}
	log.Printf("Creating pod \"%s\"...\n", completedOptions.Service)
	pod, err = podClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating pod: %v", err)
	}
	log.Printf("Created pod \"%s\".", service.GetObjectMeta().GetName())
	defer func() {
		log.Printf("Deleting pod %s ...", pod.Name)
		err := podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting pod: %v", err)
		}
	}()

	// Wait for the pod to be ready.
	// kubectl wait for --condition=ready
	watchOptions := metav1.ListOptions{}
	watchOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", completedOptions.Service).String()
	watchOptions.ResourceVersion = pod.GetResourceVersion()
	podWatch, err := podClient.Watch(ctx, watchOptions)
	if err != nil {
		return fmt.Errorf("error watching pod: %v", err)
	}

	// TODO: sleep here to assure the pod is ready. Then test if UntilWithoutRetry
	// will also receive events from before it was called.
	wctx, wcancel := watchtools.ContextWithOptionalTimeout(context.Background(), 5*time.Minute)
	_, err = watchtools.UntilWithoutRetry(wctx, podWatch, condPodReady)
	wcancel()
	if err != nil {
		if err == watchtools.ErrWatchClosed {
			return fmt.Errorf("error waiting for pod ready: podWatch has been closed before pod ready event received")
		}
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("error waiting for pod ready: timed out after %d seconds", 300)
		}
		return fmt.Errorf("error waiting for pod ready: received unknown error \"%f\"", err)
	}
	log.Printf("Pod ready..\n")

	// The pod is now ready. Execute the command.

	// TODO: per exec only one port mapping!
	var errg errgroup.Group
	for _, mm := range completedOptions.PortMappings {
		m := mm
		errg.Go(func() error {
			err := execute(completedOptions, pod, m)
			if err != nil {
				panic(err)
			}
			return err
		})
	}

	errCh := make(chan error)
	go func() {
		errCh <- errg.Wait()
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-stopCh:
		// TODO: graceful stop conns
		return nil
	}
}

func execute(completedOptions *ClientOptions, pod *corev1.Pod, m port.Mapping) error {
	cmd := []string{dewServerCommand, m.ContainerPort().String()}
	req := completedOptions.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.GetName()).
		Namespace(pod.GetNamespace()).
		SubResource("exec").
		// Param("container", "dew").
		VersionedParams(&corev1.PodExecOptions{
			Container: dewContainer,
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	log.Printf("Executing %s..\n", strings.Join(cmd, " "))
	exec, err := remotecommand.NewSPDYExecutor(completedOptions.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("error NewSPDYExecutor: %v", err)
	}

	// Unfortunately, the Executor is not yet terminateable:
	// https://github.com/kubernetes/kubernetes/pull/103177

	// Solution: write to the program to exit:
	// https://github.com/kubernetes/client-go/issues/884#issuecomment-821098839

	// Setup target connection.
	conn, err := net.Dial(m.Protocol.String(), m.TargetAddress())
	if err != nil {
		return fmt.Errorf("failed to dial target address %s: %v", m.TargetAddress(), err)
	}
	log.Printf("%s connection to %s established.\n", m.Protocol.String(), m.TargetAddress())

	stderr := new(bytes.Buffer)
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  conn,
		Stdout: conn,
		Stderr: stderr,
		Tty:    false,
	})
	if err != nil {
		return fmt.Errorf("error exec.Stream: %v", err)
	}
	return nil
}

func servicePorts(mappings []port.Mapping) []corev1.ServicePort {
	var ports []corev1.ServicePort
	for i, m := range mappings {
		ports = append(ports, corev1.ServicePort{
			Name:       fmt.Sprint(i),
			Port:       int32(m.ContainerPortNumber),
			TargetPort: intstr.FromInt(m.ContainerPortNumber),
			Protocol:   protocolToCoreV1(m.Protocol),
		})
	}
	return ports
}

func containerPorts(mappings []port.Mapping) []corev1.ContainerPort {
	var ports []corev1.ContainerPort
	for _, m := range mappings {
		ports = append(ports, corev1.ContainerPort{
			ContainerPort: int32(m.ContainerPortNumber),
			Protocol:      protocolToCoreV1(m.Protocol),
			// TODO: HostIP?
		})
	}
	return ports
}

func protocolToCoreV1(p port.Protocol) corev1.Protocol {
	if p == port.ProtocolSCTP {
		return corev1.ProtocolSCTP
	}
	if p == port.ProtocolUDP {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// waitService is a watchtools.ConditionFunc. Waits for the service to have one
// pod attached.
func condPodReady(event watch.Event) (bool, error) {
	pod := event.Object.(*corev1.Pod)
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}
