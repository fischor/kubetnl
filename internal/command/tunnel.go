package command

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/fischor/kubetnl/internal/interruptcontext"
	"github.com/fischor/kubetnl/internal/port"
	"github.com/fischor/kubetnl/internal/portforward"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8sportforward "k8s.io/client-go/tools/portforward"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/klog/v2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/templates"
)

type TunnelOptions struct {
	genericclioptions.IOStreams

	Namespace        string
	EnforceNamespace bool
	Image            string

	// Name of the tunnel. This will also be the name of the pod and service.
	Name string

	RawPortMappings []string

	PortMappings []port.Mapping

	// The port in the running container that SSH connections are accepted
	// on.
	RemoteSSHPort int

	ContinueOnTunnelError bool

	// The port on the localhost that is used to forward SSH connections to
	// the remote container.
	LocalSSHPort int

	RESTConfig *rest.Config
	ClientSet  *kubernetes.Clientset
}

var (
	tunnelLong = templates.LongDesc(`
		Tunnel TCP connections for a service within a cluster to another (external) endpoint.

		A service and pod gets created in the cluster named like the soley positional 
		argument passed to the tunnel command. The pod runs a SSH server. A portforward
		connection to the pod is establish. Using the forwarded port, a SSH connection
		to the pod gets established. That SSH connection is used to tunnel any traffic
		the pod received on the ports passed with the --publish/-p flag.

		To stop the tunnel use CTRL+C once. This will gracefully close all connections
		and cleanup the created resources in the cluster.`)

	tunnelExample = templates.LongDesc(`
		# Tunnel to local port 8080 from myservice.<namespace>.svc.cluster.local:80.
		kubetnl tunnel myservice 8080:80

		# Tunnel to port 10.10.10.10:3333 from myservice.<namespace>.svc.cluster.local:80.
		kubetnl tunnel myservice 10.10.10.10:3333:80

		# Tunnel to local port 8080 from myservice.<namespace>.svc.cluster.local:80 and to local port 9090 from myservice.<namespace>.svc.cluster.local:90.
		kubetnl tunnel myservice 8080:80 9090:90

		# Tunnel to local port 80 from myservice.<namespace>.svc.cluster.local:80 using a different version 0.1.0 of the server image.
		kubetnl tunnel --image docker.io/fischor/kubetnl-server:0.1.0 mytunnel myservice 80:80`)
)

var (
	kubetnlPodContainerName = "main"
)

// NewTunnelCommand creates a ne tunnel command.
//
// The tunnel command works with a graceful shutdown.
func NewTunnelCommand(f cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := &TunnelOptions{
		IOStreams:    streams,
		LocalSSHPort: 7154, // TODO: grab one randomly
		Image:        "docker.io/fischor/kubetnl-server:0.1.0",
	}

	cmd := &cobra.Command{
		Use:     "tunnel SERVICE_NAME [options] TARGET_ADDR:SERVICE_PORT [...[TARGET_ADDR:SERVICE_PORT]]",
		Short:   "Tunnel TCP connections for a service within a cluster to another (external) endpoint",
		Long:    tunnelLong,
		Example: tunnelExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			ctx, graceCh := interruptcontext.WithGrafulInterrupt(cmd.Context())
			cmdutil.CheckErr(o.Run(ctx, graceCh))
		},
	}

	cmd.Flags().StringVar(&o.Image, "image", o.Image, "The container image thats get deployed to serve a SSH server")

	return cmd
}

func (o *TunnelOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		return cmdutil.UsageErrorf(cmd, "SERVICE_NAME and list of TARGET_ADDR:SERVICE_PORT pairs are required for tunnel")
	}
	o.Name = args[0]
	var err error
	o.PortMappings, err = port.ParseMappings(args[1:])
	if err != nil {
		return err
	}
	o.RemoteSSHPort, err = chooseSSHPort(o.PortMappings)
	if err != nil {
		return err
	}
	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.RESTConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.ClientSet, err = f.KubernetesClientSet()
	if err != nil {
		return err
	}
	return nil
}

func (o *TunnelOptions) Run(ctx context.Context, graceCh <-chan struct{}) error {
	// Create the service for incoming traffic within the cluster. The
	// services accepts traffic on all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the specied protocol.
	serviceClient := o.ClientSet.CoreV1().Services(o.Namespace)
	svcPorts := servicePorts(o.PortMappings)
	service := getService(o.Name, svcPorts)
	klog.V(2).Infof("Creating service \"%s\"...", o.Name)
	service, err := serviceClient.Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service: %v", err)
	}
	klog.V(3).Infof("Created service \"%s\".", service.GetObjectMeta().GetName())
	defer interruptcontext.DoGraceful(ctx, func() {
		klog.V(2).Infof("Deleting service %s ...", service.Name)
		deletePolicy := metav1.DeletePropagationForeground
		deleteOptions := metav1.DeleteOptions{PropagationPolicy: &deletePolicy}
		err := serviceClient.Delete(ctx, service.Name, deleteOptions)
		if err != nil {
			klog.Warningf("error deleting service: %v", err)
		}
	})

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Create the service for incoming traffic within the cluster. The pod
	// exposes all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the specied protocol.
	// Additionally it exposes the port for the ssh conn.
	ports := append(containerPorts(o.PortMappings), corev1.ContainerPort{
		Name:          "ssh",
		ContainerPort: int32(o.RemoteSSHPort),
	})
	podClient := o.ClientSet.CoreV1().Pods(o.Namespace)
	pod := getPod(o.Name, o.Image, o.RemoteSSHPort, ports)
	klog.V(2).Infof("Creating pod \"%s\"...", o.Name)
	pod, err = podClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating pod: %v", err)
	}
	klog.V(3).Infof("Created pod \"%s\".", service.GetObjectMeta().GetName())
	defer interruptcontext.DoGraceful(ctx, func() {
		klog.V(2).Infof("Deleting pod %s ...", pod.Name)
		deletePolicy := metav1.DeletePropagationForeground
		deleteOptions := metav1.DeleteOptions{PropagationPolicy: &deletePolicy}
		err := podClient.Delete(ctx, pod.Name, deleteOptions)
		if err != nil {
			klog.Warningf("error deleting pod: %v. That pod probably still runs. You can use kubetnl cleanup to clean up all resources created by kubetnl.", err)
		}
	})

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Wait for the pod to be ready before setting up a SSH connection.
	watchOptions := metav1.ListOptions{}
	watchOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", o.Name).String()
	watchOptions.ResourceVersion = pod.GetResourceVersion()
	podWatch, err := podClient.Watch(ctx, watchOptions)
	if err != nil {
		return fmt.Errorf("error watching pod %s: %v", o.Name, err)
	}
	// TODO In case of graceful interrupt, wcancel() and return cmd.ErrInterrupted
	// if err == wctx.Err (== context.Cancelled).
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
	klog.V(2).Infof("Pod ready..\n")

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	pfwdReadyCh := make(chan struct{}) // Closed when portforwarding ready.
	pfwdStopCh := make(chan struct{}, 1)
	pfwdDoneCh := make(chan struct{}) // Closed when portforwarding exits.
	go func() error {
		// Do a portforwarding to the pods exposed SSH port.
		req := o.ClientSet.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("portforward")
		transport, upgrader, err := spdy.RoundTripperFor(o.RESTConfig)
		if err != nil {
			return err
		}
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())
		pfwdPorts := []string{fmt.Sprintf("%d:%d", o.LocalSSHPort, o.RemoteSSHPort)}
		var bout, berr bytes.Buffer
		pfwdOut := bufio.NewWriter(&bout)
		pfwdErr := bufio.NewWriter(&berr)
		pfwd, err := k8sportforward.New(dialer, pfwdPorts, pfwdStopCh, pfwdReadyCh, pfwdOut, pfwdErr)
		if err != nil {
			return err
		}
		err = pfwd.ForwardPorts() // blocks
		if err != nil {
			return fmt.Errorf("error port-forwarding from :%d --> %d: %v", o.LocalSSHPort, o.RemoteSSHPort, err)
		}
		// If this errors, also everything following will error.
		close(pfwdDoneCh)
		return nil
	}()
	defer interruptcontext.DoGraceful(ctx, func() {
		close(pfwdStopCh)
		<-pfwdDoneCh
		klog.V(2).Infof("port-forwarding closed")
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-graceCh:
		return interruptcontext.Interrupted
	case <-pfwdReadyCh:
		klog.V(2).Infof("port-forwarding from :%d --> %d", o.LocalSSHPort, o.RemoteSSHPort)
	}

	// TODO: need to do retries here. It seems the pod does not accept SSH
	// connections also its ready.
	<-time.After(2 * time.Second)

	// Establish SSH connection over the forwarded port.
	sshConfig := &ssh.ClientConfig{
		User: "user",
		Auth: []ssh.AuthMethod{
			ssh.Password("password"),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			// Accept all keys.
			return nil
		},
	}
	sshAddr := fmt.Sprintf("localhost:%d", o.LocalSSHPort)
	sshClient, err := ssh.Dial("tcp", sshAddr, sshConfig)
	if err != nil {
		klog.Errorf("error dialing ssh (%s): %v", sshAddr, err)
		return fmt.Errorf("error dialing ssh: %v", err)
	}
	defer interruptcontext.DoGraceful(ctx, func() {
		sshClient.Close()
		klog.V(2).Info("ssl connection (%s) closed", sshAddr)
	})
	klog.V(2).Infof("ssl connection (%s) ready", sshAddr)

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Setup tunnels.
	var pairs []forwarderWithListener
	for _, m := range o.PortMappings {
		// TODO: Check for interrupt and ctx.Done in every iteration.
		// TODO Support remote ips: Note that it does not work without the 0.0.0.0 here.
		target := m.TargetAddress()
		remote := fmt.Sprintf("0.0.0.0:%d", m.ContainerPortNumber)
		l, err := sshClient.Listen("tcp", remote)
		if err != nil {
			if !o.ContinueOnTunnelError {
				// Close all created listeners.
				for _, p := range pairs {
					p.l.Close()
				}
				fmt.Fprintf(o.Out, "Failed to tunnel from %s.%s.svc.cluster.local:%d --> %s\n", o.Name, o.Namespace, m.ContainerPortNumber, target)
				return fmt.Errorf("failed to listen on remote %s: %v", remote, err)
			}
			klog.Errorf("failed to listen on remote %s: %v. No tunnel created.", remote, err)
		}
		pairs = append(pairs, forwarderWithListener{
			f: &portforward.Forwarder{TargetAddr: target},
			l: l,
		})
		fmt.Fprintf(o.Out, "Tunneling from %s.%s.svc.cluster.local:%d --> %s\n", o.Name, o.Namespace, m.ContainerPortNumber, target)
	}

	// Open tunnels.
	tErrg, tctx := errgroup.WithContext(ctx)
	for _, pp := range pairs {
		p := pp
		tErrg.Go(func() error { return p.f.Open(p.l) })
	}
	go func() {
		select {
		case <-tctx.Done():
			// If tctx is done and tctx.Err is non-nil an error
			// occured. Close the other tunnels if requested.
			// Note that if ctx is done and and tctx.Err is nil,
			// the Errgroup and thus the tunnels already exited.
			if tctx.Err() != nil && !o.ContinueOnTunnelError {
				for _, p := range pairs {
					p.f.Close()
				}
			}
		case <-graceCh:
			for _, p := range pairs {
				p.f.Close()
			}
		}
	}()
	_ = tErrg.Wait()

	// Note that, in case of a graceful shutdown the defer functions will
	// close the SSH connection, close the portforwarding and cleanup the
	// pod and services.
	return nil
}

type forwarderWithListener struct {
	f *portforward.Forwarder
	l net.Listener
}

func getPod(name, image string, sshPort int, ports []corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.kubetnl": name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  kubetnlPodContainerName,
				Image: image,
				Ports: ports,
				Env: []corev1.EnvVar{
					{Name: "PORT", Value: strconv.Itoa(sshPort)},
					{Name: "PASSWORD_ACCESS", Value: "true"},
					{Name: "USER_NAME", Value: "user"},
					{Name: "USER_PASSWORD", Value: "password"},
				},
			}},
		},
	}
}

func getService(name string, ports []corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.kubetnl": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"io.github.kubetnl": name,
			},
			Ports: ports,
		},
	}

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

// chooseSSHPort chooses the port number for the SSH server respecting the ports
// that are used for incoming traffic.
func chooseSSHPort(mm []port.Mapping) (int, error) {
	if !isInUse(mm, 2222) {
		return 2222, nil
	}
	// TODO: for 22 portforwarding somewhat never works.
	if !isInUse(mm, 22) {
		return 22, nil
	}
	min := 49152
	max := 65535
	for i := min; i <= max; i++ {
		if !isInUse(mm, i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("Failed to choose a port for the SSH connection - all ports in use")
}

func isInUse(mm []port.Mapping, containerPort int) bool {
	for _, m := range mm {
		if m.ContainerPortNumber == containerPort {
			return true
		}
	}
	return false
}
